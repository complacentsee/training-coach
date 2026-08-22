package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/muktihari/fit/profile/typedef"
)

// A steady run with a burst in the middle: the fastest mile sits inside
// the burst, the fastest 5 km must include it plus steady ground either
// side, and the arithmetic of both is checkable by hand.
func burstRun(t *testing.T, date time.Time) []byte {
	t.Helper()
	return benchFixture(t, date, typedef.SportRunning, []benchSeg{
		{secs: 1200, vel: 3.0, hr0: 140, hr1: 150, step: -1, lapTrigger: typedef.LapTriggerDistance},
		{secs: 400, vel: 4.5, hr0: 165, hr1: 178, step: -1, lapTrigger: typedef.LapTriggerManual},
		{secs: 1200, vel: 3.0, hr0: 155, hr1: 150, step: -1, lapTrigger: typedef.LapTriggerDistance},
	})
}

func TestBestEffortIsTheFewestSecondsForTheDistance(t *testing.T) {
	s, err := decodeActivity(burstRun(t, time.Date(2026, 1, 13, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	// A mile at 4.5 m/s: 1609.344 / 4.5 = 357.6 s, so the first whole
	// second at or past a mile is 358 — the burst is 1800 m, room enough.
	if got := bestEffort(s, metresPerMile); got == nil || *got < 357 || *got > 359 {
		t.Errorf("fastest mile %v, want ~358 s", deref(got))
	}
	// 5 km must take the whole 1800 m burst (400 s) plus 3200 m of steady
	// running at 3 m/s (1066.7 s) on one side or both: ~1467 s.
	if got := bestEffort(s, 5000); got == nil || *got < 1465 || *got > 1470 {
		t.Errorf("fastest 5 km %v, want ~1467 s", deref(got))
	}
	// Longer than the file: none.
	if got := bestEffort(s, 20000); got != nil {
		t.Errorf("a 20 km stretch in a 9.6 km run: %v", *got)
	}
}

// Every imported run lands its two numbers; a ride lands none; a run too
// short for 5 km lands NULL there; the day's best is the minimum over
// its runs; a vanished file's row is pruned.
func TestEffortsRowsFollowTheArchive(t *testing.T) {
	dir := t.TempDir()
	ts := fitTestMuxServer(t, dir)
	adir := filepath.Join(dir, "activities")
	if err := os.MkdirAll(adir, 0o755); err != nil {
		t.Fatal(err)
	}
	put := func(name string, data []byte) {
		if err := os.WriteFile(filepath.Join(adir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ts.s.metrics.importOne(name, data, time.UTC, nil); err != nil {
			t.Fatalf("import %s: %v", name, err)
		}
	}
	d := func(day int) time.Time { return time.Date(2026, 1, day, 0, 0, 0, 0, time.UTC) }
	put("2026-01-13-07-00-00.fit", burstRun(t, d(13)))
	put("2026-01-13-18-00-00.fit", benchFixture(t, d(13), typedef.SportRunning, []benchSeg{ // a faster short run: 2 km at 5 m/s
		{secs: 400, vel: 5.0, hr0: 160, hr1: 180, step: -1, lapTrigger: typedef.LapTriggerManual}}))
	put("2026-01-14-07-00-00.fit", benchFixture(t, d(14), typedef.SportCycling, []benchSeg{
		{secs: 1800, vel: 9.0, hr0: 120, hr1: 150, w0: 200, w1: 200, step: -1, lapTrigger: typedef.LapTriggerManual}}))

	got, err := ts.s.metrics.effortsByDate("2026-01-13", "2026-01-14")
	if err != nil {
		t.Fatal(err)
	}
	e13, ok := got["2026-01-13"]
	if !ok || e13.MileS == nil || e13.K5S == nil {
		t.Fatalf("13 Jan: %+v", e13)
	}
	// The evening 2 km at 5 m/s holds the day's fastest mile (~322 s); only
	// the morning run is long enough for 5 km.
	if *e13.MileS < 321 || *e13.MileS > 323 {
		t.Errorf("13 Jan fastest mile %d, want ~322 from the short fast run", *e13.MileS)
	}
	if *e13.K5S < 1465 || *e13.K5S > 1470 {
		t.Errorf("13 Jan fastest 5 km %d, want ~1467 from the long run", *e13.K5S)
	}
	if _, ok := got["2026-01-14"]; ok {
		t.Error("a ride must not appear in the efforts series")
	}
	var k5 *int
	if err := ts.s.metrics.r.QueryRow(`SELECT k5_s FROM efforts WHERE name = ?`, "2026-01-13-18-00-00.fit").Scan(&k5); err != nil || k5 != nil {
		t.Errorf("the 2 km run's 5 km stretch should be NULL: %v %v", k5, err)
	}

	// Prune follows the archive.
	if err := os.Remove(filepath.Join(adir, "2026-01-13-18-00-00.fit")); err != nil {
		t.Fatal(err)
	}
	ts.s.metrics.reconcile(adir, time.UTC, nil)
	got, _ = ts.s.metrics.effortsByDate("2026-01-13", "2026-01-13")
	if e := got["2026-01-13"]; e.MileS == nil || *e.MileS < 357 {
		t.Errorf("after the short run left the archive the day's mile should be the long run's ~358: %v", deref(e.MileS))
	}
}

// The startup reconcile back-fills runs the table lacks — the archive
// from before the table existed, and every run after a version bump —
// without re-importing anything else.
func TestEffortsBackfillOnReconcile(t *testing.T) {
	dir := t.TempDir()
	ts := fitTestMuxServer(t, dir)
	adir := filepath.Join(dir, "activities")
	if err := os.MkdirAll(adir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "2026-01-13-07-00-00.fit"
	data := burstRun(t, time.Date(2026, 1, 13, 0, 0, 0, 0, time.UTC))
	if err := os.WriteFile(filepath.Join(adir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.s.metrics.importOne(name, data, time.UTC, nil); err != nil {
		t.Fatal(err)
	}
	// Forget the row, as an archive imported before the table would have.
	if _, err := ts.s.metrics.w.Exec(`DELETE FROM efforts`); err != nil {
		t.Fatal(err)
	}
	ts.s.metrics.reconcile(adir, time.UTC, nil)
	ts.s.metrics.backfillEfforts(adir)
	got, _ := ts.s.metrics.effortsByDate("2026-01-13", "2026-01-13")
	if e := got["2026-01-13"]; e.MileS == nil {
		t.Fatal("the backfill did not measure the run's efforts")
	}
	// An older version is re-measured too.
	if _, err := ts.s.metrics.w.Exec(`UPDATE efforts SET version = 0, mile_s = 1`); err != nil {
		t.Fatal(err)
	}
	ts.s.metrics.backfillEfforts(adir)
	got, _ = ts.s.metrics.effortsByDate("2026-01-13", "2026-01-13")
	if e := got["2026-01-13"]; e.MileS == nil || *e.MileS == 1 {
		t.Errorf("an old-version row was not re-measured: %v", deref(e.MileS))
	}
	var n int
	if err := ts.s.metrics.r.QueryRow(`SELECT COUNT(*) FROM activities`).Scan(&n); err != nil || n != 1 {
		t.Errorf("the backfill re-imported activities: %d rows", n)
	}
}

// /trends charts the two dense panels — one label on the best, one on
// the latest, every number in the table — with the goal line on the 5 km
// panel when the goal is a 5K, and the nav offers Trends for a block with
// runs recorded even if it tags no benchmark day.
func TestTrendsShowsTheBestEffortTrend(t *testing.T) {
	dir := t.TempDir()
	start := shiftedBlock(t, dir) // the embedded example, a 10K goal, no tags
	ts := fitTestMuxServer(t, dir)
	for i, secs := range []int{2000, 1900, 2100, 1800} { // four steady runs, the third slower
		name := start.AddDate(0, 0, i+1).Format("2006-01-02") + "-07-00-00.fit"
		data := benchFixture(t, start.AddDate(0, 0, i+1), typedef.SportRunning, []benchSeg{
			{secs: secs, vel: 6000.0 / float64(secs), hr0: 150, hr1: 160, step: -1, lapTrigger: typedef.LapTriggerDistance}})
		if _, err := ts.s.metrics.importOne(name, data, time.UTC, nil); err != nil {
			t.Fatal(err)
		}
	}
	rec := httptest.NewRecorder()
	ts.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/trends", nil))
	body := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("/trends: %d", rec.Code)
	}
	sums := regexp.MustCompile(`<h2>([^<]*)</h2>\s*<span class="bench-sum">([^<]*)</span>`).FindAllStringSubmatch(body, -1)
	got := map[string]string{}
	for _, m := range sums {
		got[m[1]] = m[2]
	}
	// 6 km at the fourth run's pace: a mile in 1800*1609.344/6000 = 482.8 → 483 s.
	if !strings.HasPrefix(got["Fastest mile"], "best 8:03 (W1)") {
		t.Errorf("fastest mile summary %q, want best 8:03 from the fourth run", got["Fastest mile"])
	}
	// 5000 of 6000 m at the fourth run's pace is 1500.0 s; the stretch
	// search walks integer-second samples, so it lands on 1500 or 1501.
	if !regexp.MustCompile(`^best 25:0[01] \(W1\)`).MatchString(got["Fastest 5 km stretch"]) {
		t.Errorf("fastest 5 km summary %q, want best 25:00/25:01 from the fourth run", got["Fastest 5 km stretch"])
	}
	if strings.Contains(body, "GOAL 45:00") {
		t.Error("a 10K goal must not draw a goal line on the 5 km panel")
	}
	// Dense: four markers, two printed labels per panel (best and latest —
	// here the same run, so one).
	panels := strings.Split(body, `<section class="card bench">`)[1:]
	if len(panels) != 2 {
		t.Fatalf("%d panels, want 2", len(panels))
	}
	for _, p := range panels {
		dots := strings.Count(p, `class="bench-dot`)
		labels := strings.Count(p, `class="bench-val"`)
		if dots != 4 || labels != 1 {
			t.Errorf("dense panel: %d markers, %d labels; want 4 and 1 (best = latest)", dots, labels)
		}
		if strings.Count(p, "<tr>") != 4 {
			t.Errorf("the table should hold every run: %d rows", strings.Count(p, "<tr>"))
		}
	}
	rec = httptest.NewRecorder()
	ts.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if !strings.Contains(rec.Body.String(), `href="/trends"`) {
		t.Error("the nav should offer Trends once the block has a run recorded")
	}
}
