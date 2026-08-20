package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The defaults block (2026-01-05, two weeks) reads, Monday-first:
// rest, quality(steps), easy, rest, easy, long, rest — both weeks.
// Dates: wk1 Mon 01-05 … Sun 01-11; wk2 Mon 01-12 … Sun 01-18.

func TestAmendmentsReplay(t *testing.T) {
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	must := func(e Entry) {
		if err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	must(Entry{Kind: kindAmend, Date: "2026-01-07", Key: "move", Val: "2026-01-08"})
	must(Entry{Kind: kindAmend, Date: "2026-01-09", Key: "cancel"})
	// Re-amending a date supersedes…
	must(Entry{Kind: kindAmend, Date: "2026-01-07", Key: "cancel"})
	// …and a revoke stands it down entirely.
	must(Entry{Kind: kindAmend, Date: "2026-01-09", Key: "revoke"})
	got := s.Amendments()
	if len(got) != 1 {
		t.Fatalf("standing amendments = %d, want 1: %+v", len(got), got)
	}
	if got[0].Date != "2026-01-07" || got[0].Key != "cancel" {
		t.Fatalf("winner = %s %s, want the superseding cancel on 2026-01-07", got[0].Date, got[0].Key)
	}
}

func TestAmendCheckRefusals(t *testing.T) {
	ts := fitTestMuxServer(t, "")
	d := ts.s.data.Load()
	none := map[string]amendInfo{}
	cases := []struct {
		name   string
		op     amendOp
		taken  map[string]amendInfo
		reason string
	}{
		{"rest source", amendOp{Date: "2026-01-05", Op: "cancel"}, none, "rest day"},
		{"steps source", amendOp{Date: "2026-01-06", Op: "cancel"}, none, "structured workout"},
		{"outside block", amendOp{Date: "2025-06-01", Op: "cancel"}, none, "outside any block"},
		{"unknown op", amendOp{Date: "2026-01-07", Op: "teleport"}, none, "unknown op"},
		{"cross week", amendOp{Date: "2026-01-07", Op: "move", Arg: "2026-01-15"}, none, "outside the week"},
		{"dest not rest", amendOp{Date: "2026-01-07", Op: "move", Arg: "2026-01-09"}, none, "not a rest day"},
		{"dest is source", amendOp{Date: "2026-01-07", Op: "move", Arg: "2026-01-07"}, none, "destination is the source"},
		{"no tag to strip", amendOp{Date: "2026-01-07", Op: "plain"}, none, "no benchmark tag"},
		{"source already amended", amendOp{Date: "2026-01-07", Op: "cancel"},
			map[string]amendInfo{"2026-01-07": {Role: "vacated"}}, "already amended"},
		{"dest already amended", amendOp{Date: "2026-01-07", Op: "move", Arg: "2026-01-08"},
			map[string]amendInfo{"2026-01-08": {Role: "landed"}}, "destination already amended"},
		{"valid move", amendOp{Date: "2026-01-07", Op: "move", Arg: "2026-01-08"}, none, ""},
		{"valid cancel", amendOp{Date: "2026-01-07", Op: "cancel"}, none, ""},
	}
	for _, c := range cases {
		got := amendCheck(d.Blocks, d.Loc, c.taken, c.op)
		if c.reason == "" && got != "" {
			t.Errorf("%s: refused %q, want accepted", c.name, got)
		}
		if c.reason != "" && !strings.Contains(got, c.reason) {
			t.Errorf("%s: reason %q, want it to mention %q", c.name, got, c.reason)
		}
	}
}

func TestEffectiveMoveRendersEverywhere(t *testing.T) {
	ts := fitTestMuxServer(t, "")
	s := ts.s
	authored := s.data.Load()

	// Wed wk1 (easy) moves to Thu wk1 (rest), appended directly — the POST
	// gate is tested separately; this is the materialisation.
	if err := s.store.Append(Entry{Kind: kindAmend, Date: "2026-01-07", Key: "move", Val: "2026-01-08", Note: "work conflict"}); err != nil {
		t.Fatal(err)
	}

	eff := s.ds()
	if eff == authored {
		t.Fatal("ds() returned the authored dataset despite a standing amendment")
	}
	if eff.Rev != authored.Rev {
		t.Errorf("effective Rev %s != authored %s — the overlay must not rotate identity", eff.Rev, authored.Rev)
	}

	loc := authored.Loc
	day := func(iso string) time.Time {
		tm, _ := time.ParseInLocation("2006-01-02", iso, loc)
		return tm
	}
	blk := eff.Blocks[0]
	wk, di, _ := blk.Locate(day("2026-01-07"))
	if wk.Days[di].Kind != KindRest {
		t.Errorf("vacated day kind = %s, want rest", wk.Days[di].Kind)
	}
	wk, di, _ = blk.Locate(day("2026-01-08"))
	if wk.Days[di].Kind != KindEasy {
		t.Errorf("landed day kind = %s, want easy", wk.Days[di].Kind)
	}
	// The authored dataset is untouched.
	awk, adi, _ := authored.Blocks[0].Locate(day("2026-01-07"))
	if awk.Days[adi].Kind != KindEasy {
		t.Errorf("authored day mutated to %s — the clone leaked", awk.Days[adi].Kind)
	}
	// A same-week move keeps the volume; the aggregates read the effective days.
	if a, e := authored.Blocks[0].Weeks[0].Volume(), blk.Weeks[0].Volume(); a != e {
		t.Errorf("same-week move changed volume %v -> %v", a, e)
	}

	if _, ok := s.amendInfoFor("2026-01-07"); !ok {
		t.Error("no amendInfo for the vacated date")
	}
	if info, ok := s.amendInfoFor("2026-01-08"); !ok || info.Role != "landed" {
		t.Errorf("landed info = %+v, %v", info, ok)
	}

	// The grader's own payload sees the effective plan.
	rec := get(ts.mux, "/api/day?date=2026-01-07", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/day = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"rest"`) {
		t.Errorf("vacated day's /api/day does not read rest: %s", rec.Body.String())
	}

	// The calendar carries the ghost.
	rec = get(ts.mux, "/calendar", nil)
	if !strings.Contains(rec.Body.String(), "→ Thu 8") {
		t.Errorf("calendar carries no ghost arrow for the vacated cell")
	}
}

func TestEffectiveCancelDropsVolume(t *testing.T) {
	ts := fitTestMuxServer(t, "")
	s := ts.s
	authored := s.data.Load()
	if err := s.store.Append(Entry{Kind: kindAmend, Date: "2026-01-10", Key: "cancel", Note: "sick"}); err != nil {
		t.Fatal(err)
	}
	a := authored.Blocks[0].Weeks[0].Volume()
	e := s.ds().Blocks[0].Weeks[0].Volume()
	if e >= a {
		t.Errorf("cancelled long run left volume %v (authored %v) — aggregates must derive from the effective days", e, a)
	}
}

func TestVoidedAmendmentNeverBreaksServing(t *testing.T) {
	ts := fitTestMuxServer(t, "")
	s := ts.s
	// An amendment for a date no loaded block contains: voided, not fatal.
	if err := s.store.Append(Entry{Kind: kindAmend, Date: "2031-01-01", Key: "cancel"}); err != nil {
		t.Fatal(err)
	}
	d := s.ds()
	if d == nil {
		t.Fatal("ds() nil")
	}
	if _, ok := s.amendInfoFor("2031-01-01"); ok {
		t.Error("a voided amendment is still reported as standing")
	}
	if rec := get(ts.mux, "/calendar", nil); rec.Code != http.StatusOK {
		t.Errorf("calendar = %d with a voided amendment in the log", rec.Code)
	}
}

func postJSON(mux *http.ServeMux, url string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestPostAmendGates(t *testing.T) {
	ts := fitTestMuxServer(t, "")
	amend := func(body any) *httptest.ResponseRecorder {
		return postJSON(ts.mux, "/api/amend", body)
	}
	type req struct {
		Date string `json:"date"`
		Op   string `json:"op"`
		Arg  string `json:"arg"`
		Note string `json:"note"`
	}
	if rec := amend(req{Date: "2026-01-06", Op: "cancel"}); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "structured workout") {
		t.Errorf("steps day: %d %s", rec.Code, rec.Body.String())
	}
	if rec := amend(req{Date: "2026-01-07", Op: "revoke"}); rec.Code != http.StatusBadRequest {
		t.Errorf("revoke with nothing standing: %d", rec.Code)
	}
	// The defaults block is entirely in the past, so a structurally valid
	// move is refused by the destination-in-the-past gate — and nothing is
	// appended by a refusal.
	if rec := amend(req{Date: "2026-01-07", Op: "move", Arg: "2026-01-08"}); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "past") {
		t.Errorf("past destination: %d %s", rec.Code, rec.Body.String())
	}
	if got := len(ts.s.store.Amendments()); got != 0 {
		t.Errorf("%d amendments standing after refusals — a refusal must write nothing", got)
	}
	// A graded day refuses before the past gate.
	if err := ts.s.store.Append(Entry{Kind: "grade", Date: "2026-01-07", Val: "B", Note: "n"}); err != nil {
		t.Fatal(err)
	}
	if rec := amend(req{Date: "2026-01-07", Op: "move", Arg: "2026-01-08"}); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "graded") {
		t.Errorf("graded source: %d %s", rec.Code, rec.Body.String())
	}
}

// futureBlockDir writes a volume whose block is the defaults block re-dated
// to start next Monday, so the whole POST path — including the past gate —
// can run forward in time. The library and athlete stay embedded.
func futureBlockDir(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("defaults/blocks/example-base-block.json")
	if err != nil {
		t.Fatal(err)
	}
	var blk map[string]any
	if err := json.Unmarshal(raw, &blk); err != nil {
		t.Fatal(err)
	}
	loc := chicago(t)
	now := time.Now().In(loc)
	monday := now.AddDate(0, 0, 8-int(now.Weekday())) // next week's Monday
	if now.Weekday() == time.Sunday {
		monday = now.AddDate(0, 0, 1)
	}
	blk["start"] = monday.Format("2006-01-02")
	out, err := json.Marshal(blk)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "blocks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blocks", "example-base-block.json"), append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPostAmendAppliesAndRevokes(t *testing.T) {
	dir := futureBlockDir(t)
	ts := fitTestMuxServer(t, dir)
	d := ts.s.data.Load()
	blk := d.Blocks[0]
	wed := blk.DayOf(0, 2).Format("2006-01-02") // easy
	thu := blk.DayOf(0, 3).Format("2006-01-02") // rest

	rec := postJSON(ts.mux, "/api/amend", map[string]string{
		"date": wed, "op": "move", "arg": thu, "note": "work conflict"})
	if rec.Code != http.StatusOK {
		t.Fatalf("apply = %d: %s", rec.Code, rec.Body.String())
	}
	info, ok := ts.s.amendInfoFor(wed)
	if !ok || info.Role != "vacated" || info.Other != thu {
		t.Fatalf("standing after apply = %+v, %v", info, ok)
	}

	// The rework payload reports the standing amendment…
	rr := get(ts.mux, "/api/rework?date="+wed, nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Moved to") {
		t.Fatalf("rework on amended day = %d: %s", rr.Code, rr.Body.String())
	}

	// …and revoking by EITHER end restores the authored week.
	rec = postJSON(ts.mux, "/api/amend", map[string]string{"date": thu, "op": "revoke"})
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d: %s", rec.Code, rec.Body.String())
	}
	if _, ok := ts.s.amendInfoFor(wed); ok {
		t.Error("amendment still standing after revoke")
	}
	wk, di, _ := ts.s.ds().Blocks[0].Locate(blk.DayOf(0, 2))
	if wk.Days[di].Kind != KindEasy {
		t.Errorf("authored week not restored: %s", wk.Days[di].Kind)
	}
}

func TestReworkCandidates(t *testing.T) {
	dir := futureBlockDir(t)
	ts := fitTestMuxServer(t, dir)
	blk := ts.s.data.Load().Blocks[0]
	wed := blk.DayOf(0, 2).Format("2006-01-02")

	rec := get(ts.mux, "/api/rework?date="+wed, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("rework = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`"move"`, `"cancel"`, "Absorb"} {
		if !strings.Contains(body, want) {
			t.Errorf("candidates missing %s: %s", want, body)
		}
	}
	// The steps day explains itself rather than listing candidates.
	tue := blk.DayOf(0, 1).Format("2006-01-02")
	rec = get(ts.mux, "/api/rework?date="+tue, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"can":false`) {
		t.Errorf("steps day rework = %d: %s", rec.Code, rec.Body.String())
	}
}
