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
		{"cancel a structured day", amendOp{Date: "2026-01-06", Op: "cancel"}, none, ""},
		{"structured session moves to rest", amendOp{Date: "2026-01-06", Op: "move", Arg: "2026-01-08"}, none, ""},
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
	if wk.Days[di].Label != "Rest" {
		t.Errorf("vacated day label = %q — a labelless card renders as a bare dash", wk.Days[di].Label)
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

	// The calendar carries the ghost — and provenance stays out of the
	// cell's title/Note, which feeds the popover's grade-note slot.
	rec = get(ts.mux, "/calendar", nil)
	if !strings.Contains(rec.Body.String(), "→ Thu 8") {
		t.Errorf("calendar carries no ghost arrow for the vacated cell")
	}
	if strings.Contains(rec.Body.String(), `title="Moved`) {
		t.Errorf("the amend line leaked into a cell title — that slot is the grade's")
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
	// The dissolution is said where the athlete looks, not only in a log.
	if rec := get(ts.mux, "/", nil); !strings.Contains(rec.Body.String(), "no longer applies") {
		t.Error("a voided amendment is invisible on the today page")
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
	// Week 1's Friday becomes an easy bike so the swap/displace paths have
	// a destination; the defaults library carries no bike guide, so the
	// block brings its own — the block-level override path.
	weeks := blk["weeks"].([]any)
	blk["guides"] = map[string]any{
		"s-bike-easy": map[string]any{
			"id": "s-bike-easy", "title": "Easy spin", "summary": "Z1",
			"sections": []any{map[string]any{"label": "How", "text": "Spin easy."}},
		},
	}
	// The spin carries steps so the both-structured trade has a real case.
	weeks[0].(map[string]any)["days"].([]any)[4] = map[string]any{
		"kind": "bike_easy", "label": "Easy spin", "mins": 30,
		"steps": []any{map[string]any{"role": "active", "time": "30:00",
			"power": []any{"{{pct 45 .Athlete.Power.ftp}}", "{{pct 55 .Athlete.Power.ftp}}"}}},
	}
	// A kind-gated tracked task, so the tracked-loss path has something to
	// lose: "str" rides the quality day the way Strength A does.
	blk["checklist"] = append(blk["checklist"].([]any), map[string]any{
		"key": "str", "label": "Strength", "guide": "task-daily",
		"when": `{{and .InBlock (eq .Kind "quality")}}`,
	})
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
	// The athlete rides along so its issue can track the fixture's tasks:
	// the embedded defaults athlete plus "str" on the Achilles.
	araw, err := os.ReadFile("defaults/athlete.json")
	if err != nil {
		t.Fatal(err)
	}
	var ath map[string]any
	if err := json.Unmarshal(araw, &ath); err != nil {
		t.Fatal(err)
	}
	issue := ath["issues"].([]any)[0].(map[string]any)
	issue["tracks"] = []any{"daily", "str"}
	aout, err := json.Marshal(ath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "athlete.json"), append(aout, '\n'), 0o644); err != nil {
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

	// Re-apply, then grade the landed day: the record now gates the revoke
	// from BOTH ends — un-moving would strand the grade on a rest day.
	rec = postJSON(ts.mux, "/api/amend", map[string]string{
		"date": wed, "op": "move", "arg": thu, "note": "again"})
	if rec.Code != http.StatusOK {
		t.Fatalf("re-apply = %d: %s", rec.Code, rec.Body.String())
	}
	if err := ts.s.store.Append(Entry{Kind: "grade", Date: thu, Val: "B", Note: "n"}); err != nil {
		t.Fatal(err)
	}
	for _, end := range []string{wed, thu} {
		rec = postJSON(ts.mux, "/api/amend", map[string]string{"date": end, "op": "revoke"})
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "graded") {
			t.Errorf("revoke via %s with a graded end: %d %s", end, rec.Code, rec.Body.String())
		}
	}
	if _, ok := ts.s.amendInfoFor(wed); !ok {
		t.Error("the gated revoke dissolved the amendment anyway")
	}
}

func TestWeekPageCarriesTheReworkTrigger(t *testing.T) {
	dir := futureBlockDir(t)
	ts := fitTestMuxServer(t, dir)
	rec := get(ts.mux, "/week/1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("week = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "data-rework=") {
		t.Error("the week page offers no rework trigger — the central case is reworking a coming day, and the today card can only speak for today")
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
	// A structured day reworks too — its steps travel with it.
	tue := blk.DayOf(0, 1).Format("2006-01-02")
	rec = get(ts.mux, "/api/rework?date="+tue, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"can":true`) ||
		!strings.Contains(rec.Body.String(), `"move"`) {
		t.Errorf("structured day rework = %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAStructuredSessionTravelsWithItsSteps: the identity law forbids a
// standing serial serving changed bytes — but a date that never served a
// workout may start to, and one that stops serving tells no lie. So the
// quality session's steps follow it onto Thursday's rest day: /fit serves
// the workout at its new date and 404s the old one.
func TestAStructuredSessionTravelsWithItsSteps(t *testing.T) {
	dir := futureBlockDir(t)
	ts := fitTestMuxServer(t, dir)
	blk := ts.s.data.Load().Blocks[0]
	tue := blk.DayOf(0, 1).Format("2006-01-02") // quality, carries steps
	thu := blk.DayOf(0, 3).Format("2006-01-02") // rest

	rec := postJSON(ts.mux, "/api/amend", map[string]string{
		"date": tue, "op": "move", "arg": thu, "note": "conflict"})
	if rec.Code != http.StatusOK {
		t.Fatalf("moving a structured session = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := get(ts.mux, "/fit/"+thu, nil); rec.Code != http.StatusOK {
		t.Errorf("/fit/%s (landed) = %d — the steps did not travel", thu, rec.Code)
	}
	if rec := get(ts.mux, "/fit/"+tue, nil); rec.Code != http.StatusNotFound {
		t.Errorf("/fit/%s (vacated) = %d, want 404", tue, rec.Code)
	}
	// A single-sided move rotates nothing: the landed date's serial never
	// served and the vacated one stops serving — no serial tells a lie.
	if ir := ts.s.ds().identityRev(); ir != ts.s.data.Load().Rev {
		t.Errorf("a single-sided move rotated identity to %q — only both-structured trades may", ir)
	}
}

// TestDisplaceDropsTheBikeAndMovesTheRun: the swap's harder sibling — the
// run takes the easy bike's slot, the bike comes off the week, and the
// vacated day becomes rest. Both readings of run-outranks-bike are offered
// side by side, and revoking restores both slots.
func TestDisplaceDropsTheBikeAndMovesTheRun(t *testing.T) {
	dir := futureBlockDir(t)
	ts := fitTestMuxServer(t, dir)
	blk := ts.s.data.Load().Blocks[0]
	wed := blk.DayOf(0, 2).Format("2006-01-02") // easy run
	fri := blk.DayOf(0, 4).Format("2006-01-02") // easy spin (fixture-injected)

	// Both options appear for the bike day…
	rec := get(ts.mux, "/api/rework?date="+wed, nil)
	for _, want := range []string{`"swap"`, `"displace"`, "dropping its bike"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("candidates missing %s: %s", want, rec.Body.String())
		}
	}

	rec = postJSON(ts.mux, "/api/amend", map[string]string{
		"date": wed, "op": "displace", "arg": fri, "note": "one has to go"})
	if rec.Code != http.StatusOK {
		t.Fatalf("displace = %d: %s", rec.Code, rec.Body.String())
	}
	eff := ts.s.ds().Blocks[0]
	loc := ts.s.data.Load().Loc
	day := func(iso string) time.Time { tm, _ := time.ParseInLocation("2006-01-02", iso, loc); return tm }
	if wk, di, _ := eff.Locate(day(fri)); wk.Days[di].Kind != KindEasy {
		t.Errorf("the run did not take the bike's slot: %s", wk.Days[di].Kind)
	}
	if wk, di, _ := eff.Locate(day(wed)); wk.Days[di].Kind != KindRest {
		t.Errorf("the vacated day is not rest: %s", wk.Days[di].Kind)
	}
	if info, _ := ts.s.amendInfoFor(fri); info.Label != "Easy spin" {
		t.Errorf("the landed side forgot what it cost: %+v", info)
	}
	if line := amendLine(mustInfo(t, ts.s, fri), loc); !strings.Contains(line, "dropping Easy spin") {
		t.Errorf("provenance line says %q, want the dropped bike named", line)
	}

	rec = postJSON(ts.mux, "/api/amend", map[string]string{"date": fri, "op": "revoke"})
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d: %s", rec.Code, rec.Body.String())
	}
	if wk, di, _ := ts.s.ds().Blocks[0].Locate(day(fri)); wk.Days[di].Kind != KindBikeEasy {
		t.Errorf("the bike did not come back: %s", wk.Days[di].Kind)
	}
}

func mustInfo(t *testing.T, s *server, iso string) amendInfo {
	t.Helper()
	info, ok := s.amendInfoFor(iso)
	if !ok {
		t.Fatalf("no amendInfo for %s", iso)
	}
	return info
}

// TestTradingTwoStructuredDaysRotatesIdentity: the formerly forbidden case
// — both dates' bytes would change under standing serials — is allowed
// because exactly those ops fold into identityRev: every workout serial
// re-mints (the same consequence any plan edit has), and revoking restores
// the authored identity byte-exactly.
func TestTradingTwoStructuredDaysRotatesIdentity(t *testing.T) {
	dir := futureBlockDir(t)
	ts := fitTestMuxServer(t, dir)
	authored := ts.s.data.Load()
	blk := authored.Blocks[0]
	tue := blk.DayOf(0, 1).Format("2006-01-02") // quality, steps
	fri := blk.DayOf(0, 4).Format("2006-01-02") // spin, steps (fixture)

	if ir := ts.s.ds().identityRev(); ir != authored.Rev {
		t.Fatalf("identityRev %q differs from the Rev with no amendments", ir)
	}
	rec := postJSON(ts.mux, "/api/amend", map[string]string{
		"date": tue, "op": "swap", "arg": fri, "note": "trade"})
	if rec.Code != http.StatusOK {
		t.Fatalf("both-structured swap = %d: %s", rec.Code, rec.Body.String())
	}
	if ts.s.ds().identityRev() == authored.Rev {
		t.Error("two structured days traded places and no identity rotated")
	}
	// Both dates serve their arrived workouts under the rotated identity.
	for _, iso := range []string{tue, fri} {
		if rec := get(ts.mux, "/fit/"+iso, nil); rec.Code != http.StatusOK {
			t.Errorf("/fit/%s = %d after the trade", iso, rec.Code)
		}
	}
	// The overlay state is visible from outside.
	if rec := get(ts.mux, "/healthz", nil); !strings.Contains(rec.Body.String(), `"amend":1`) ||
		!strings.Contains(rec.Body.String(), `"ident"`) {
		t.Errorf("healthz hides the overlay state: %s", rec.Body.String())
	}
	// Revoking restores the authored identity exactly.
	if rec := postJSON(ts.mux, "/api/amend", map[string]string{"date": fri, "op": "revoke"}); rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d: %s", rec.Code, rec.Body.String())
	}
	if ir := ts.s.ds().identityRev(); ir != authored.Rev {
		t.Errorf("identity did not return with the authored plan: %q", ir)
	}
}
