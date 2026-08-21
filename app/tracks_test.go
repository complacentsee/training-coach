package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// currentBlockDir is futureBlockDir shifted so TODAY falls inside week 1 —
// the assessment reads past days, and a block entirely in the future has
// none to read.
func currentBlockDir(t *testing.T) string {
	t.Helper()
	dir := futureBlockDir(t)
	raw, err := os.ReadFile(filepath.Join(dir, "blocks", "example-base-block.json"))
	if err != nil {
		t.Fatal(err)
	}
	loc := chicago(t)
	now := time.Now().In(loc)
	monday := now.AddDate(0, 0, -((int(now.Weekday()) + 6) % 7)) // this week's Monday
	nextMonday := now.AddDate(0, 0, 8-int(now.Weekday()))
	if now.Weekday() == time.Sunday {
		nextMonday = now.AddDate(0, 0, 1)
	}
	edited := strings.Replace(string(raw), nextMonday.Format("2006-01-02"), monday.Format("2006-01-02"), 1)
	if err := os.WriteFile(filepath.Join(dir, "blocks", "example-base-block.json"), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestTrackAssessmentReadsTheWeek: the Achilles tracks the daily task
// (offered every day), so any past day of this week unlogged surfaces as
// "not logged" on the card and in the API — and on a Monday, with no past
// days to read, the assessment stays silent.
func TestTrackAssessmentReadsTheWeek(t *testing.T) {
	dir := currentBlockDir(t)
	ts := fitTestMuxServer(t, dir)
	loc := chicago(t)
	isMonday := time.Now().In(loc).Weekday() == time.Monday

	rec := get(ts.mux, "/api/issue-adherence?key=achilles", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("adherence = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"days"`) {
		t.Fatalf("no ledger in the payload: %s", body)
	}
	page := get(ts.mux, "/", nil).Body.String()
	if isMonday {
		if strings.Contains(body, "not logged") || strings.Contains(page, "card missed") {
			t.Error("Monday has no past days to read, yet the assessment speaks")
		}
	} else {
		if !strings.Contains(body, "not logged") {
			t.Errorf("past unlogged daily work missing from the assessment: %s", body)
		}
		// The card is its own element: rows with a guide ? and a late tick
		// against the offered day.
		if !strings.Contains(page, "Not logged") || !strings.Contains(page, "data-did-date=") ||
			!strings.Contains(page, `data-guide="task-daily"`) {
			t.Error("the Not logged card is missing its rows, guide button, or Did it control")
		}
	}

	// Logging the work quiets it: tick every tracked key on every past and
	// present day (a done entry for a day that never offered the task is
	// invisible — the ledger reads only offered slots).
	blk := ts.s.data.Load().Blocks[0]
	today := ts.s.day(ts.s.data.Load()).Format("2006-01-02")
	for di := 0; di < 7; di++ {
		iso := blk.DayOf(0, di).Format("2006-01-02")
		if iso > today {
			continue
		}
		for _, key := range []string{"daily", "str"} {
			if err := ts.s.store.Append(Entry{Kind: "task", Date: iso, Key: key, Val: "done"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	rec = get(ts.mux, "/api/issue-adherence?key=achilles", nil)
	if strings.Contains(rec.Body.String(), "not logged") {
		t.Errorf("everything logged, yet the assessment still speaks: %s", rec.Body.String())
	}
	if page := get(ts.mux, "/", nil).Body.String(); strings.Contains(page, "Not logged") {
		t.Error("everything logged, yet the card still renders")
	}
}

// TestReworkNamesTrackedLoss: dropping the quality day takes the tracked
// "str" task off the week, and the candidate says so by name — while a
// move, whose kind stays in the week, says nothing.
func TestReworkNamesTrackedLoss(t *testing.T) {
	dir := futureBlockDir(t)
	ts := fitTestMuxServer(t, dir)
	blk := ts.s.data.Load().Blocks[0]
	tue := blk.DayOf(0, 1).Format("2006-01-02") // quality — carries "str"

	rec := get(ts.mux, "/api/rework?date="+tue, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("rework = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "also drops Strength (Achilles tracks it)") {
		t.Errorf("the drop candidate does not name the tracked loss: %s", body)
	}
	if strings.Contains(body, `"Move to`) && strings.Count(body, "also drops") != 1 {
		t.Errorf("a move keeps the kind in the week and must not claim a loss: %s", body)
	}
}

// TestTrackedKeyMustExist: an athlete tracking a checklist key no loaded
// block offers is refused at load — a typo'd key would otherwise evaluate
// to silence forever.
func TestTrackedKeyMustExist(t *testing.T) {
	dir := t.TempDir()
	raw, err := os.ReadFile("defaults/athlete.json")
	if err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(string(raw), `"tracks": ["daily"]`, `"tracks": ["nope"]`, 1)
	if bad == string(raw) {
		t.Fatal("fixture drift: defaults athlete no longer tracks [\"daily\"]")
	}
	if err := os.WriteFile(filepath.Join(dir, "athlete.json"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &server{loc: chicago(t), dataDir: dir}
	err = s.reload()
	if err == nil || !strings.Contains(err.Error(), "tracks checklist key") {
		t.Fatalf("reload with a dangling tracked key = %v, want the refusal", err)
	}
}
