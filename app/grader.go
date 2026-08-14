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
}

func graderConfigFromEnv() (graderConfig, error) {
	c := graderConfig{
		Mode:     envOr("GRADER_MODE", "off"),
		Provider: envOr("GRADER_PROVIDER", "anthropic"),
		Dialect:  os.Getenv("GRADER_DIALECT"),
		Model:    os.Getenv("GRADER_MODEL"),
		BaseURL:  os.Getenv("GRADER_BASE_URL"),
		Key:      os.Getenv("GRADER_API_KEY"),
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

type grader struct {
	s     *server
	cfg   graderConfig
	llm   *llmClient
	today func() time.Time // the athlete-timezone day; tests pin it

	mu       sync.Mutex
	inFlight map[string]bool // ISO dates being graded right now
}

func newGrader(s *server, cfg graderConfig) *grader {
	return &grader{
		s: s, cfg: cfg,
		llm: &llmClient{
			HTTP:      &http.Client{Timeout: 180 * time.Second},
			BaseURL:   cfg.BaseURL,
			Key:       cfg.Key,
			Model:     cfg.Model,
			MaxTokens: 2048,
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

	if err := g.grade(m); err != nil {
		log.Printf("grader: %s (%s): FAILED, day left ungraded: %v", m.Name, m.Date, err)
	}
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
	for i := range acts {
		if seen[acts[i].Date] {
			continue
		}
		seen[acts[i].Date] = true
		if g.skipReason(&acts[i]) == "" {
			g.maybeGrade(&acts[i])
		}
	}
}

// gradeTimeout bounds one whole grading run — several provider round
// trips plus tool work; generous, because a local model may need minutes.
const gradeTimeout = 15 * time.Minute

func (g *grader) grade(m *activityMetrics) error {
	ctx, cancel := context.WithTimeout(context.Background(), gradeTimeout)
	defer cancel()

	posted := false
	tools := g.tools(m, &posted)
	turn := g.llm.anthropicTurn
	if g.cfg.Dialect == "openai" {
		turn = g.llm.openaiTurn
	}
	prompt := fmt.Sprintf(
		"Grade the recorded activity %q for %s. Read the prescription and the metrics with the tools, then post exactly one grade.",
		m.Name, m.Date)

	final, err := runLLMLoop(ctx, turn, g.systemPrompt(), prompt, tools)
	if err != nil {
		return err
	}
	if !posted {
		return fmt.Errorf("loop finished without posting: %s", strings.TrimSpace(final))
	}
	log.Printf("grader: %s (%s): done (mode=%s)", m.Name, m.Date, g.cfg.Mode)
	return nil
}

// tools are the loop's whole world: the same payloads the API serves, the
// recent log for context, and one chance to post.
func (g *grader) tools(m *activityMetrics, posted *bool) []llmTool {
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
				rows := []row{}
				for _, e := range g.s.store.All() {
					if e.Date < cutoff {
						continue
					}
					kind := e.Kind
					if k := issueKeyOf(e); k != "" {
						kind = "issue"
					} else if e.Kind != "grade" && e.Kind != "note" {
						continue
					}
					rows = append(rows, row{e.Date, kind, e.Key, e.Val, e.Note})
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
1. get_prescription for the date and get_metrics for the activity. get_recent_entries for context: prior grades and their notes, the athlete's own notes, issue ratings.
2. Decide the grade:
   - Runs: the grading legend's bands applied to grade_input.under_grade_cap_share decide the letter.
   - Bikes: a judgment across the numbers — HR in-band share after the warm-up, average watts against the prescribed band, nothing over the cap, duration close to prescribed. No single threshold.
   - Test days (the day carries a benchmark tag): the rubric does not apply. Grade protocol execution — was the measurement made valid — and say so in the note.
   - hr.dropout_share over 0.05: the HR numbers are contaminated; say so and grade on what survives.
3. Write the note like the log's existing grade entries: one paragraph, plain ASCII, derivation first, every number beside its target, a comparison to prior dated sessions where one is meaningful, at most one thing to work on. Every number comes from a tool result — never from memory.
4. post_grade once, with the date, the letter, and the note. If anything prevents a confident grade — a prescription that does not match what was recorded, contaminated data with nothing to grade on — post nothing and state why instead.`

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
