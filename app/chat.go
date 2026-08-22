package main

// The coach chat: the judgment layer over the plan, the log and the
// archive — "calf at 4, two missed doses, what gives this week?" — as a
// conversation rather than a reading. Phase 2 of the rework design
// (decided 20 Aug 2026, built 21 Aug) shipped DISCUSS-ONLY: the model
// reads everything the pages read and changes nothing. Phase 2b (22 Aug)
// lets it PROPOSE: one change per reply, drawn only from what the app's
// own rework flow offers for that date, recorded in the transcript as a
// card the athlete applies or dismisses. Applying goes through
// /api/amend like the rework control — armed twice, by the athlete; the
// model never writes the plan, and until the athlete acts nothing has
// changed, which the procedure makes it say.
//
// Shape, all of it the house's own precedent:
//
//   - One conversation per athlete-local day, kept as one file per day in
//     <data>/chat/<date>.jsonl: appended to, never rewritten, server-only,
//     never shipped by push-data. A conversation matters for the day it
//     is had (Adam, 22 Aug 2026), so the retention is today and yesterday
//     — yesterday only so a reload at 00:01 does not blank the page
//     mid-thought — and older files are deleted whole at startup and at
//     the 00:05 tick, the backup prune's own shape. Transcripts hold the
//     athlete's words and the model's replies — not the tool traffic,
//     which is re-derivable and large.
//   - POST a message, get 202, poll: the regrade pattern. One model turn
//     in flight per day; a second POST while it runs is a 409. No SSE.
//   - The LLM client, dialects and tool loop are the grader's (llm.go);
//     the system prompt is NOT — the grading procedure and its prompt are
//     byte-pinned by the corpus and this never touches them. Chat has its
//     own procedure here plus an optional <data>/coaching-notes.md, read
//     fresh each turn and clipped the way grading-notes.md is.
//   - CHAT_* config falls back to GRADER_* for the provider, base URL, key,
//     model and dialect, so one grader.env serves both; CHAT_MODE is its
//     own switch, off by default. A typo is fatal at startup, like the
//     grader's. A cloud model is the practical requirement: a local 4B
//     turn was measured at 45 s to 6 min, and a person is waiting here.
//   - Every number the model quotes comes from a tool, server-resolved:
//     the day's prescription, the week, session history, measured
//     metrics, the log, the rework candidates, issue adherence, the
//     benchmark timeline. It never retypes the plan.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

/* ── configuration ─────────────────────────────────────────────────────── */

type chatConfig struct {
	Mode     string // off | on
	Provider string
	Dialect  string
	Model    string
	BaseURL  string
	Key      string
	Effort   string
	NotesMax int
	// TurnsPerDay caps what one day's conversation may cost: each user
	// message is a model turn with tools behind it.
	TurnsPerDay int
}

const defaultChatTurns = 40

// chatConfigFromEnv reads CHAT_*, each falling back to its GRADER_*
// counterpart where one exists, so a stack that already grades needs only
// CHAT_MODE=on to talk. A value that is set and wrong is fatal.
func chatConfigFromEnv() (chatConfig, error) {
	fb := func(chatKey, graderKey, def string) string {
		if v := os.Getenv(chatKey); v != "" {
			return v
		}
		return envOr(graderKey, def)
	}
	c := chatConfig{
		Mode:        envOr("CHAT_MODE", "off"),
		Provider:    fb("CHAT_PROVIDER", "GRADER_PROVIDER", "anthropic"),
		Dialect:     fb("CHAT_DIALECT", "GRADER_DIALECT", ""),
		Model:       fb("CHAT_MODEL", "GRADER_MODEL", ""),
		BaseURL:     fb("CHAT_BASE_URL", "GRADER_BASE_URL", ""),
		Key:         fb("CHAT_API_KEY", "GRADER_API_KEY", ""),
		Effort:      fb("CHAT_REASONING_EFFORT", "GRADER_REASONING_EFFORT", ""),
		NotesMax:    defaultNotesMax,
		TurnsPerDay: defaultChatTurns,
	}
	if v := os.Getenv("CHAT_NOTES_MAX"); v != "" {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n <= 0 {
			return c, fmt.Errorf("CHAT_NOTES_MAX is %q, want a positive byte count", v)
		}
		c.NotesMax = n
	}
	if v := os.Getenv("CHAT_TURNS_PER_DAY"); v != "" {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n <= 0 {
			return c, fmt.Errorf("CHAT_TURNS_PER_DAY is %q, want a positive count", v)
		}
		c.TurnsPerDay = n
	}
	switch c.Mode {
	case "off", "on":
	default:
		return c, fmt.Errorf("CHAT_MODE is %q, want off or on", c.Mode)
	}
	if c.Mode == "off" {
		return c, nil
	}
	switch c.Provider {
	case "anthropic":
		if c.Model == "" {
			c.Model = "claude-opus-5"
		}
		if c.BaseURL == "" {
			c.BaseURL = "https://api.anthropic.com"
		}
	case "openai":
		if c.BaseURL == "" {
			c.BaseURL = "https://api.openai.com"
		}
		if c.Model == "" {
			return c, fmt.Errorf("CHAT_PROVIDER=openai needs CHAT_MODEL (or GRADER_MODEL)")
		}
	default:
		return c, fmt.Errorf("CHAT_PROVIDER is %q, want anthropic or openai", c.Provider)
	}
	if c.Dialect == "" {
		c.Dialect = c.Provider
	}
	if c.Dialect != "anthropic" && c.Dialect != "openai" {
		return c, fmt.Errorf("CHAT_DIALECT is %q, want anthropic or openai", c.Dialect)
	}
	return c, nil
}

/* ── the transcript store ──────────────────────────────────────────────── */

// chatLine is one line of a day's file: who said what, on which day's
// conversation. Role is user, assistant, or error — an error is the turn
// that did not happen, kept so the page can say so and so the record is
// honest about what the athlete was told.
type chatLine struct {
	TS   time.Time `json:"ts"`
	Date string    `json:"date"`
	Role string    `json:"role"` // user | assistant | error | proposal | decision
	Text string    `json:"text"`
	// Data is the structured half of a proposal (date, op, arg, title,
	// detail) or a decision (the proposal's id and applied|dismissed).
	Data json.RawMessage `json:"data,omitempty"`
}

// chatProposal is what propose_amendment records: exactly a rework
// candidate, so applying it is exactly what the rework control would do.
type chatProposal struct {
	Date   string `json:"date"`
	Op     string `json:"op"`
	Arg    string `json:"arg,omitempty"`
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// chatDecision is the athlete's answer to a proposal, keyed by the
// proposal line's id (its timestamp).
type chatDecision struct {
	Proposal string `json:"proposal"`
	Status   string `json:"status"` // applied | dismissed
}

// lineID names a transcript line for a decision to point at.
func (l chatLine) lineID() string { return l.TS.UTC().Format(time.RFC3339Nano) }

// chatStore is the transcripts, one append-only file per day under
// <data>/chat, held in memory by day. Same discipline as the entries log
// within a day: appended with fsync, never rewritten, a malformed line
// skipped loudly rather than fatal. Across days it is a ring: a file
// older than the retention is deleted whole, never edited.
type chatStore struct {
	dir string
	mu  sync.RWMutex
	by  map[string][]chatLine
}

// chatKeepDays is the retention: today and yesterday.
const chatKeepDays = 2

var chatFileName = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})\.jsonl$`)

func chatFilePath(dir, date string) string { return filepath.Join(dir, date+".jsonl") }

// openChatStore reads every day file present. A single <data>/chat.jsonl
// from before the per-day files (21 Aug 2026) is read once, its lines
// moved into their days' files, and the old file renamed aside — not
// deleted, because nothing here deletes a record it did not write.
func openChatStore(dataDir string) (*chatStore, error) {
	c := &chatStore{dir: filepath.Join(dataDir, "chat"), by: map[string][]chatLine{}}
	ents, err := os.ReadDir(c.dir)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read chat dir: %w", err)
	}
	for _, e := range ents {
		m := chatFileName.FindStringSubmatch(e.Name())
		if e.IsDir() || m == nil {
			continue
		}
		lines, err := readChatFile(filepath.Join(c.dir, e.Name()))
		if err != nil {
			return nil, err
		}
		c.by[m[1]] = lines
	}
	legacy := filepath.Join(dataDir, "chat.jsonl")
	if lines, err := readChatFile(legacy); err == nil {
		for _, l := range lines {
			if err := c.Append(l); err != nil {
				return nil, fmt.Errorf("moving %s into day files: %w", legacy, err)
			}
		}
		if err := os.Rename(legacy, legacy+".migrated"); err != nil {
			return nil, fmt.Errorf("retiring %s: %w", legacy, err)
		}
		log.Printf("chat: moved %d lines from chat.jsonl into per-day files; the old file is chat.jsonl.migrated", len(lines))
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return c, nil
}

// readChatFile is one file's lines, a torn line skipped loudly. A
// missing file is the os.IsNotExist error, for the caller to decide.
func readChatFile(path string) ([]chatLine, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []chatLine
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for ln := 1; sc.Scan(); ln++ {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var l chatLine
		if err := json.Unmarshal(line, &l); err != nil {
			// A torn line — a crash between write and sync — costs that
			// line and nothing after it, the entries log's own rule.
			fmt.Fprintf(os.Stderr, "chat: %s: skipping malformed line %d: %v\n", filepath.Base(path), ln, err)
			continue
		}
		out = append(out, l)
	}
	return out, sc.Err()
}

func (c *chatStore) Append(l chatLine) error {
	if l.TS.IsZero() {
		l.TS = time.Now()
	}
	if !isoDatePattern.MatchString(l.Date) {
		return fmt.Errorf("chat line for %q: not a date", l.Date)
	}
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return fmt.Errorf("create chat dir: %w", err)
	}
	f, err := os.OpenFile(chatFilePath(c.dir, l.Date), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open chat file for append: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("append: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	c.by[l.Date] = append(c.by[l.Date], l)
	return nil
}

// Prune deletes every day's file dated before keepFrom, whole, and
// forgets it. Run at startup and at the daily tick with keepFrom =
// yesterday. A file that will not delete is reported and the rest go.
func (c *chatStore) Prune(keepFrom string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var errs []error
	for date := range c.by {
		if date >= keepFrom {
			continue
		}
		if err := os.Remove(chatFilePath(c.dir, date)); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
			continue
		}
		delete(c.by, date)
	}
	return errors.Join(errs...)
}

// Day is the day's conversation, oldest first — a copy.
func (c *chatStore) Day(date string) []chatLine {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]chatLine, len(c.by[date]))
	copy(out, c.by[date])
	return out
}

// Turns counts the athlete's messages on a day that were answered, or
// are being answered: what the cap is measured in. A message whose turn
// ended in an error line did not cost a reply and is not charged — a
// provider outage must not lock the day out.
func (c *chatStore) Turns(date string) int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	n := 0
	lines := c.by[date]
	for i, l := range lines {
		if l.Role == "user" && !(i+1 < len(lines) && lines[i+1].Role == "error") {
			n++
		}
	}
	return n
}

// Dates lists the days that hold a conversation, newest first.
func (c *chatStore) Dates() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.by))
	for d := range c.by {
		out = append(out, d)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}

/* ── the coach ─────────────────────────────────────────────────────────── */

// chatTurnTimeout bounds one model turn, tools included. A person is
// waiting on this, which is the opposite of the grader's situation: its
// 45-minute run bound exists because nothing waits on a grade.
const chatTurnTimeout = 2 * time.Minute

// chatTextMax bounds one message from the athlete, in characters — the
// textarea's own unit — so the page and the server refuse the same text.
const chatTextMax = 4000

type coach struct {
	s     *server
	cfg   chatConfig
	llm   *llmClient
	turn  llmTurn
	store *chatStore
	today func() time.Time
	// now is the athlete-local clock, injectable so a test can make it
	// 9:40 pm: what is still doable today depends on the hour.
	now func() time.Time
	// One turn in flight per day's conversation; a second message while
	// it runs is refused, never queued — the reply would answer a question
	// the athlete had already moved past.
	mu   sync.Mutex
	busy map[string]bool
}

// prune is the retention: today and yesterday stay, older days go.
func (c *coach) prune() {
	keepFrom := c.today().AddDate(0, 0, -(chatKeepDays - 1)).Format("2006-01-02")
	if err := c.store.Prune(keepFrom); err != nil {
		log.Printf("coach: pruning conversations before %s: %v", keepFrom, err)
	}
}

func newCoach(s *server, cfg chatConfig, store *chatStore) *coach {
	c := &coach{s: s, cfg: cfg, store: store, busy: map[string]bool{},
		llm: &llmClient{HTTP: &http.Client{}, BaseURL: cfg.BaseURL, Key: cfg.Key,
			Model: cfg.Model, ReasoningEffort: cfg.Effort, MaxTokens: 4096},
		today: func() time.Time { return s.day(s.ds()) },
		now:   func() time.Time { return time.Now().In(s.ds().Loc) }}
	c.turn = c.llm.anthropicTurn
	if cfg.Dialect == "openai" {
		c.turn = c.llm.openaiTurn
	}
	return c
}

// say takes the athlete's message for the day's conversation and starts
// the turn. The message is in the transcript before the model sees it —
// the regrade's note-before-run ordering — so a crash mid-turn loses the
// reply, never the question. Returns an HTTP status and reason when the
// message is refused.
func (c *coach) say(date, text string) (int, string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return http.StatusBadRequest, "say something"
	}
	if n := utf8.RuneCountInString(text); n > chatTextMax {
		return http.StatusRequestEntityTooLarge, fmt.Sprintf("that is %d characters; %d is the most one message can carry", n, chatTextMax)
	}
	if !isoDatePattern.MatchString(date) {
		return http.StatusBadRequest, "date must be YYYY-MM-DD"
	}
	if date != c.today().Format("2006-01-02") {
		return http.StatusConflict, "only today's conversation takes new messages; earlier days are a record"
	}
	c.mu.Lock()
	if c.busy[date] {
		c.mu.Unlock()
		return http.StatusConflict, "still thinking about the last message"
	}
	if c.store.Turns(date) >= c.cfg.TurnsPerDay {
		c.mu.Unlock()
		return http.StatusTooManyRequests, fmt.Sprintf("that is today's %d messages; it picks up again tomorrow", c.cfg.TurnsPerDay)
	}
	c.busy[date] = true // claimed before the append so the lock is not held across an fsync
	c.mu.Unlock()
	if err := c.store.Append(chatLine{Date: date, Role: "user", Text: text}); err != nil {
		c.mu.Lock()
		delete(c.busy, date)
		c.mu.Unlock()
		return http.StatusInternalServerError, "could not record that message, so nothing was asked: " + err.Error()
	}
	go c.run(date)
	return 0, ""
}

// run is one turn: the day's transcript so far, the tools, the system
// prompt rebuilt with the moment's derived context; the reply appended.
func (c *coach) run(date string) {
	defer func() {
		c.mu.Lock()
		delete(c.busy, date)
		c.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), chatTurnTimeout)
	defer cancel()
	start := time.Now()
	// A reply that says "I propose" without having called the tool is the
	// grader's prose-instead-of-tool mode, measured here on 22 Aug 2026:
	// the card never appears and the athlete has nothing to act on. The
	// turn wrapper keeps the latest reply so done can read it, and the
	// model is told once to call the tool or withdraw the word.
	tools := c.tools(date)
	var lastText string
	turn := func(ctx context.Context, system string, msgs []llmMsg, tl []llmTool) (llmMsg, string, error) {
		reply, reason, err := c.turn(ctx, system, msgs, tl)
		if err == nil && reply.Text != "" {
			lastText = reply.Text
		}
		return reply, reason, err
	}
	done := func() bool { return !saysProposes(lastText) || c.store.proposedSince(date, start) }
	reply, err := driveLLM(ctx, turn, c.systemPrompt(date), c.history(date), tools, done,
		"Your reply says you propose a change, but propose_amendment was not called, so the athlete has no card to act on. Either call propose_amendment now with the candidate get_rework offered, or reply again without proposing.")
	if err != nil {
		log.Printf("coach %s: turn FAILED after %s: %v", date, time.Since(start).Round(time.Second), err)
		_ = c.store.Append(chatLine{Date: date, Role: "error", Text: "That turn did not complete: " + err.Error()})
		return
	}
	if strings.TrimSpace(reply) == "" {
		reply = "(no reply)"
	}
	if err := c.store.Append(chatLine{Date: date, Role: "assistant", Text: reply}); err != nil {
		log.Printf("coach %s: reply not recorded: %v", date, err)
	}
	log.Printf("coach %s: replied in %s (%d chars)", date, time.Since(start).Round(time.Second), len(reply))
}

// history is the day's transcript as the model sees it: the athlete's
// words and the model's replies, in order. Error lines are not turns.
func (c *coach) history(date string) []llmMsg {
	var msgs []llmMsg
	for _, l := range c.store.Day(date) {
		switch l.Role {
		case "user", "assistant":
			msgs = append(msgs, llmMsg{Role: l.Role, Text: l.Text})
		case "proposal":
			// Its own proposal, as the model sees it again next turn.
			msgs = append(msgs, llmMsg{Role: "assistant", Text: "[proposed: " + l.Text + "]"})
		case "decision":
			var dec chatDecision
			_ = json.Unmarshal(l.Data, &dec)
			msgs = append(msgs, llmMsg{Role: "user", Text: "[the athlete " + dec.Status + " the proposal]"})
		}
	}
	// Anthropic's dialect wants alternation; two assistant texts in a row
	// (a proposal and the reply after it) fold into one.
	var folded []llmMsg
	for _, m := range msgs {
		if n := len(folded); n > 0 && folded[n-1].Role == m.Role {
			folded[n-1].Text += "\n\n" + m.Text
			continue
		}
		folded = append(folded, m)
	}
	return folded
}

// saysProposes reads a reply for a proposal made in words: "I propose …"
// and not "I cannot propose" or "I am not proposing".
func saysProposes(text string) bool {
	t := strings.ToLower(text)
	if !strings.Contains(t, "i propose") && !strings.Contains(t, "i'd propose") && !strings.Contains(t, "i would propose") {
		return false
	}
	for _, no := range []string{"cannot propose", "can't propose", "can not propose", "not propose", "not proposing", "no proposal"} {
		if strings.Contains(t, no) {
			return false
		}
	}
	return true
}

// proposedSince reports whether a proposal line landed on the day at or
// after t — this turn's, if any.
func (c *chatStore) proposedSince(date string, t time.Time) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, l := range c.by[date] {
		if l.Role == "proposal" && !l.TS.Before(t) {
			return true
		}
	}
	return false
}

/* ── what the model is told ────────────────────────────────────────────── */

// coachProcedure is the chat's own system prompt. It shares nothing with
// gradingProcedure and must not: the grading prompts are byte-pinned by
// the corpus, and a coaching voice is a different job from a verdict.
const coachProcedure = `You are the training log's coach: the judgment layer over a structured running block, its daily log and the archive of recorded activities. The athlete opens this to think a decision through — a sore calf, a missed session, whether a week's quality is compromised, what a test day's number means — and wants the derivation, not the conclusion.

How to work:
- Every number you quote comes from a tool. The prescription, the week, the history, the measured metrics, the log, the rework candidates, the issue ledger and the benchmark timeline are all there, already resolved against the athlete's anchors and units. Never retype a pace, a distance or a heart-rate number from memory, and never estimate one a tool can give you.
- Read before you speak. A question about "this week" needs get_week; about a session, get_prescription and, if it was recorded, get_metrics; about a pattern, session_history; about how the body is doing, get_recent_entries and get_issue_adherence; about progress toward the goal, get_trends. A few calls, then an answer.
- Derive, then judge. Say what the session was for in this phase, what the numbers say it delivered, what the absence of one costs, and what the declared bands currently permit. State the reasoning in the numbers and let the athlete disagree with it.
- You change nothing yourself. You cannot edit the plan, post a grade or log an entry. What you can do is PROPOSE one change per reply with propose_amendment — and only a change that get_rework offers for that date, which you must have called first. A proposal becomes a card in the conversation that the athlete applies or dismisses; until they act, nothing has changed. Say "I propose" or "proposed", never "done", "moved" or "scheduled". Do not propose when the right answer is to absorb the miss, and do not propose twice in one reply.
- The athlete's policies, decided by them and not yours to relitigate: when days run out the survival order is quality, then long, then the decoupling test, then easy, then recovery; a missed decoupling test is dead until the next four-week cycle and the day becomes a plain easy run; graded or recorded days and down, taper and race weeks are not moved; a run may take an easy bike day's slot when it is worth more.
- Be brief. This is read on a phone. Short paragraphs, one derivation per point, no headings, no bullet lists longer than four, no preamble about what you are about to do. Emphasis is *strong* and _em_ and nothing else; no other markup renders.
- "Do nothing, absorb it" is a first-class answer and often the right one. A recommendation must earn its friction.
- Advice about what to still do today must fit the hours left in it — the context says what time it is. Late in the evening a missed dose is missed; the question is tomorrow.
- If a tool refuses or returns nothing, say so plainly and answer with what you have. Do not invent the missing number.`

// systemPrompt is the procedure, the optional athlete-specific coaching
// notes, and the moment's derived context — today, where it sits in the
// block, the day's session, the issues' latest readings, what is standing
// — so the first question does not cost three tool calls to orient.
func (c *coach) systemPrompt(date string) string {
	sp := coachProcedure
	if b, err := os.ReadFile(filepath.Join(c.s.dataDir, "coaching-notes.md")); err == nil && len(b) > 0 {
		if max := c.cfg.NotesMax; max > 0 && len(b) > max {
			full := len(b)
			b = clipAtLine(b, max)
			log.Printf("coach: coaching-notes.md is %d bytes against a %d limit; only the first %d are in effect.", full, max, len(b))
		}
		sp += "\n\nAthlete-specific coaching notes — these override the general guidance where they conflict:\n" + string(b)
	}
	sp += "\n\nContext, derived by the server now:\n" + c.context(date)
	return sp
}

func (c *coach) context(date string) string {
	d := c.s.ds()
	day := c.today()
	now := c.now()
	var b strings.Builder
	// The hour, not just the date: on 21 Aug 2026 the coach told the
	// athlete to still complete the day's strength work at 9:40 pm,
	// because all it knew was the date.
	fmt.Fprintf(&b, "- Today is %s and it is %s, the athlete's local date and time (%s). This conversation is dated %s.\n",
		day.Format("Mon 2 Jan 2006"), now.Format("3:04 pm"), d.Loc, date)
	switch h := now.Hour(); {
	case h >= 20:
		b.WriteString("- The day is nearly over: treat today's unlogged work as missed, not as still to do. Advise about tomorrow and the week, never about fitting more into tonight.\n")
	case h < 6:
		b.WriteString("- It is the small hours: today's session has not happened yet.\n")
	}
	fmt.Fprintf(&b, "- Athlete: %s, units %s.\n", d.Athlete.Name, d.Athlete.Units)
	blk := d.Current(day)
	if blk == nil {
		b.WriteString("- No training block is loaded.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "- Block %q, %s; goal %s", blk.Name, blk.Span(), blk.Goal.Event)
	if blk.Goal.Target != "" {
		fmt.Fprintf(&b, " in %s", blk.Goal.Target)
	}
	b.WriteString(".\n")
	if wk, di, ok := blk.Locate(day); ok {
		sess := wk.Days[di]
		fmt.Fprintf(&b, "- Week %d of %d, day %d", wk.N, blk.WeekCount(), di+1)
		if m := wk.Mesocycle(); m != nil {
			fmt.Fprintf(&b, ", %s", m.Name)
		}
		if len(wk.Tags) > 0 {
			fmt.Fprintf(&b, " (week tags: %s)", strings.Join(wk.Tags, ", "))
		}
		fmt.Fprintf(&b, ". Today's session: %s", stripEmph(sess.Label))
		if a := sess.Amount(c.s.units()); a != "" && a != "—" {
			fmt.Fprintf(&b, ", %s", a)
		}
		if sess.Tag != "" {
			fmt.Fprintf(&b, " [benchmark %s]", sess.Tag)
		}
		b.WriteString(".\n")
	} else {
		switch {
		case blk.StartDate().After(day):
			fmt.Fprintf(&b, "- The block starts in %d days.\n", blk.DaysUntilStart(day))
		default:
			b.WriteString("- The block has finished.\n")
		}
	}
	for _, is := range d.Athlete.Issues {
		if r := c.s.store.RatingOn(is.Key, day.Format("2006-01-02")); r != nil {
			fmt.Fprintf(&b, "- Issue %q rated %s today", is.Name, r.Val)
			if n, err := strconv.Atoi(r.Val); err == nil {
				if band := is.BandFor(n); band != nil {
					fmt.Fprintf(&b, " (%s: %s)", band.Label, band.Action)
				}
			}
			b.WriteString(".\n")
		} else if rs := c.s.store.Ratings(is.Key); len(rs) > 0 {
			last := rs[len(rs)-1]
			fmt.Fprintf(&b, "- Issue %q last rated %s on %s; not rated today.\n", is.Name, last.Val, last.Date)
		}
	}
	if e := c.s.effective(); e.applied > 0 {
		fmt.Fprintf(&b, "- %d amendment(s) standing on the plan; get_week shows them in place.\n", e.applied)
	}
	fmt.Fprintf(&b, "- Messages used today: %d of %d.\n", c.store.Turns(date), c.cfg.TurnsPerDay)
	return b.String()
}

/* ── the tools: every one a read ───────────────────────────────────────── */

// internalGET serves one of the app's own JSON routes to the model, so a
// tool answers exactly what the page would be answered. No handler is
// reimplemented for the chat.
func (s *server) internalGET(path string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, path, nil)
	if err != nil {
		return "", err
	}
	rec := &memResponse{code: http.StatusOK, hdr: http.Header{}}
	s.routes().ServeHTTP(rec, req)
	if rec.code != http.StatusOK {
		return "", fmt.Errorf("%s", strings.TrimSpace(rec.body.String()))
	}
	return rec.body.String(), nil
}

type memResponse struct {
	code int
	hdr  http.Header
	body bytes.Buffer
}

func (m *memResponse) Header() http.Header         { return m.hdr }
func (m *memResponse) Write(b []byte) (int, error) { return m.body.Write(b) }
func (m *memResponse) WriteHeader(code int)        { m.code = code }

// tools is the read set plus, for this turn, propose_amendment: one
// proposal per reply, latched.
func (c *coach) tools(date string) []llmTool {
	proposed := false
	out := c.readTools()
	obj := func(props string) json.RawMessage {
		return json.RawMessage(`{"type":"object","properties":{` + props + `},"additionalProperties":false}`)
	}
	out = append(out, llmTool{
		Name:        "propose_amendment",
		Description: "Propose ONE change to the plan for the athlete to apply or dismiss: exactly one of the candidates get_rework lists for that date (same op and arg). Records a card in the conversation; changes nothing until the athlete acts. Once per reply.",
		Schema: obj(`"date":{"type":"string","description":"YYYY-MM-DD, the day being changed"},` +
			`"op":{"type":"string","description":"move | swap | displace | cancel | plain — as get_rework offered"},` +
			`"arg":{"type":"string","description":"the destination date for move/swap/displace; empty otherwise"},` +
			`"reason":{"type":"string","description":"one sentence: why, in the numbers"}`),
		Run: func(_ context.Context, args json.RawMessage) (string, error) {
			var in struct{ Date, Op, Arg, Reason string }
			if err := json.Unmarshal(args, &in); err != nil {
				return "", err
			}
			if proposed {
				return "", errors.New("one proposal per reply; this reply already carries one")
			}
			if !isoDatePattern.MatchString(in.Date) {
				return "", errors.New("date must be YYYY-MM-DD")
			}
			offer, code := c.s.reworkPayload(in.Date)
			if code != http.StatusOK {
				return "", errors.New(offer.Reason)
			}
			if !offer.Can {
				return "", errors.New("the flow refuses this day: " + offer.Reason)
			}
			var match *reworkCandidate
			var offered []string
			for i := range offer.Candidates {
				cand := offer.Candidates[i]
				if cand.Op == "" {
					continue
				}
				offered = append(offered, cand.Op+" "+cand.Arg)
				if cand.Op == in.Op && cand.Arg == in.Arg {
					match = &offer.Candidates[i]
				}
			}
			if match == nil {
				return "", fmt.Errorf("not one of the candidates get_rework offers for %s (%s)", in.Date, strings.Join(offered, ", "))
			}
			if len(in.Reason) > amendNoteMax {
				in.Reason = in.Reason[:amendNoteMax]
			}
			p := chatProposal{Date: in.Date, Op: match.Op, Arg: match.Arg, Title: match.Title, Detail: match.Detail, Reason: strings.TrimSpace(in.Reason)}
			data, _ := json.Marshal(p)
			text := p.Title
			if p.Detail != "" {
				text += " — " + p.Detail
			}
			if err := c.store.Append(chatLine{Date: date, Role: "proposal", Text: text, Data: data}); err != nil {
				return "", fmt.Errorf("the proposal could not be recorded, so it was not made: %v", err)
			}
			proposed = true
			return "Proposed and shown to the athlete as a card: " + text + ". It is not applied; say so, and leave the decision to them.", nil
		},
	})
	return out
}

// decide is POST /api/chat/decide {date, proposal, status}: the athlete's
// answer to a proposal card. Applying the change itself is the page's
// POST to /api/amend, the rework control's own call; this records what
// happened to the card so the conversation shows it and the model knows.
func (s *server) postChatDecide(w http.ResponseWriter, r *http.Request) {
	if s.coach == nil {
		http.Error(w, "the coach is switched off", http.StatusServiceUnavailable)
		return
	}
	var in struct {
		Date     string `json:"date"`
		Proposal string `json:"proposal"`
		Status   string `json:"status"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if in.Status != "applied" && in.Status != "dismissed" {
		http.Error(w, `status must be "applied" or "dismissed"`, http.StatusBadRequest)
		return
	}
	if !isoDatePattern.MatchString(in.Date) {
		http.Error(w, "date must be YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	found := false
	for _, l := range s.coach.store.Day(in.Date) {
		if l.Role == "proposal" && l.lineID() == in.Proposal {
			found = true
		}
		if l.Role == "decision" {
			var dec chatDecision
			if json.Unmarshal(l.Data, &dec) == nil && dec.Proposal == in.Proposal {
				http.Error(w, "that proposal was already "+dec.Status, http.StatusConflict)
				return
			}
		}
	}
	if !found {
		http.Error(w, "no such proposal on that day", http.StatusNotFound)
		return
	}
	data, _ := json.Marshal(chatDecision{Proposal: in.Proposal, Status: in.Status})
	if err := s.coach.store.Append(chatLine{Date: in.Date, Role: "decision", Text: in.Status, Data: data}); err != nil {
		http.Error(w, "could not record that: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.jsonOK(w)
}

func (c *coach) readTools() []llmTool {
	obj := func(props string) json.RawMessage {
		return json.RawMessage(`{"type":"object","properties":{` + props + `},"additionalProperties":false}`)
	}
	marshal := func(v any, code int, msg string) (string, error) {
		if code != http.StatusOK {
			return "", fmt.Errorf("%s", msg)
		}
		b, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	dateArg := `"date":{"type":"string","description":"YYYY-MM-DD"}`
	return []llmTool{
		{
			Name:        "get_prescription",
			Description: "A day's prescribed session as the athlete sees it: label, detail, targets resolved against their anchors, the week's context, the grading legend, and their current HR/power/pace anchors.",
			Schema:      obj(dateArg),
			Run: func(_ context.Context, args json.RawMessage) (string, error) {
				var in struct{ Date string }
				if err := json.Unmarshal(args, &in); err != nil {
					return "", err
				}
				out, code, msg := c.s.dayPayload(in.Date, "")
				return marshal(out, code, msg)
			},
		},
		{
			Name:        "get_week",
			Description: "The seven days of the week containing a date, as the plan now stands with any amendments replayed: each day's kind, label, amount, benchmark tag, whether a recording exists, its grade, whether it was skipped, and any amendment on it. Use this before reasoning about what a week has left.",
			Schema:      obj(dateArg),
			Run: func(_ context.Context, args json.RawMessage) (string, error) {
				var in struct{ Date string }
				if err := json.Unmarshal(args, &in); err != nil {
					return "", err
				}
				out, code, msg := c.s.weekPayload(in.Date)
				return marshal(out, code, msg)
			},
		},
		{
			Name:        "session_history",
			Description: "Earlier sessions of the same kind as a date's in this block, newest first: grade, duration, distance, average HR, share under the cap, decoupling, conditions. Like for like — a long run against the previous long runs.",
			Schema:      obj(dateArg + `,"limit":{"type":"integer","description":"how many, default 6"}`),
			Run: func(_ context.Context, args json.RawMessage) (string, error) {
				var in struct {
					Date  string
					Limit int
				}
				if err := json.Unmarshal(args, &in); err != nil {
					return "", err
				}
				out, code, msg := c.s.sessionHistoryPayload(in.Date, "", in.Limit)
				return marshal(out, code, msg)
			},
		},
		{
			Name:        "get_metrics",
			Description: "Measured numbers for a recorded activity by its stored filename (get_week and get_prescription name them): HR, power, cadence, decoupling, laps with their triggers, stops, and the grade inputs against the current anchors.",
			Schema:      obj(`"name":{"type":"string","description":"the activity's stored .fit filename"}`),
			Run: func(_ context.Context, args json.RawMessage) (string, error) {
				var in struct{ Name string }
				if err := json.Unmarshal(args, &in); err != nil {
					return "", err
				}
				out, code, msg := c.s.activityMetricsPayload(in.Name)
				return marshal(out, code, msg)
			},
		},
		{
			Name:        "get_recent_entries",
			Description: "The log's recent entries — grades and their notes, the athlete's own notes, daily issue ratings, skips — oldest first. This is what the athlete has said and how recent days were judged.",
			Schema:      obj(`"days":{"type":"integer","description":"how many days back (default 10, max 60)"}`),
			Run: func(_ context.Context, args json.RawMessage) (string, error) {
				var in struct{ Days int }
				_ = json.Unmarshal(args, &in)
				if in.Days <= 0 {
					in.Days = 10
				}
				if in.Days > 60 {
					in.Days = 60
				}
				cutoff := c.today().AddDate(0, 0, -in.Days).Format("2006-01-02")
				type row struct {
					Date string `json:"date"`
					Kind string `json:"kind"`
					Key  string `json:"key,omitempty"`
					Val  string `json:"val,omitempty"`
					Note string `json:"note,omitempty"`
				}
				all := c.s.store.All()
				rows := []row{}
				size := 0
				for i := len(all) - 1; i >= 0 && size < 4*recentEntriesBudget; i-- {
					e := all[i]
					if e.Date < cutoff {
						continue
					}
					kind := e.Kind
					if k := issueKeyOf(e); k != "" {
						kind = "issue"
					} else if e.Kind != "grade" && e.Kind != "note" && e.Kind != kindSkip && e.Kind != kindAmend && e.Kind != "metric" {
						continue
					}
					rows = append(rows, row{e.Date, kind, e.Key, e.Val, e.Note})
					size += len(e.Note) + len(e.Val) + 48
				}
				for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
					rows[i], rows[j] = rows[j], rows[i]
				}
				b, err := json.Marshal(rows)
				return string(b), err
			},
		},
		{
			Name:        "get_rework",
			Description: "What the app's own Rework control would offer for a date: whether the day can be reworked, why not if not, any amendment standing on it, the skip note, and the deterministic candidate moves with their titles and details. Quote these when a change is the answer; the athlete applies one from the day's card.",
			Schema:      obj(dateArg),
			Run: func(_ context.Context, args json.RawMessage) (string, error) {
				var in struct{ Date string }
				if err := json.Unmarshal(args, &in); err != nil {
					return "", err
				}
				if !isoDatePattern.MatchString(in.Date) {
					return "", errors.New("date must be YYYY-MM-DD")
				}
				return c.s.internalGET("/api/rework?date=" + in.Date)
			},
		},
		{
			Name:        "get_issue_adherence",
			Description: "For a declared issue (by key, e.g. \"calf\"): its declared scale and bands, the rehab phase the block is in, the checklist work that tracks it and which of that has and has not been logged recently — the card's own ledger.",
			Schema:      obj(`"key":{"type":"string","description":"the issue's key"}`),
			Run: func(_ context.Context, args json.RawMessage) (string, error) {
				var in struct{ Key string }
				if err := json.Unmarshal(args, &in); err != nil {
					return "", err
				}
				for _, ch := range in.Key {
					if !(ch == '-' || ch == '_' || (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9')) {
						return "", errors.New("key must be lowercase letters, digits, - or _")
					}
				}
				return c.s.internalGET("/api/issue-adherence?key=" + in.Key)
			},
		},
		{
			Name:        "get_trends",
			Description: "The block's benchmark timeline: for each test type the measured series so far (date, week, value, what sits beside it), the change since the first, and the gap to the goal. FTP in watts, decoupling Pa:HR and Pw:HR in percent, LTHR with the pace over the final twenty minutes, time trials against the goal time.",
			Schema:      obj(``),
			Run: func(_ context.Context, _ json.RawMessage) (string, error) {
				d := c.s.ds()
				day := c.today()
				blk := d.Current(day)
				if blk == nil {
					return "", errors.New("no training block loaded")
				}
				type pt struct {
					Date  string `json:"date"`
					Week  int    `json:"week"`
					Text  string `json:"value"`
					Aside string `json:"aside,omitempty"`
				}
				type series struct {
					Label  string `json:"label,omitempty"`
					Points []pt   `json:"points"`
				}
				type panel struct {
					Title   string   `json:"title"`
					Summary string   `json:"summary"`
					Goal    string   `json:"goal,omitempty"`
					Series  []series `json:"series"`
				}
				var out []panel
				for _, p := range benchPanels(c.s.benchmarks(d, blk, day), c.s.units(), blk.Goal) {
					pn := panel{Title: p.Title, Summary: p.Summary, Goal: p.GoalText}
					for _, s := range p.Series {
						sr := series{Label: s.Label}
						for _, x := range s.Points {
							sr.Points = append(sr.Points, pt{x.Date, x.Week, x.Text, x.Aside})
						}
						pn.Series = append(pn.Series, sr)
					}
					out = append(out, pn)
				}
				if out == nil {
					out = []panel{}
				}
				b, err := json.Marshal(out)
				return string(b), err
			},
		},
	}
}

// weekPayload is the week around a date as the plan now stands — the
// effective block, amendments replayed — with what the log and the
// archive say happened on each day. Server-resolved like everything the
// model reads.
func (s *server) weekPayload(dateStr string) (any, int, string) {
	if !isoDatePattern.MatchString(dateStr) {
		return nil, http.StatusBadRequest, "date must be YYYY-MM-DD"
	}
	e := s.effective()
	d := e.d
	day := s.day(d)
	t, err := time.ParseInLocation("2006-01-02", dateStr, d.Loc)
	if err != nil {
		return nil, http.StatusBadRequest, "date must be YYYY-MM-DD"
	}
	blk := d.Current(day)
	if blk == nil || !blk.Contains(t) {
		// Any block that holds the date, current or not.
		blk = nil
		for _, b := range d.Blocks {
			if b.Contains(t) {
				blk = b
			}
		}
		if blk == nil {
			return nil, http.StatusNotFound, "no block holds " + dateStr
		}
	}
	wk, _, ok := blk.Locate(t)
	if !ok {
		return nil, http.StatusNotFound, "no block holds " + dateStr
	}
	grades, skips := s.store.Grades(), s.store.Skips()
	recorded := map[string]bool{}
	if s.metrics != nil {
		if set, err := s.metrics.datesWithActivity(wk.StartISO(), blk.DayOf(wk.N-1, 6).Format("2006-01-02")); err == nil {
			recorded = set
		}
	}
	type dayOut struct {
		Date      string `json:"date"`
		Day       string `json:"day"`
		Kind      string `json:"kind"`
		Label     string `json:"label"`
		Amount    string `json:"amount,omitempty"`
		Tag       string `json:"tag,omitempty"`
		Steps     bool   `json:"structured,omitempty"`
		Past      bool   `json:"past"`
		Today     bool   `json:"today,omitempty"`
		Recorded  bool   `json:"recorded"`
		Grade     string `json:"grade,omitempty"`
		GradeNote string `json:"grade_note,omitempty"`
		Skipped   bool   `json:"skipped,omitempty"`
		SkipNote  string `json:"skip_note,omitempty"`
		Amendment string `json:"amendment,omitempty"`
	}
	out := struct {
		Block  string   `json:"block"`
		Week   int      `json:"week"`
		Weeks  int      `json:"weeks"`
		Dates  string   `json:"dates"`
		Meso   string   `json:"mesocycle,omitempty"`
		Tags   []string `json:"tags,omitempty"`
		Volume string   `json:"prescribed_volume"`
		Ran    string   `json:"ran,omitempty"`
		Days   []dayOut `json:"days"`
	}{Block: blk.Name, Week: wk.N, Weeks: blk.WeekCount(), Dates: wk.Dates(), Tags: wk.Tags,
		Volume: wk.Volume().In(s.units())}
	if m := wk.Mesocycle(); m != nil {
		out.Meso = m.Name
	}
	if ran := s.recordedRunning(blk); ran != nil && !blk.DayOf(wk.N-1, 0).After(day) {
		out.Ran = weekRan(ran, blk, wk.N-1).In(s.units())
	}
	for di, sess := range wk.Days {
		dt := blk.DayOf(wk.N-1, di)
		iso := dt.Format("2006-01-02")
		o := dayOut{Date: iso, Day: dt.Format("Mon"), Kind: string(sess.Kind), Label: stripEmph(sess.Label),
			Tag: sess.Tag, Steps: len(sess.Steps) > 0, Past: dt.Before(day), Today: dt.Equal(day),
			Recorded: recorded[iso]}
		if a := sess.Amount(s.units()); a != "—" {
			o.Amount = a
		}
		if g, ok := grades[iso]; ok {
			o.Grade, o.GradeNote = g.Val, g.Note
		}
		if sk, ok := skips[iso]; ok {
			o.Skipped, o.SkipNote = true, sk.Note
		}
		if info, ok := e.info[iso]; ok {
			o.Amendment = amendLine(info, d.Loc)
		}
		out.Days = append(out.Days, o)
	}
	return out, http.StatusOK, ""
}

/* ── HTTP ──────────────────────────────────────────────────────────────── */

// getChat is GET /api/chat?date=: the day's transcript and whether a turn
// is in flight — what the page renders and polls.
func (s *server) getChat(w http.ResponseWriter, r *http.Request) {
	if s.coach == nil {
		http.Error(w, "the coach is switched off", http.StatusServiceUnavailable)
		return
	}
	date := r.URL.Query().Get("date")
	if date == "" {
		date = s.coach.today().Format("2006-01-02")
	}
	if !isoDatePattern.MatchString(date) {
		http.Error(w, "date must be YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	s.coach.mu.Lock()
	busy := s.coach.busy[date]
	s.coach.mu.Unlock()
	type msg struct {
		TS   string          `json:"ts"`
		ID   string          `json:"id"`
		Role string          `json:"role"`
		Text string          `json:"text"`
		Data json.RawMessage `json:"data,omitempty"`
	}
	out := struct {
		Date     string   `json:"date"`
		Today    string   `json:"today"`
		Busy     bool     `json:"busy"`
		Turns    int      `json:"turns"`
		Cap      int      `json:"cap"`
		Messages []msg    `json:"messages"`
		Days     []string `json:"days"`
	}{Date: date, Today: s.coach.today().Format("2006-01-02"), Busy: busy,
		Turns: s.coach.store.Turns(date), Cap: s.coach.cfg.TurnsPerDay, Messages: []msg{}, Days: s.coach.store.Dates()}
	for _, l := range s.coach.store.Day(date) {
		out.Messages = append(out.Messages, msg{l.TS.UTC().Format(time.RFC3339), l.lineID(), l.Role, l.Text, l.Data})
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, out)
}

// postChat is POST /api/chat {date?, text}: one message for the day's
// conversation; 202 and the turn runs, the page polls.
func (s *server) postChat(w http.ResponseWriter, r *http.Request) {
	if s.coach == nil {
		http.Error(w, "the coach is switched off", http.StatusServiceUnavailable)
		return
	}
	var in struct {
		Date string `json:"date"`
		Text string `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 6*chatTextMax+1024)).Decode(&in); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if in.Date == "" {
		in.Date = s.coach.today().Format("2006-01-02")
	}
	if code, reason := s.coach.say(in.Date, in.Text); code != 0 {
		http.Error(w, reason, code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "thinking", "date": in.Date})
}

type coachData struct {
	Nav   string
	Title string
	Date  string
	Today bool
	Off   bool
}

// coachPage is GET /coach (+?date=): the conversation, rendered by
// coach.js from /api/chat so the transcript and the poll share one
// source of truth.
func (s *server) coachPage(w http.ResponseWriter, r *http.Request) {
	cd := coachData{Nav: "coach", Title: "Coach"}
	if s.coach == nil {
		cd.Off = true
		s.render(w, "coach.html", cd)
		return
	}
	today := s.coach.today().Format("2006-01-02")
	cd.Date = r.URL.Query().Get("date")
	if cd.Date == "" {
		cd.Date = today
	}
	if !isoDatePattern.MatchString(cd.Date) {
		http.Error(w, "date must be YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	cd.Today = cd.Date == today
	s.render(w, "coach.html", cd)
}
