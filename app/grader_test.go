package main

// The grader's contract, pinned over the embedded defaults: which imports
// trigger a grade at all, what dry mode withholds, what live mode posts and
// verifies, and that config typos are loud. The provider is a scripted
// httptest stub speaking the Anthropic dialect — no network, no model.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/muktihari/fit/encoder"
	"github.com/muktihari/fit/profile/mesgdef"
	"github.com/muktihari/fit/profile/typedef"
	"github.com/muktihari/fit/proto"
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
		got := g.skipReason(&c.m)
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
	if got := g.skipReason(&activityMetrics{Name: "a.fit", Date: "2026-01-13", Sport: "running"}); !strings.Contains(got, "already graded") {
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
	if _, err := g.grade(m); err == nil || !strings.Contains(err.Error(), "without posting") {
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
