package main

// The grader's contract, pinned over the embedded defaults: which imports
// trigger a grade at all, what dry mode withholds, what live mode posts and
// verifies, and that config typos are loud. The provider is a scripted
// httptest stub speaking the Anthropic dialect — no network, no model.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/muktihari/fit/encoder"
	"github.com/muktihari/fit/profile/mesgdef"
	"github.com/muktihari/fit/profile/typedef"
	"github.com/muktihari/fit/proto"
	"os"
	"path/filepath"
)

// Time-parameterized fixture pieces (decode_test.go's are anchored to its
// own fixtureT0; the grader needs dates inside the defaults block).

func mesgdefFileID(t0 time.Time) proto.Message {
	return mesgdef.NewFileId(nil).
		SetType(typedef.FileActivity).
		SetManufacturer(typedef.ManufacturerDevelopment).
		SetTimeCreated(t0).ToMesg(nil)
}

func recordAt(t0 time.Time, sec int, hr uint8, speedRaw uint16, cad uint8) proto.Message {
	return mesgdef.NewRecord(nil).
		SetTimestamp(t0.Add(time.Duration(sec) * time.Second)).
		SetHeartRate(hr).
		SetSpeed(speedRaw).
		SetCadence(cad).ToMesg(nil)
}

func encodeRaw(t *testing.T, msgs []proto.Message) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := encoder.New(&buf).Encode(&proto.FIT{Messages: msgs}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestGraderConfigFromEnv(t *testing.T) {
	set := func(mode, provider, dialect, model, base string) {
		t.Setenv("GRADER_MODE", mode)
		t.Setenv("GRADER_PROVIDER", provider)
		t.Setenv("GRADER_DIALECT", dialect)
		t.Setenv("GRADER_MODEL", model)
		t.Setenv("GRADER_BASE_URL", base)
		t.Setenv("GRADER_API_KEY", "")
	}

	set("", "", "", "", "")
	if c, err := graderConfigFromEnv(); err != nil || c.Mode != "off" {
		t.Fatalf("empty env: %+v %v", c, err)
	}
	set("dry", "", "", "", "")
	c, err := graderConfigFromEnv()
	if err != nil || c.Provider != "anthropic" || c.Model != "claude-opus-5" ||
		c.BaseURL != "https://api.anthropic.com" || c.Dialect != "anthropic" {
		t.Fatalf("anthropic defaults: %+v %v", c, err)
	}
	set("live", "openai", "", "", "http://localhost:11434")
	if _, err := graderConfigFromEnv(); err == nil {
		t.Fatal("openai without a model must refuse")
	}
	set("live", "openai", "anthropic", "qwen3:4b", "http://localhost:11434")
	if c, err := graderConfigFromEnv(); err != nil || c.Dialect != "anthropic" {
		t.Fatalf("ollama anthropic dialect: %+v %v", c, err)
	}
	set("sometimes", "", "", "", "")
	if _, err := graderConfigFromEnv(); err == nil {
		t.Fatal("a typo'd mode must be loud, not silently off")
	}
}

// week2Run encodes a fixture run inside the defaults block's second week
// (the block runs 2026-01-05 → 2026-01-18; Tue 13 Jan is a quality day).
func week2Run(t *testing.T, day int) []byte {
	t.Helper()
	t0 := time.Date(2026, 1, day, 12, 0, 0, 0, time.UTC)
	msgs := []proto.Message{
		mesgdefFileID(t0),
	}
	for i := 0; i <= 10; i++ {
		msgs = append(msgs, recordAt(t0, i, uint8(120+i), 3000, 80))
	}
	msgs = append(msgs, sessionMsg(typedef.SportRunning, 100_00))
	return encodeRaw(t, msgs)
}

func graderUnderTest(t *testing.T, mode, baseURL string) (*grader, testServer) {
	t.Helper()
	ts := fitTestMuxServer(t, t.TempDir())
	g := newGrader(ts.s, graderConfig{
		Mode: mode, Provider: "anthropic", Dialect: "anthropic",
		Model: "m", BaseURL: baseURL,
	})
	// No settle window: a test must not sleep out the wait for a split
	// session's other recordings, and every fixture here is one day's
	// worth already on disk.
	g.settle = 0
	// Pin "today" to Wed 14 Jan 2026, inside the defaults block's week 2 —
	// in UTC, because that is the defaults athlete's declared timezone and
	// the frame the grader parses dates in.
	g.today = func() time.Time {
		return time.Date(2026, 1, 14, 0, 0, 0, 0, time.UTC)
	}
	return g, ts
}

func TestGraderSkipRules(t *testing.T) {
	g, ts := graderUnderTest(t, "dry", "http://unused.invalid")

	cases := []struct {
		m    activityMetrics
		want string // substring of the reason; "" = eligible
	}{
		{activityMetrics{Name: "a.fit", Date: "2026-01-13", Sport: "running"}, ""},
		{activityMetrics{Name: "a.fit", Date: "2026-01-13", Sport: "walking"}, "not graded"},
		{activityMetrics{Name: "a.fit", Date: "2026-01-10", Sport: "running"}, "backfill"},
		{activityMetrics{Name: "a.fit", Date: "2026-01-12", Sport: "running"}, "rest day"},
		{activityMetrics{Name: "a.fit", Date: "2026-01-13", Sport: "cycling"}, "prescribes"},
		{activityMetrics{Name: "a.fit", Date: "2026-03-01", Sport: "running"}, "outside"},
	}
	for _, c := range cases {
		got := g.skipReason(&c.m, false)
		if c.want == "" && got != "" {
			t.Errorf("%s %s: want eligible, got %q", c.m.Date, c.m.Sport, got)
		}
		if c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s %s: reason %q, want ~%q", c.m.Date, c.m.Sport, got, c.want)
		}
	}

	// A posted grade is the idempotency marker.
	if err := ts.s.store.Append(Entry{Date: "2026-01-13", Kind: "grade", Val: "A"}); err != nil {
		t.Fatal(err)
	}
	if got := g.skipReason(&activityMetrics{Name: "a.fit", Date: "2026-01-13", Sport: "running"}, false); !strings.Contains(got, "already graded") {
		t.Errorf("graded day: %q", got)
	}
}

// scriptedProvider fakes the whole grading conversation in the Anthropic
// dialect: prescription → metrics → post_grade → end. It asserts the tool
// results it is fed look like the real payloads.
func scriptedProvider(t *testing.T, date, name string) *httptest.Server {
	t.Helper()
	turns := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			System   string `json:"system"`
			Messages []struct {
				Role    string `json:"role"`
				Content []struct {
					Type    string `json:"type"`
					Content any    `json:"content"`
				} `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		turns++
		switch turns {
		case 1:
			if !strings.Contains(body.System, "post exactly one grade") {
				t.Errorf("system prompt missing the procedure")
			}
			w.Write([]byte(`{"content":[{"type":"tool_use","id":"t1","name":"get_prescription","input":{"date":"` + date + `"}}],"stop_reason":"tool_use"}`))
		case 2:
			last := body.Messages[len(body.Messages)-1]
			res, _ := last.Content[0].Content.(string)
			if !strings.Contains(res, `"session"`) || !strings.Contains(res, `"quality"`) {
				t.Errorf("prescription result: %.120s", res)
			}
			w.Write([]byte(`{"content":[{"type":"tool_use","id":"t2","name":"get_metrics","input":{"name":"` + name + `"}}],"stop_reason":"tool_use"}`))
		case 3:
			last := body.Messages[len(body.Messages)-1]
			res, _ := last.Content[0].Content.(string)
			if !strings.Contains(res, `"hr"`) || !strings.Contains(res, `"avg":125.5`) {
				t.Errorf("metrics result: %.120s", res)
			}
			w.Write([]byte(`{"content":[{"type":"tool_use","id":"t3","name":"post_grade","input":{"date":"` + date + `","val":"B","note":"Scripted note with the numbers beside their targets."}}],"stop_reason":"tool_use"}`))
		default:
			w.Write([]byte(`{"content":[{"type":"text","text":"graded."}],"stop_reason":"end_turn"}`))
		}
	}))
}

// stallingProvider gathers its facts and then writes its conclusion as
// prose instead of calling post_grade — measured behaviour from the
// server's local 4B. It complies after `stalls` reminders; a provider that
// never complies is the same script with stalls larger than the nudge cap.
func stallingProvider(t *testing.T, date, name string, stalls int) *httptest.Server {
	t.Helper()
	stalled := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content []struct {
					Type    string `json:"type"`
					Text    string `json:"text"`
					Content any    `json:"content"`
				} `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		seen := 0
		for _, m := range body.Messages {
			for _, b := range m.Content {
				if b.Type == "tool_result" {
					seen++
				}
			}
		}
		switch {
		case seen == 0:
			w.Write([]byte(`{"content":[{"type":"tool_use","id":"t1","name":"get_prescription","input":{"date":"` + date + `"}}],"stop_reason":"tool_use"}`))
		case seen == 1:
			w.Write([]byte(`{"content":[{"type":"tool_use","id":"t2","name":"get_metrics","input":{"name":"` + name + `"}}],"stop_reason":"tool_use"}`))
		case stalled < stalls:
			stalled++
			w.Write([]byte(`{"content":[{"type":"text","text":"Here are the key metrics for the session: avg power 119 W."}],"stop_reason":"end_turn"}`))
		default:
			w.Write([]byte(`{"content":[{"type":"tool_use","id":"t3","name":"post_grade","input":{"date":"` + date + `","val":"C","note":"Posted after the reminder."}}],"stop_reason":"tool_use"}`))
		}
	}))
}

// TestGraderNudgesAStalledModel: narrating the analysis instead of calling
// post_grade must not lose the grade — the model is reminded that only the
// call records anything, and one reminder is enough.
func TestGraderNudgesAStalledModel(t *testing.T) {
	const name, date = "2026-01-13-12-00-00.fit", "2026-01-13"
	srv := stallingProvider(t, date, name, 1)
	defer srv.Close()
	g, ts := graderUnderTest(t, "live", srv.URL)

	m, err := ts.s.metrics.importOne(name, week2Run(t, 13), time.UTC, nil)
	if err != nil {
		t.Fatal(err)
	}
	g.maybeGrade(m)

	grade, ok := ts.s.store.Grades()[date]
	if !ok || grade.Val != "C" || !strings.Contains(grade.Note, "after the reminder") {
		t.Fatalf("stalled model's grade never landed: ok=%v %+v", ok, grade)
	}
}

// TestGraderNeverPostsPartially: a model that will not make the call, no
// matter how often it is reminded, leaves the day ungraded. Prose is not a
// grade, and the failure is loud rather than half-recorded.
func TestGraderNeverPostsPartially(t *testing.T) {
	const name, date = "2026-01-13-12-00-00.fit", "2026-01-13"
	srv := stallingProvider(t, date, name, 99)
	defer srv.Close()
	g, ts := graderUnderTest(t, "live", srv.URL)

	m, err := ts.s.metrics.importOne(name, week2Run(t, 13), time.UTC, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.grade(m, ""); err == nil || !strings.Contains(err.Error(), "without posting") {
		t.Fatalf("err = %v, want a refusal naming the missing post", err)
	}
	if _, ok := ts.s.store.Grades()[date]; ok {
		t.Error("a day was graded without the grade ever being posted")
	}
	if n := len(ts.s.store.All()); n != 0 {
		t.Errorf("%d entries written by a run that never posted", n)
	}
}

func TestGraderEndToEnd(t *testing.T) {
	const name = "2026-01-13-12-00-00.fit"
	const date = "2026-01-13"

	for _, mode := range []string{"dry", "live"} {
		t.Run(mode, func(t *testing.T) {
			srv := scriptedProvider(t, date, name)
			defer srv.Close()
			g, ts := graderUnderTest(t, mode, srv.URL)

			m, err := ts.s.metrics.importOne(name, week2Run(t, 13), time.UTC, nil)
			if err != nil {
				t.Fatal(err)
			}
			g.maybeGrade(m)

			grade, ok := ts.s.store.Grades()[date]
			if mode == "dry" {
				if ok {
					t.Fatalf("dry mode posted a grade: %+v", grade)
				}
				return
			}
			if !ok || grade.Val != "B" || !strings.Contains(grade.Note, "Scripted note") {
				t.Fatalf("live mode grade: ok=%v %+v", ok, grade)
			}
			// The posted grade is the idempotency marker: a second import
			// of the same day must not grade again (the provider would
			// explode the turn count if consulted).
			g.maybeGrade(m)
			if all := ts.s.store.All(); len(all) != 1 {
				t.Fatalf("re-grade appended: %d entries", len(all))
			}
		})
	}
}

// TestGradingNotesAreBounded: the athlete overlay is the one input to the
// system prompt with no natural ceiling — a hand-edited file, read fresh on
// every run, appended verbatim. A runaway edit or a half-finished write would
// otherwise push the measured numbers out of a small model's context, and the
// grade that came back would read perfectly while being made on nothing.
func TestGradingNotesAreBounded(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) {
		if err := os.WriteFile(filepath.Join(dir, "grading-notes.md"), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	graderOver := func(max int) *grader {
		return &grader{s: &server{dataDir: dir}, cfg: graderConfig{NotesMax: max}}
	}

	// Under the limit: present in full, untouched.
	write("first rule\nsecond rule\nlast rule\n")
	sp := graderOver(defaultNotesMax).systemPrompt()
	for _, want := range []string{"first rule", "second rule", "last rule"} {
		if !strings.Contains(sp, want) {
			t.Errorf("a file well under the limit lost %q", want)
		}
	}
	if !strings.Contains(sp, "You are the training log's automated workout grader") {
		t.Error("the embedded procedure is missing from the prompt")
	}

	// Over it: cut, and cut on a line boundary so no rule is half-quoted.
	write("keep this line\n" + strings.Repeat("x", 500) + "\nDO NOT GRADE A TEST DAY ON THE SHARE\n")
	sp = graderOver(40).systemPrompt()
	if !strings.Contains(sp, "keep this line") {
		t.Error("the truncation dropped what fitted")
	}
	if strings.Contains(sp, "DO NOT GRADE A TEST DAY ON THE SHARE") {
		t.Error("a rule past the limit reached the prompt")
	}
	if i := strings.Index(sp, "keep this line"); i >= 0 && strings.Contains(sp[i:], "xxxx") {
		t.Error("cut mid-line; half a rule can read as a different rule")
	}

	// The limit is configurable, because a roomier model can take more.
	t.Setenv("GRADER_MODE", "off")
	t.Setenv("GRADER_NOTES_MAX", "")
	if c, err := graderConfigFromEnv(); err != nil || c.NotesMax != defaultNotesMax {
		t.Errorf("unset should default to %d, got %d (%v)", defaultNotesMax, c.NotesMax, err)
	}
	t.Setenv("GRADER_NOTES_MAX", "65536")
	if c, err := graderConfigFromEnv(); err != nil || c.NotesMax != 65536 {
		t.Errorf("override ignored: %d (%v)", c.NotesMax, err)
	}
	// A typo is fatal, like every other GRADER_*: a silently wrong bound
	// would quietly amputate the rules.
	for _, bad := range []string{"lots", "0", "-1", "16k"} {
		t.Setenv("GRADER_NOTES_MAX", bad)
		if _, err := graderConfigFromEnv(); err == nil {
			t.Errorf("GRADER_NOTES_MAX=%q was accepted", bad)
		}
	}
}

/* ── the day, not the file ──────────────────────────────────────────────── */

// runPart is one recording of a split session: a run of secs seconds and
// distM metres, started at the given hour on 13 Jan 2026.
func runPart(t *testing.T, hour, secs int, distM float64) []byte {
	t.Helper()
	t0 := time.Date(2026, 1, 13, hour, 0, 0, 0, time.UTC)
	msgs := []proto.Message{mesgdefFileID(t0)}
	for i := 0; i <= secs; i++ {
		msgs = append(msgs, recordAt(t0, i, 130, 3000, 80))
	}
	msgs = append(msgs, sessionMsg(typedef.SportRunning, uint32(distM*100)))
	return encodeRaw(t, msgs)
}

// splitDay imports a warm-up, an effort and a cool-down as three separate
// recordings of one training day, and returns them oldest first.
func splitDay(t *testing.T, ts testServer) []*activityMetrics {
	t.Helper()
	parts := []struct {
		name       string
		hour, secs int
		distM      float64
	}{
		{"2026-01-13-08-00-00.fit", 8, 600, 3200},  // warm-up
		{"2026-01-13-08-30-00.fit", 8, 1200, 5000}, // the effort
		{"2026-01-13-09-30-00.fit", 9, 500, 2400},  // cool-down
	}
	var out []*activityMetrics
	for _, p := range parts {
		m, err := ts.s.metrics.importOne(p.name, runPart(t, p.hour, p.secs, p.distM), time.UTC, nil)
		if err != nil {
			t.Fatalf("%s: %v", p.name, err)
		}
		out = append(out, m)
	}
	return out
}

// TestSplitSessionIsGradedAsOneDay: a warm-up, an effort and a cool-down
// recorded separately are ONE session. The prompt has to name all three, in
// the order they were run, with the day's totals — the tools can already
// answer for any filename, so naming them is the whole of what was missing.
func TestSplitSessionIsGradedAsOneDay(t *testing.T) {
	g, ts := graderUnderTest(t, "dry", "http://unused.invalid")
	parts := splitDay(t, ts)

	prompt := g.gradePrompt(parts[0], "")
	for _, name := range []string{
		"2026-01-13-08-00-00.fit", "2026-01-13-08-30-00.fit", "2026-01-13-09-30-00.fit",
	} {
		if !strings.Contains(prompt, name) {
			t.Errorf("prompt never names %s:\n%s", name, prompt)
		}
	}
	// Chronological, because "the first one" and "the last one" are how the
	// procedure tells a warm-up from an effort.
	iWarm := strings.Index(prompt, "2026-01-13-08-00-00.fit")
	iWork := strings.Index(prompt, "2026-01-13-08-30-00.fit")
	iCool := strings.Index(prompt, "2026-01-13-09-30-00.fit")
	if !(iWarm < iWork && iWork < iCool) {
		t.Errorf("recordings out of order (%d, %d, %d):\n%s", iWarm, iWork, iCool, prompt)
	}
	// The day's totals are computed here, not left to a language model
	// adding up miles to decide whether the session was completed.
	// In the ATHLETE's units — the defaults athlete is metric, and a day
	// total spelled in miles for someone who thinks in kilometres is the
	// same bug as hand-copying a figure.
	if !strings.Contains(prompt, "10.6 km") { // 3200 + 5000 + 2400 m
		t.Errorf("prompt carries no day total:\n%s", prompt)
	}
	if !strings.Contains(prompt, "38:20") { // 600 + 1200 + 500 s, plus a second each
		t.Errorf("prompt carries no total duration:\n%s", prompt)
	}
}

// TestOneRecordingKeepsItsOldPrompt: the corpus is graded against that
// sentence. A day with a single recording must be handed in with the
// wording it has always had, to the byte — a prompt change is a behaviour
// change even when it reads like a synonym.
func TestOneRecordingKeepsItsOldPrompt(t *testing.T) {
	g, ts := graderUnderTest(t, "dry", "http://unused.invalid")
	const name = "2026-01-13-12-00-00.fit"
	m, err := ts.s.metrics.importOne(name, week2Run(t, 13), time.UTC, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := `Grade the recorded activity "2026-01-13-12-00-00.fit" for 2026-01-13. ` +
		`Read the prescription and the metrics with the tools, then post exactly one grade.`
	if got := g.gradePrompt(m, ""); got != want {
		t.Errorf("single-recording prompt drifted:\n got %q\nwant %q", got, want)
	}
}

// TestAWalkIsNotPartOfTheRun: "every appropriate recording" means the ones
// belonging to the day's session. A walk on a run day is a walk.
func TestAWalkIsNotPartOfTheRun(t *testing.T) {
	g, ts := graderUnderTest(t, "dry", "http://unused.invalid")
	if _, err := ts.s.metrics.importOne("2026-01-13-08-00-00.fit", runPart(t, 8, 600, 3200), time.UTC, nil); err != nil {
		t.Fatal(err)
	}
	walk := encodeRaw(t, append(
		[]proto.Message{mesgdefFileID(time.Date(2026, 1, 13, 17, 0, 0, 0, time.UTC))},
		recordAt(time.Date(2026, 1, 13, 17, 0, 0, 0, time.UTC), 0, 90, 500, 50),
		sessionMsg(typedef.SportWalking, 200_000)))
	if _, err := ts.s.metrics.importOne("2026-01-13-17-00-00.fit", walk, time.UTC, nil); err != nil {
		t.Fatal(err)
	}
	sess, ok := g.sessionOn("2026-01-13")
	if !ok {
		t.Fatal("no session on the pinned day")
	}
	acts, err := g.dayRecordings("2026-01-13", sess.Kind)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 1 || acts[0].Name != "2026-01-13-08-00-00.fit" {
		t.Errorf("the walk was counted as part of the run: %+v", acts)
	}
}

// TestTheLastRecordingGradesTheDay: the settle window. The warm-up lands
// first and must stand down — grading on arrival would judge a whole time
// trial by its warm-up and then lock the effort out, because the posted
// grade is the idempotency marker.
func TestTheLastRecordingGradesTheDay(t *testing.T) {
	const date = "2026-01-13"
	var mu sync.Mutex
	var graded []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// A RUN is a conversation that starts with one message. Counting
		// requests would count the loop's later turns as extra runs.
		var turn struct {
			Messages []struct {
				Content any `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &turn)
		if len(turn.Messages) == 1 {
			mu.Lock()
			graded = append(graded, string(body))
			mu.Unlock()
		}
		// End the loop: this test is about WHICH import grades the day, not
		// about the conversation.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"no"}],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()

	g, ts := graderUnderTest(t, "dry", srv.URL)
	g.settle = 150 * time.Millisecond
	parts := splitDay(t, ts)

	var wg sync.WaitGroup
	for _, m := range parts {
		wg.Add(1)
		go func(m *activityMetrics) { defer wg.Done(); g.maybeGrade(m) }(m)
		time.Sleep(20 * time.Millisecond) // a transfer delivers them in order
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(graded) != 1 {
		t.Fatalf("want exactly one grading run for %s, got %d", date, len(graded))
	}
	// And it is the run that knows about all three recordings.
	for _, name := range []string{"08-00-00", "08-30-00", "09-30-00"} {
		if !strings.Contains(graded[0], name) {
			t.Errorf("the surviving run never saw %s", name)
		}
	}
}

// TestRegradeSupersedesAStaleVerdict: the human override. A day that is
// already graded, and old enough that no import would ever trigger it
// again, is exactly what a re-grade is for — so force waives those two
// rules and only those two. The new grade is a LATER entry: the log is
// append-only, and the earlier verdict stays in the record.
func TestRegradeSupersedesAStaleVerdict(t *testing.T) {
	const name, date = "2026-01-13-12-00-00.fit", "2026-01-13"
	srv := scriptedProvider(t, date, name)
	defer srv.Close()
	g, ts := graderUnderTest(t, "live", srv.URL)
	ts.s.grader = g

	if _, err := ts.s.metrics.importOne(name, week2Run(t, 13), time.UTC, nil); err != nil {
		t.Fatal(err)
	}
	if err := ts.s.store.Append(Entry{Date: date, Kind: "grade", Val: "F", Note: "the stale one"}); err != nil {
		t.Fatal(err)
	}
	// The automatic path is now closed for this day, twice over.
	m := &activityMetrics{Name: name, Date: date, Sport: "running"}
	if got := g.skipReason(m, false); !strings.Contains(got, "already graded") {
		t.Fatalf("fixture: want the day closed, got %q", got)
	}
	if got := g.skipReason(&activityMetrics{Name: name, Date: "2026-01-10", Sport: "running"}, true); got != "" {
		t.Errorf("force did not waive the backfill rule: %q", got)
	}

	rec := post(ts.mux, "/api/regrade?date="+date, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("regrade = %d %q", rec.Code, rec.Body.String())
	}
	// It runs in the background; wait for the superseding entry.
	deadline := time.Now().Add(3 * time.Second)
	var grade Entry
	for time.Now().Before(deadline) {
		if gr, ok := ts.s.store.Grades()[date]; ok && gr.Val != "F" {
			grade = gr
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if grade.Val == "" {
		t.Fatal("the re-grade never posted")
	}
	awaitIdle(t, g, date)
	// Append-only: the stale verdict is still in the log, just no longer
	// the answer.
	var grades int
	for _, e := range ts.s.store.All() {
		if e.Kind == "grade" && e.Date == date {
			grades++
		}
	}
	if grades != 2 {
		t.Errorf("want the old grade kept and a new one appended, got %d grade entries", grades)
	}
}

// TestRegradeRefusesWhatItCannotGrade: force waives the rules that exist to
// hold the AUTOMATIC path back. It does not invent a session where there is
// none, and each refusal carries a status that says which kind it is.
func TestRegradeRefusesWhatItCannotGrade(t *testing.T) {
	g, ts := graderUnderTest(t, "dry", "http://unused.invalid")
	ts.s.grader = g
	const name = "2026-01-13-12-00-00.fit"
	body := week2Run(t, 13)
	if _, err := ts.s.metrics.importOne(name, body, time.UTC, nil); err != nil {
		t.Fatal(err)
	}
	// The page payload reads the archived bytes, not the metrics row.
	if err := os.MkdirAll(ts.s.activitiesDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ts.s.activitiesDir(), name), body, 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		date string
		code int
		want string
	}{
		{"2026-01-13", http.StatusAccepted, ""},
		{"2026-01-12", http.StatusBadRequest, "rest day"},         // rest day, nothing recorded
		{"2026-01-14", http.StatusBadRequest, "nothing matching"}, // a session, no recording
		{"2026-03-01", http.StatusNotFound, "outside"},            // no such day in the block
		{"not-a-date", http.StatusBadRequest, "YYYY-MM-DD"},
	}
	for _, c := range cases {
		rec := post(ts.mux, "/api/regrade?date="+c.date, nil)
		if rec.Code != c.code {
			t.Errorf("%s: %d %q, want %d", c.date, rec.Code, rec.Body.String(), c.code)
		}
		if c.want != "" && !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s: %q, want ~%q", c.date, rec.Body.String(), c.want)
		}
	}

	awaitIdle(t, g, "2026-01-13")

	// Grading switched off is not a client error, and the page must not
	// offer a button that can only fail.
	g.cfg.Mode = "off"
	if rec := post(ts.mux, "/api/regrade?date=2026-01-13", nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("mode=off: %d %q", rec.Code, rec.Body.String())
	}
	out, code, _ := ts.s.detailPayload(name, "")
	if code != http.StatusOK {
		t.Fatalf("detail payload: %d", code)
	}
	if out.CanRegrade {
		t.Error("the page was offered a re-grade with grading switched off")
	}
	g.cfg.Mode = "dry"
	out, _, _ = ts.s.detailPayload(name, "")
	if !out.CanRegrade {
		t.Error("the page was not offered a re-grade with grading on")
	}
}

// awaitIdle waits for any background grading of these dates to finish. A
// re-grade returns 202 and runs on, so a test that returns without waiting
// races its own cleanup: the scripted provider closes, the run's next turn
// is refused, and TempDir removal trips over a store still being written.
// Measured as a flake under parallel load, 16 Aug 2026.
func awaitIdle(t *testing.T, g *grader, dates ...string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for _, date := range dates {
		started := false
		for time.Now().Before(deadline) {
			g.mu.Lock()
			busy := g.inFlight[date]
			g.mu.Unlock()
			if busy {
				started = true
			} else if started {
				break
			} else if _, done := g.s.store.Grades()[date]; done {
				break // it ran and finished between two looks
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
}

// TestWhatTheAthleteSaysIsRecorded: a re-grade may carry the athlete's own
// account of the day, and it goes in the LOG rather than into a prompt and
// out of existence — "note" is already the kind for free text addressed to
// the coach, so every later reading of that day sees it, including a human
// one. The run is also told directly, because a re-grade is usually asked
// for BECAUSE the last grade missed something.
func TestWhatTheAthleteSaysIsRecorded(t *testing.T) {
	const name, date = "2026-01-13-12-00-00.fit", "2026-01-13"
	const said = "The chain came off at mile 3 and the trainer would not re-engage."
	srv := scriptedProvider(t, date, name)
	defer srv.Close()
	g, ts := graderUnderTest(t, "live", srv.URL)
	ts.s.grader = g
	m, err := ts.s.metrics.importOne(name, week2Run(t, 13), time.UTC, nil)
	if err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]string{"note": said})
	if rec := post(ts.mux, "/api/regrade?date="+date, body); rec.Code != http.StatusAccepted {
		t.Fatalf("regrade = %d %q", rec.Code, rec.Body.String())
	}

	// In the log, immediately — before the grade it informs.
	var found bool
	for _, e := range ts.s.store.All() {
		if e.Kind == "note" && e.Date == date && e.Note == said {
			found = true
		}
	}
	awaitIdle(t, g, date)
	if !found {
		t.Error("what the athlete said was never recorded")
	}
	// And in the run's own prompt, quoted, not paraphrased.
	prompt := g.gradePrompt(m, said)
	if !strings.Contains(prompt, said) {
		t.Errorf("the run was never told:\n%s", prompt)
	}
	// Testimony, not instruction: it must not be handed over as a verdict.
	if !strings.Contains(prompt, "does not decide the grade by itself") {
		t.Errorf("the note was passed without its framing:\n%s", prompt)
	}
	// A day with one recording and nothing said keeps the byte-identical
	// prompt the corpus is measured against.
	if strings.Contains(g.gradePrompt(m, ""), "athlete asked") {
		t.Error("an empty note still changed the prompt")
	}

	// Too long is refused before anything is written.
	long, _ := json.Marshal(map[string]string{"note": strings.Repeat("x", regradeNoteMax+1)})
	rec := post(ts.mux, "/api/regrade?date="+date, long)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversized note = %d %q", rec.Code, rec.Body.String())
	}
	for _, e := range ts.s.store.All() {
		if e.Kind == "note" && strings.Contains(e.Note, "xxxx") {
			t.Error("a refused note was written to the log anyway")
		}
	}
}

// TestTheDayCarriesItsCurrentGrade: the popover is opened from a calendar
// cell rendered whenever the page loaded, so the payload has to carry the
// verdict as it stands NOW — and graded_at with it, because a re-grade can
// land on the same letter and only the timestamp says it is a new one.
func TestTheDayCarriesItsCurrentGrade(t *testing.T) {
	ts := fitTestMuxServer(t, t.TempDir())
	const name, date = "2026-01-13-12-00-00.fit", "2026-01-13"
	body := week2Run(t, 13)
	if _, err := ts.s.metrics.importOne(name, body, time.UTC, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ts.s.activitiesDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ts.s.activitiesDir(), name), body, 0o644); err != nil {
		t.Fatal(err)
	}

	out, code, _ := ts.s.detailPayload(name, "")
	if code != http.StatusOK {
		t.Fatalf("detail: %d", code)
	}
	if out.Grade != "" || out.GradedAt != "" {
		t.Errorf("an ungraded day claimed a grade: %q at %q", out.Grade, out.GradedAt)
	}

	if err := ts.s.store.Append(Entry{Date: date, Kind: "grade", Val: "B", Note: "first"}); err != nil {
		t.Fatal(err)
	}
	out, _, _ = ts.s.detailPayload(name, "")
	first := out.GradedAt
	if out.Grade != "B" || out.GradeNote != "first" || first == "" {
		t.Fatalf("grade not served: %+v", out)
	}

	// A re-grade to the SAME letter must still be detectable.
	time.Sleep(1100 * time.Millisecond) // the log stamps whole seconds
	if err := ts.s.store.Append(Entry{Date: date, Kind: "grade", Val: "B", Note: "second"}); err != nil {
		t.Fatal(err)
	}
	out, _, _ = ts.s.detailPayload(name, "")
	if out.GradeNote != "second" {
		t.Errorf("the superseding grade was not served: %q", out.GradeNote)
	}
	if out.GradedAt == first {
		t.Error("graded_at did not move, so the page cannot see a same-letter re-grade")
	}
}
