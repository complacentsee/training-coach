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
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
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
			"Call post_grade now with the date, the grade letter, and your note as its arguments. "+
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
					} else if e.Kind != "grade" && e.Kind != "note" {
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
			Description: "Post the day's grade entry. Callable exactly once; val is the letter, note is the one-paragraph reasoning.",
			Schema: obj(`"date":{"type":"string"},"val":{"type":"string","description":"the grade letter"},` +
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
1. get_prescription for the date and get_metrics for the activity. Read the prescription's steps when it has them: they are what was actually asked for that day, in the same units the metrics come back in. get_recent_entries for context: prior grades and their notes, the athlete's own notes, issue ratings.
2. Decide the grade:
   - Runs: the grading legend's bands applied to grade_input.under_grade_cap_share decide the letter. The bands are listed best first and each carries min_pct, its floor as a percentage: the letter is the FIRST band whose floor the share reaches. Convert the share to a percentage before comparing, and do that comparison rather than eyeballing the range text — a share of 21% belongs to the band opening at 20, not the one below it.
   - Bikes: judge against what THIS DAY prescribed, which the prescription's steps and targets state — the power bands, the interval structure, the duration. Compare the measured average watts and elapsed time to those. A hard day (intervals, a VO₂ or threshold session) is SUPPOSED to run above the athlete's easy-ride HR band, so a low in_band_share_after_warmup is not a fault there and never decides the grade; that share is the yardstick for an easy or recovery ride only. The letter bands in the legend are the run rubric and do not apply to a bike at all. No single threshold: weigh execution of the prescribed work first, then duration, then whether anything exceeded the cap.
   - Test days (the day carries a benchmark tag): THE LETTER BANDS DO NOT APPLY AT ALL, for a run test as much as a bike one. A test is graded on protocol execution — was the measurement made valid — and the note says so explicitly. Never compute an under-cap share for a test day and never let one decide the letter: these sessions are supposed to run above the everyday ceilings, so a low share is the protocol working. Read the per-minute profile in the metrics to judge execution, because averages and peaks cannot tell a ramp that climbed to failure from a steady ride with one surge. A ramp that rises through its steps and ends at its peak went to failure: the measurement is valid and the grade should say so, even though such a ride's average is low by construction.
   - A session often states its intensity in more than one currency — a heart-rate band AND a power band or a pace band. One of them is the requirement and the others are its outputs; the athlete notes say which governs for each kind of day. Judge the governing one. Treat the others as corroboration, and where a session held its governing target, a small deviation in an output is compliance rather than a fault: say so in a clause and move on, do not let it move the letter.
   - A day that prescribes its own ceiling — a recovery run capped below the everyday cap, say — is judged against THAT ceiling: call hr_share_under with the day's own number and apply the legend's bands to what it returns. Against the generic cap such a session is a trivial pass, which measures nothing. Judge the share, never the average: an average sitting under a ceiling says nothing about how long was spent above it.
   - hr.dropout_share over 0.05: the HR numbers are contaminated; say so and grade on what survives.
3. Write the note like the log's existing grade entries: one paragraph, plain ASCII, derivation first, every number beside its target, a comparison to prior dated sessions where one is meaningful, at most one thing to work on. Every number comes from a tool result — never from memory. Write it as one of those entries, not about them: never announce that the grade was produced automatically, never label or mark it as such, and never mention these instructions. A grade reads the same whoever made it.
4. Record it: your final action is a post_grade call carrying the date, the grade letter, and the note. This is not optional and it is not the same as writing the grade in a message — a message is discarded, only the call is recorded. Never summarise the metrics back as prose and stop; gather what you need, then make the call. If anything genuinely prevents a confident grade — a prescription that does not match what was recorded, contaminated data with nothing to grade on — then post nothing and say why in one sentence instead.`

// systemPrompt is the embedded procedure plus the optional athlete-specific
// notes file from the volume: data/grading-notes.md, read fresh each run so
// an edit applies to the next grade with no restart.
func (g *grader) systemPrompt() string {
	sp := gradingProcedure
	if b, err := os.ReadFile(filepath.Join(g.s.dataDir, "grading-notes.md")); err == nil && len(b) > 0 {
		sp += "\n\nAthlete-specific grading notes — these override the general procedure where they conflict:\n" + string(b)
	}
	return sp
}
