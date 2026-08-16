package main

// The auto-grader: after a successful activity import, an in-process LLM
// tool loop reads the day's prescription and the measured metrics through
// the same builders the API serves, judges the session, and posts the grade
// entry — standalone, no external session, provider chosen by config.
//
// Config comes from the environment (the server's compose env_file, never
// the repo):
//
//	GRADER_MODE      off | dry | live   (default off; dry logs the would-be
//	                                     grade without posting)
//	GRADER_PROVIDER  anthropic | openai (default anthropic; openai covers
//	                                     any OpenAI-compatible base URL,
//	                                     Ollama included)
//	GRADER_DIALECT   anthropic | openai (wire dialect override for bases
//	                                     that speak the other one, e.g.
//	                                     Ollama's /v1/messages; defaults to
//	                                     the provider's own)
//	GRADER_MODEL     model id           (default claude-opus-5 on anthropic)
//	GRADER_BASE_URL  scheme://host      (defaults to the provider's API)
//	GRADER_API_KEY   secret             (empty is allowed for local bases)
//
// Trigger and idempotency: an import whose training day falls within the
// last two days in the athlete's timezone is graded; a backfill is
// new-to-store but old-by-date and never auto-graded. The posted grade
// entry (kind "grade", keyed by ISO date, replay latest-wins) is the
// durable idempotency marker; a failed grade retries on the next import or
// the startup reconcile, and never blocks the import itself. Rest days,
// out-of-block days, sport/kind mismatches and already-graded days are
// skipped. Manual "pull and post" stays possible forever — a correction is
// just a later entry, per the log's law.
//
// Failure policy: any provider or tool failure, in dry or live mode, logs
// loudly and leaves the day ungraded — never a partial post.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type graderConfig struct {
	Mode     string
	Provider string
	Dialect  string
	Model    string
	BaseURL  string
	Key      string
	// Effort is GRADER_REASONING_EFFORT, passed through on the OpenAI
	// dialect. Some reasoning models reject function tools on
	// /v1/chat/completions unless it is "none".
	Effort string
	// NotesMax bounds the athlete-specific overlay appended to the embedded
	// procedure, in bytes. GRADER_NOTES_MAX raises it for a model with room
	// for more.
	NotesMax int
}

func graderConfigFromEnv() (graderConfig, error) {
	c := graderConfig{
		Mode:     envOr("GRADER_MODE", "off"),
		Provider: envOr("GRADER_PROVIDER", "anthropic"),
		Dialect:  os.Getenv("GRADER_DIALECT"),
		Model:    os.Getenv("GRADER_MODEL"),
		BaseURL:  os.Getenv("GRADER_BASE_URL"),
		Key:      os.Getenv("GRADER_API_KEY"),
		Effort:   os.Getenv("GRADER_REASONING_EFFORT"),
	}
	c.NotesMax = defaultNotesMax
	if v := os.Getenv("GRADER_NOTES_MAX"); v != "" {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n <= 0 {
			return c, fmt.Errorf("GRADER_NOTES_MAX is %q, want a positive byte count", v)
		}
		c.NotesMax = n
	}
	switch c.Mode {
	case "off", "dry", "live":
	default:
		return c, fmt.Errorf("GRADER_MODE is %q, want off, dry or live", c.Mode)
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
			return c, fmt.Errorf("GRADER_PROVIDER=openai needs GRADER_MODEL")
		}
	default:
		return c, fmt.Errorf("GRADER_PROVIDER is %q, want anthropic or openai", c.Provider)
	}
	if c.Dialect == "" {
		c.Dialect = c.Provider
	}
	if c.Dialect != "anthropic" && c.Dialect != "openai" {
		return c, fmt.Errorf("GRADER_DIALECT is %q, want anthropic or openai", c.Dialect)
	}
	return c, nil
}

// gradeRecentDays is the auto-grade window: an import whose training day is
// older than this many days is a backfill, not a fresh session.
const gradeRecentDays = 2

// defaultNotesMax bounds data/grading-notes.md, the athlete-specific overlay
// appended to the embedded procedure. It is the one input to the system prompt
// with no natural ceiling: it is a hand-edited file read fresh on every run, so
// a runaway edit or a truncated write would otherwise be pasted in whole and
// push the measured numbers out of a small model's context. 16 KiB is about
// three times the current file against a 7.5 KiB procedure — room to grow
// without ever tripping. GRADER_NOTES_MAX raises it where the model has room.
const defaultNotesMax = 16 << 10

// recentEntriesBudget bounds the log excerpt the grader reads, in bytes of
// entry payload. Generous enough to carry several prior grade notes (the
// house style is learned from them), small enough that a local model's
// context holds the whole conversation.
const recentEntriesBudget = 6000

type grader struct {
	s     *server
	cfg   graderConfig
	llm   *llmClient
	today func() time.Time // the athlete-timezone day; tests pin it
	// blindDate hides that date's own grade from the log excerpt. A
	// comparison against a human's grade is worthless if the grader can
	// read it first, and re-grading a day whose verdict is already in the
	// log is exactly what an evaluation does.
	blindDate string

	mu       sync.Mutex
	inFlight map[string]bool // ISO dates being graded right now
}

func newGrader(s *server, cfg graderConfig) *grader {
	return &grader{
		s: s, cfg: cfg,
		llm: &llmClient{
			// No client timeout: the per-run context deadline below is the
			// real bound, and it has to be, because a local model on modest
			// hardware spends tens of seconds per turn — measured at ~50 s
			// for a first turn on the server's 4B. A client timeout shorter
			// than the run would kill grades that were about to succeed.
			HTTP:            &http.Client{},
			BaseURL:         cfg.BaseURL,
			Key:             cfg.Key,
			Model:           cfg.Model,
			ReasoningEffort: cfg.Effort,
			MaxTokens:       2048,
		},
		today:    func() time.Time { return s.day(s.ds()) },
		inFlight: map[string]bool{},
	}
}

// maybeGrade applies every skip rule, then grades. Runs in its own
// goroutine; nothing here can block or fail an import.
func (g *grader) maybeGrade(m *activityMetrics) {
	if reason := g.skipReason(m); reason != "" {
		log.Printf("grader: %s (%s): skipped — %s", m.Name, m.Date, reason)
		return
	}
	g.mu.Lock()
	if g.inFlight[m.Date] {
		g.mu.Unlock()
		log.Printf("grader: %s: a grade for %s is already running", m.Name, m.Date)
		return
	}
	g.inFlight[m.Date] = true
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.inFlight, m.Date)
		g.mu.Unlock()
	}()

	if _, err := g.grade(m); err != nil {
		log.Printf("grader: %s (%s): FAILED, day left ungraded: %v", m.Name, m.Date, err)
	}
}

// gradeResult is what a run concluded — returned so a caller can compare it
// (dry mode against a human's grade, say) rather than reading it out of the
// log.
type gradeResult struct {
	Date, Val, Note string
}

// skipReason is every rule that makes an import not-a-grading-trigger,
// returned as prose so the log says why.
func (g *grader) skipReason(m *activityMetrics) string {
	if m.Sport != "running" && m.Sport != "cycling" {
		return "sport " + m.Sport + " is not graded"
	}
	d := g.s.ds()
	today := g.today()
	date, err := time.ParseInLocation("2006-01-02", m.Date, d.Loc)
	if err != nil {
		return "unparseable date"
	}
	if date.Before(today.AddDate(0, 0, -gradeRecentDays)) {
		return "a backfill, not a fresh session"
	}
	blk := d.Current(today)
	if blk == nil {
		return "no block loaded"
	}
	wk, di, ok := blk.Locate(date)
	if !ok {
		return "date is outside the current block"
	}
	sess := wk.Days[di]
	if sess.Kind == KindRest {
		return "a rest day is never graded"
	}
	if (m.Sport == "cycling") != sess.Kind.IsBike() {
		return fmt.Sprintf("recorded %s but the day prescribes %s", m.Sport, sess.Kind)
	}
	if _, ok := g.s.store.Grades()[m.Date]; ok {
		return "already graded"
	}
	return ""
}

// reconcile retries recent ungraded days at startup — the recovery path
// for a grade that failed after its import succeeded. Sequential and
// best-effort; the import trigger remains the normal path.
func (g *grader) reconcile() {
	since := g.today().AddDate(0, 0, -gradeRecentDays).Format("2006-01-02")
	acts, err := g.s.metrics.recent(since)
	if err != nil {
		log.Printf("grader reconcile: %v", err)
		return
	}
	seen := map[string]bool{}
	days, graded := 0, 0
	for i := range acts {
		if seen[acts[i].Date] {
			continue
		}
		seen[acts[i].Date] = true
		days++
		// Say why, always. A reconcile that decides silently is
		// indistinguishable from one that never ran.
		if reason := g.skipReason(&acts[i]); reason != "" {
			log.Printf("grader reconcile: %s (%s): skipped — %s", acts[i].Name, acts[i].Date, reason)
			continue
		}
		graded++
		g.maybeGrade(&acts[i])
	}
	log.Printf("grader reconcile: %d activities since %s, %d day(s), %d graded",
		len(acts), since, days, graded)
}

// gradeTimeout bounds one whole grading run. Generous because a local
// model is genuinely slow: measured on the server's 4B, single turns cost
// 45 s to 6 min and a full run 13. Nothing waits on this — it happens
// after an import, off every request path — so the only thing a tight
// bound would buy is killed grades.
const gradeTimeout = 45 * time.Minute

func (g *grader) grade(m *activityMetrics) (*gradeResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gradeTimeout)
	defer cancel()

	posted := false
	var result *gradeResult
	tools := g.tools(m, &posted, &result)
	turn := g.llm.anthropicTurn
	if g.cfg.Dialect == "openai" {
		turn = g.llm.openaiTurn
	}
	prompt := fmt.Sprintf(
		"Grade the recorded activity %q for %s. Read the prescription and the metrics with the tools, then post exactly one grade.",
		m.Name, m.Date)

	final, err := runLLMLoop(ctx, turn, g.systemPrompt(), prompt, tools,
		func() bool { return posted },
		"You have not recorded anything yet: analysis written as a message is discarded. "+
			"Call post_grade now with the date, the grade letter (or DNF), and your note as its arguments. "+
			"If you genuinely cannot grade this session, say why in one sentence instead.")
	if err != nil {
		return nil, err
	}
	if !posted {
		return nil, fmt.Errorf("loop finished without posting: %s", strings.TrimSpace(final))
	}
	log.Printf("grader: %s (%s): done (mode=%s)", m.Name, m.Date, g.cfg.Mode)
	return result, nil
}

// tools are the loop's whole world: the same payloads the API serves, the
// recent log for context, and one chance to post.
func (g *grader) tools(m *activityMetrics, posted *bool, result **gradeResult) []llmTool {
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
	return []llmTool{
		{
			Name:        "get_prescription",
			Description: "The day's prescribed session, week context, grading legend, and the athlete's current anchors.",
			Schema:      obj(`"date":{"type":"string","description":"YYYY-MM-DD"}`),
			Run: func(_ context.Context, args json.RawMessage) (string, error) {
				var in struct{ Date string }
				if err := json.Unmarshal(args, &in); err != nil {
					return "", err
				}
				out, code, msg := g.s.dayPayload(in.Date, "")
				return marshal(out, code, msg)
			},
		},
		{
			Name:        "session_history",
			Description: "Earlier sessions of the SAME kind in this block, newest first, each measured the way this one will be — grade, duration, distance, average HR, share under the grade cap, decoupling, and the conditions it was run in. Compare the weather as well as the numbers: heat costs several beats at a given effort, so the same heart rate on a hotter day is a better session and a lower one on a cool day may be no improvement at all. Say which it was. This is how a session is compared like for like: a long run against the previous long run, a quality session against the previous quality session. The day being graded is never included.",
			Schema: obj(`"date":{"type":"string","description":"YYYY-MM-DD, the day being graded"},` +
				`"limit":{"type":"integer","description":"how many earlier sessions, default 6"}`),
			Run: func(_ context.Context, args json.RawMessage) (string, error) {
				var in struct {
					Date  string
					Limit int
				}
				if err := json.Unmarshal(args, &in); err != nil {
					return "", err
				}
				out, code, msg := g.s.sessionHistoryPayload(in.Date, "", in.Limit)
				return marshal(out, code, msg)
			},
		},
		{
			Name:        "get_metrics",
			Description: "Measured numbers for a stored activity: HR, power, cadence, decoupling, and the grade inputs computed against the current anchors.",
			Schema:      obj(`"name":{"type":"string","description":"the activity's stored .fit filename"}`),
			Run: func(_ context.Context, args json.RawMessage) (string, error) {
				var in struct{ Name string }
				if err := json.Unmarshal(args, &in); err != nil {
					return "", err
				}
				out, code, msg := g.s.activityMetricsPayload(in.Name)
				return marshal(out, code, msg)
			},
		},
		{
			Name:        "fastest_segments",
			Description: "The fastest non-overlapping stretches of a run, given a rep length in metres OR a rep duration in seconds, and how many to find. This is how interval work is checked: ask for the prescribed number of reps at the prescribed length, then compare each one's pace and heart rate to the target. They come back in the order they were run, so a fade is visible. Use this for a session the watch did NOT drive — where the laps carry no step index and the reps are buried in one continuous trace. Where the laps do carry step indices, they are the record and this is not needed. A stretch that spans a stop in the recording is never returned, because the file does not say how far the athlete travelled while it was stopped — so fewer stretches than you asked for means the work is not in the trace, gap-free, that many times.",
			Schema: obj(`"name":{"type":"string"},"meters":{"type":"number","description":"rep length, e.g. 200 or 1000"},` +
				`"secs":{"type":"integer","description":"rep duration instead of a length, e.g. 300 for 5:00"},` +
				`"count":{"type":"integer","description":"how many reps were prescribed"}`),
			Run: func(_ context.Context, args json.RawMessage) (string, error) {
				var in struct {
					Name   string
					Meters float64 `json:"meters"`
					Secs   int     `json:"secs"`
					Count  int     `json:"count"`
				}
				if err := json.Unmarshal(args, &in); err != nil {
					return "", err
				}
				if !validActivityName(in.Name) {
					return "", fmt.Errorf("name must be a plain .fit filename")
				}
				if in.Meters <= 0 && in.Secs <= 0 {
					return "", fmt.Errorf("give the rep length in meters or its duration in secs")
				}
				if in.Count < 1 || in.Count > 40 {
					return "", fmt.Errorf("count must be between 1 and 40")
				}
				data, err := os.ReadFile(filepath.Join(g.s.activitiesDir(), in.Name))
				if err != nil {
					return "", fmt.Errorf("could not read %s", in.Name)
				}
				st, err := decodeActivity(data)
				if err != nil {
					return "", err
				}
				segs := fastestSegments(st, in.Meters, in.Secs, in.Count, g.s.ds().Athlete.Units)
				if len(segs) == 0 {
					return "", fmt.Errorf("no stretches that long in %s", in.Name)
				}
				b, err := json.Marshal(map[string]any{"name": in.Name, "segments": segs})
				return string(b), err
			},
		},
		{
			Name:        "hr_share_under",
			Description: "The share of an activity's valid heart-rate time at or under a bpm ceiling — the number the legend's bands read. Use it whenever the day prescribes its own ceiling instead of the standing one; the everyday share in get_metrics answers only for the standing cap.",
			Schema:      obj(`"name":{"type":"string"},"bpm":{"type":"integer","description":"the ceiling to measure against"}`),
			Run: func(_ context.Context, args json.RawMessage) (string, error) {
				var in struct {
					Name string
					BPM  int `json:"bpm"`
				}
				if err := json.Unmarshal(args, &in); err != nil {
					return "", err
				}
				if !validActivityName(in.Name) {
					return "", fmt.Errorf("name must be a plain .fit filename")
				}
				if in.BPM <= 0 {
					return "", fmt.Errorf("bpm must be a positive ceiling")
				}
				share, err := g.s.metrics.underCapShareSQL(in.Name, in.BPM)
				if err != nil {
					return "", err
				}
				if share == nil {
					return "", fmt.Errorf("no valid heart-rate time recorded for %s", in.Name)
				}
				b, err := json.Marshal(map[string]any{
					"name": in.Name, "bpm": in.BPM, "share_at_or_under": pyRound(*share, 4)})
				return string(b), err
			},
		},
		{
			Name:        "get_recent_entries",
			Description: "Recent log entries — prior grades and their notes, the athlete's free-text notes, daily issue ratings — for context and for the note's house style.",
			Schema:      obj(`"days":{"type":"integer","description":"how many days back (default 10, max 30)"}`),
			Run: func(_ context.Context, args json.RawMessage) (string, error) {
				var in struct{ Days int }
				_ = json.Unmarshal(args, &in)
				if in.Days <= 0 {
					in.Days = 10
				}
				if in.Days > 30 {
					in.Days = 30
				}
				d := g.s.ds()
				cutoff := g.s.day(d).AddDate(0, 0, -in.Days).Format("2006-01-02")
				type row struct {
					Date string `json:"date"`
					Kind string `json:"kind"`
					Key  string `json:"key,omitempty"`
					Val  string `json:"val,omitempty"`
					Note string `json:"note,omitempty"`
				}
				// Newest first while filling, so a long history spends the
				// budget on what is most relevant, then flipped back to
				// chronological order for reading. The budget exists because
				// a local model's context is small and silently truncating
				// it would drop the system prompt and the measured numbers —
				// the two things a grade cannot be made without.
				all := g.s.store.All()
				rows := []row{}
				size := 0
				for i := len(all) - 1; i >= 0 && size < recentEntriesBudget; i-- {
					e := all[i]
					if e.Date < cutoff {
						continue
					}
					kind := e.Kind
					if k := issueKeyOf(e); k != "" {
						kind = "issue"
					} else if e.Kind != "grade" && e.Kind != "note" && e.Kind != kindSkip {
						continue
					}
					if kind == "grade" && e.Date == g.blindDate {
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
			Name:        "post_grade",
			Description: "Post the day's grade entry. Callable exactly once; val is the grade letter, or DNF where the session was not essentially completed; note is the one-paragraph reasoning.",
			Schema: obj(`"date":{"type":"string"},"val":{"type":"string","description":"the grade letter, or DNF"},` +
				`"note":{"type":"string","description":"one paragraph, plain ASCII, derivation first"}`),
			Run: func(_ context.Context, args json.RawMessage) (string, error) {
				var in struct{ Date, Val, Note string }
				if err := json.Unmarshal(args, &in); err != nil {
					return "", err
				}
				if *posted {
					return "", fmt.Errorf("a grade was already posted this run")
				}
				if in.Date != m.Date {
					return "", fmt.Errorf("this run grades %s, not %s", m.Date, in.Date)
				}
				in.Val = strings.TrimSpace(in.Val)
				if l := len(in.Val); l == 0 || l > 3 {
					return "", fmt.Errorf("val must be a short grade like A or B+")
				}
				if strings.TrimSpace(in.Note) == "" {
					return "", fmt.Errorf("a grade without its reasoning is not postable")
				}
				*result = &gradeResult{Date: in.Date, Val: in.Val, Note: in.Note}
				if g.cfg.Mode == "dry" {
					*posted = true
					log.Printf("grader DRY RUN %s: grade %s — %s", in.Date, in.Val, in.Note)
					return "dry-run mode: the grade was logged, not posted", nil
				}
				if err := g.s.store.Append(Entry{Date: in.Date, Kind: "grade", Val: in.Val, Note: in.Note}); err != nil {
					return "", err
				}
				// Not posted until read back — the same law the manual
				// procedure follows.
				got, ok := g.s.store.Grades()[in.Date]
				if !ok || got.Val != in.Val || got.Note != in.Note {
					return "", fmt.Errorf("read-back after append did not match")
				}
				*posted = true
				return "posted and verified", nil
			},
		},
	}
}

// gradingProcedure is the embedded judgment: how any athlete's day is
// graded from its numbers. Athlete-specific traps live in the volume's
// grading-notes.md, never here — this string ships in a public binary.
const gradingProcedure = `You are the training log's automated workout grader. A recorded activity has been imported; grade the day against its prescription and post exactly one grade entry, as a careful coach reading the numbers would.

Procedure:
1. get_prescription for the date and get_metrics for the activity. Read the prescription's steps when it has them: they are what was actually asked for that day, in the same units the metrics come back in. get_recent_entries for context: prior grades and their notes, the athlete's own notes, issue ratings. The prescription names the previous session of this same kind as "previous"; call session_history for the fuller series of them when you want the trend rather than a single comparison.
2. Decide the grade:
   - Runs: the grading legend's bands applied to grade_input.under_grade_cap_share decide the letter. The bands are listed best first and each carries min_pct, its floor as a percentage: the letter is the FIRST band whose floor the share reaches. Convert the share to a percentage before comparing, and do that comparison rather than eyeballing the range text — a share of 21% belongs to the band opening at 20, not the one below it.
   - Bikes: judge against what THIS DAY prescribed, which the prescription's steps and targets state — the power bands, the interval structure, the duration. Compare the measured average watts and elapsed time to those. A hard day (intervals, a VO₂ or threshold session) is SUPPOSED to run above the athlete's easy-ride HR band, so a low in_band_share_after_warmup is not a fault there and never decides the grade; that share is the yardstick for an easy or recovery ride only. The letter bands in the legend are the run rubric and do not apply to a bike at all. No single threshold: weigh execution of the prescribed work first, then duration, then whether anything exceeded the cap.
   - Test days (the day carries a benchmark tag): THE LETTER BANDS DO NOT APPLY AT ALL, for a run test as much as a bike one. A test is graded on protocol execution — was the measurement made valid — and the note says so explicitly. Never compute an under-cap share for a test day and never let one decide the letter: these sessions are supposed to run above the everyday ceilings, so a low share is the protocol working. Read the per-minute profile in the metrics to judge execution, because averages and peaks cannot tell a ramp that climbed to failure from a steady ride with one surge. A ramp that rises through its steps and ends at its peak went to failure: the measurement is valid and the grade should say so, even though such a ride's average is low by construction.
   - A session often states its intensity in more than one currency — a heart-rate band AND a power band or a pace band. One of them is the requirement and the others are its outputs; the athlete notes say which governs for each kind of day. Judge the governing one. Treat the others as corroboration, and where a session held its governing target, a small deviation in an output is compliance rather than a fault: say so in a clause and move on, do not let it move the letter.
   - DNF is a verdict, not a letter, and it is the right one whenever the session was not essentially completed — whatever the reason. Essentially complete means the prescribed work was delivered: at least 90% of the prescribed distance or duration, and every prescribed repetition (a rep counts when it covered at least nine tenths of its step). A long run that came back four miles short is DNF; two of four intervals is DNF; a mechanical failure, an injury, weather, or simply stopping are all DNF. Post val "DNF" and let the NOTE carry both halves: what stopped it, and what the work that WAS delivered measured — name the letter it would have earned when that is clear. A letter is for a session that was completed; grading a session that was not finished puts a number on a day that has none.
   - Completion is about the WORK DONE, not about whether it hit its targets. A session that covered its distance or its time and ran every prescribed rep is complete even when it was executed badly, and it takes its letter — ten miles of tempo against a ten-mile easy prescription is a completed session graded F, not a DNF, because the athlete did the volume and did the wrong thing with it. DNF is for the day that ended early. F is for the day that finished wrong.
   - A session the athlete marked skipped is not graded and never appears as a failure: it is a recorded decision, and the prescription says so. What it is worth is context for the days around it — the work was not done, so the week carries less of it and the next session meets fresher legs. Mention it only where it bears on the day being graded.
   - A RUN whose prescription carries steps with pace targets — intervals, threshold reps, a time trial — is graded on that work, never on the under-cap share. Those sessions are meant to run above the everyday ceiling, so the share measures nothing about them; do not compute it and do not cite it. Read the steps for what was asked (how many reps, how long, at what pace) and call fastest_segments with those numbers to see what was actually run. Grade whether the reps were delivered: the count, the paces against target, and whether they held or faded. Heart rate is the cost of that work, not the target. If the session was cut short, say how much of it was completed.
   - A day that prescribes its own ceiling — a recovery run capped below the everyday cap, say — is judged against THAT ceiling: call hr_share_under with the day's own number and apply the legend's bands to what it returns. Against the generic cap such a session is a trivial pass, which measures nothing. Judge the share, never the average: an average sitting under a ceiling says nothing about how long was spent above it.
   - A DAY THAT PRESCRIBES REPETITIONS IS GRADED ON THE REPETITIONS, and you must find them before you grade. Look at the laps get_metrics returned. IF ANY LAP CARRIES A step INDEX, those laps are the record — the watch was driven by the workout this app pushed, each such lap IS the prescribed step, and you read them and do not call fastest_segments. IF NO LAP CARRIES A step INDEX — which includes a payload with no laps at all — then CALL fastest_segments with the prescribed count and length before deciding anything. The reps are buried in one continuous trace there and nothing else recovers them; grading such a day without that call means grading it on the under-cap share, which is the trap this whole procedure exists to avoid.
   - WHAT A LAP MEANS DEPENDS ON ITS TRIGGER. "distance" is the watch's own auto-lap: it fires every mile and marks a split, never a repetition. "manual" is the athlete pressing the button, sometimes for a rep and sometimes twice in two seconds while fiddling with the watch — a lap far shorter than the work it should hold was the button, not a failed rep, and must never be reported as one (12 Aug 2026 carries a 2.4 s lap at 35 W and a 0.8 s lap at 318 W; neither is a rep). "time" with a step index is the pushed workout driving that step. Where a lap's elapsed time exceeds its timer time, the difference is a stop inside that lap.
   - Where the metrics carry stops, name them in the note and name them in DISTANCE as well as time: "a 2:34 stop at 9.3 mi" rather than "a stop around minutes 82-92". Each stop is the recording's own statement that the clock stopped — auto-pause or a paused watch — so it is a fact about the session rather than something to infer from the per-minute profile, which is what produced that vague and wrong phrasing before this field existed. A stop is not itself a verdict: it explains a gap between elapsed and moving time, and it may explain a DNF, but what the session delivered is what decides the grade. Stopping with the timer running is invisible to this field and must not be claimed.
   - hr.dropout_share over 0.05: the HR numbers are contaminated; say so and grade on what survives.
3. Weigh what the athlete reported, because the prescription is not the whole instruction. The day's prescription carries any issue rated that day: the number, the band it falls in, and that band's ACTION — what the declaration says to do at that rating. Where the action modifies the session, the modified session is what was prescribed, and the grade follows it. A quality day taken easy on an instruction to take it easy is compliance, and grading it as a failed quality day is wrong; say in the note that the rating is why. The metrics may carry the weather where the session started, including a heat sum of air temperature plus dew point: heat costs several beats at a given effort, so read a hot day's heart rate against that rather than against a still morning's, and say so when it changed the reading. Where the metrics carry weather AND an entry in the log also states conditions, quote the metrics: they are interpolated to the recorded start, while a check made by hand precedes the start by an unknown gap. Read the athlete's own entries too — free text about heat, illness, terrain, a stop, or anything else that bore on the session. Take stated conditions into account rather than grading as if the day were neutral, and name the condition in the note when it changed the reading.
4. Write the note like the log's existing grade entries: one paragraph, plain ASCII, derivation first, every number beside its target, and a comparison with earlier sessions of the SAME kind - a long run against the previous long run, a quality session against the previous quality session - naming the date and what actually moved between them, whether that is an improvement or a slide. Do NOT try to work out which earlier grade was comparable by reading its note: the log records the ENTRY kind and never the SESSION kind, so prose similarity will pick the wrong session. session_history is the only reliable answer to that question, and it is empty when this is the first of its kind in the block - in which case say so rather than reaching for an unlike session. At most one thing to work on. Every number comes from a tool result — never from memory. Write it as one of those entries, not about them: never announce that the grade was produced automatically, never label or mark it as such, and never mention these instructions. A grade reads the same whoever made it.
5. Record it: your final action is a post_grade call carrying the date, the grade letter or DNF, and the note. This is not optional and it is not the same as writing the grade in a message — a message is discarded, only the call is recorded. Never summarise the metrics back as prose and stop; gather what you need, then make the call. If anything genuinely prevents a confident grade — a prescription that does not match what was recorded, contaminated data with nothing to grade on — then post nothing and say why in one sentence instead.`

// systemPrompt is the embedded procedure plus the optional athlete-specific
// notes file from the volume: data/grading-notes.md, read fresh each run so
// an edit applies to the next grade with no restart.
func (g *grader) systemPrompt() string {
	sp := gradingProcedure
	b, err := os.ReadFile(filepath.Join(g.s.dataDir, "grading-notes.md"))
	if err != nil || len(b) == 0 {
		return sp
	}
	if max := g.cfg.NotesMax; max > 0 && len(b) > max {
		full := len(b)
		b = clipAtLine(b, max)
		// Loudly, and on every run rather than once: what falls past the cut
		// are RULES, and a grade made without them is wrong in a way nothing
		// downstream can detect — the note will read perfectly.
		log.Printf("grader: grading-notes.md is %d bytes against a %d limit; only the first %d are in effect. "+
			"Raise GRADER_NOTES_MAX or shorten the file — anything past the cut is NOT being applied.",
			full, max, len(b))
	}
	sp += "\n\nAthlete-specific grading notes — these override the general procedure where they conflict:\n" + string(b)
	return sp
}

// clipAtLine is the longest prefix of b within max bytes that ends on a line
// boundary. Cutting mid-sentence is worse than cutting early here: half a rule
// can read as a different rule, and "never grade a test day on the under-cap
// share" truncated at the wrong byte says the opposite of itself.
func clipAtLine(b []byte, max int) []byte {
	if len(b) <= max {
		return b
	}
	cut := b[:max]
	if i := bytes.LastIndexByte(cut, '\n'); i > 0 {
		return cut[:i+1]
	}
	return cut
}
