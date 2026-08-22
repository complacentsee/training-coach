package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/muktihari/fit/profile/typedef"
	"github.com/muktihari/fit/proto"
)

// recordingOn is a minute of sport on a date at noon UTC, declaring distM
// metres in its session message — the figure the metrics cache stores as
// distance_m.
func recordingOn(t *testing.T, date time.Time, sport typedef.Sport, distM float64) []byte {
	t.Helper()
	t0 := time.Date(date.Year(), date.Month(), date.Day(), 12, 0, 0, 0, time.UTC)
	msgs := []proto.Message{mesgdefFileID(t0)}
	for i := 0; i <= 60; i++ {
		msgs = append(msgs, recordAt(t0, i, 130, 3000, 80))
	}
	msgs = append(msgs, sessionMsg(sport, uint32(distM*100)))
	return encodeRaw(t, msgs)
}

func importOn(t *testing.T, m *metricsDB, date time.Time, sport typedef.Sport, distM float64, hour string) {
	t.Helper()
	name := date.Format("2006-01-02") + "-" + hour + "-00-00.fit"
	if _, err := m.importOne(name, recordingOn(t, date, sport, distM), time.UTC, nil); err != nil {
		t.Fatalf("import %s: %v", name, err)
	}
}

func TestRunDistanceByDateSumsRunningOnly(t *testing.T) {
	m, err := openMetricsDB(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.close)
	d := func(day int) time.Time { return time.Date(2026, 1, day, 0, 0, 0, 0, time.UTC) }
	importOn(t, m, d(6), typedef.SportRunning, 8000, "07")
	importOn(t, m, d(6), typedef.SportRunning, 2000, "18")  // a second run the same day: summed
	importOn(t, m, d(7), typedef.SportCycling, 30000, "07") // a ride: not running
	importOn(t, m, d(8), typedef.SportRunning, 8200, "07")
	importOn(t, m, d(4), typedef.SportRunning, 5000, "07")  // the day before the range
	importOn(t, m, d(12), typedef.SportRunning, 5000, "07") // the day after it

	got, err := m.runDistanceByDate("2026-01-05", "2026-01-11")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"2026-01-06": 10000, "2026-01-08": 8200}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %v, want %v", k, got[k], v)
		}
	}
}

func TestRanLabel(t *testing.T) {
	for _, c := range []struct{ plan, ran, want string }{
		{"30 mi", "27.4 mi", "ran 27.4"},
		{"37 km", "16.2 km", "ran 16.2"},
		{"0 km", "400 m", "ran 400 m"},
		{"30 miles", "27.4 miles", "ran 27.4"},
		{"1 mile", "0.8 miles", "ran 0.8 miles"},
		{"37 kilometres", "0 kilometres", "ran 0"},
	} {
		got := ranLabel(c.plan, c.ran)
		if strings.Contains(got, " ") {
			t.Errorf("ranLabel(%q, %q) = %q carries a breaking space; a row may only break before it", c.plan, c.ran, got)
		}
		if got = strings.ReplaceAll(got, "\u00a0", " "); got != c.want {
			t.Errorf("ranLabel(%q, %q) = %q, want %q", c.plan, c.ran, got, c.want)
		}
	}
}

// shiftedBlock writes the embedded example block into dir/blocks with its
// start moved so that, relative to today: week 1 is wholly past, week 2 is
// the week in progress, and a copied week 3 has not begun. Returns the
// start date. The athlete stays the embedded default (metric, UTC).
func shiftedBlock(t *testing.T, dir string) time.Time {
	t.Helper()
	raw, err := os.ReadFile("defaults/blocks/example-base-block.json")
	if err != nil {
		t.Fatal(err)
	}
	var b map[string]any
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	today := time.Now().UTC()
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	lastWeek := today.AddDate(0, 0, -7)
	start := lastWeek.AddDate(0, 0, -((int(lastWeek.Weekday()) + 6) % 7)) // its Monday
	b["start"] = start.Format("2006-01-02")
	b["goal"].(map[string]any)["date"] = start.AddDate(0, 0, 20).Format("2006-01-02")
	b["mesocycles"] = []any{map[string]any{"name": "Base", "weeks": "1-3"}}
	weeks := b["weeks"].([]any)
	w3 := map[string]any{}
	for k, v := range weeks[1].(map[string]any) {
		w3[k] = v
	}
	w3["n"] = 3
	b["weeks"] = append(weeks, w3)
	out, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "blocks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blocks", "shifted.json"), append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return start
}

var rowRe = regexp.MustCompile(`(?s)<th class="wk">.*?</th>`)

func calendarRows(t *testing.T, ts testServer) []string {
	t.Helper()
	rec := httptest.NewRecorder()
	ts.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/calendar", nil))
	if rec.Code != 200 {
		t.Fatalf("calendar: %d %s", rec.Code, rec.Body.String())
	}
	// The joins inside "ran 27.4" are non-breaking spaces; the assertions
	// read them as the spaces they display as.
	rows := rowRe.FindAllString(strings.ReplaceAll(rec.Body.String(), "\u00a0", " "), -1)
	if len(rows) != 3 {
		t.Fatalf("%d week rows, want 3", len(rows))
	}
	return rows
}

// The calendar says what was run beside what was prescribed, once a week
// has begun: a past week, the week in progress, and nothing at all on a
// week still to come. Running only — the ride in week 1 is not in its sum.
func TestCalendarShowsWhatWasRun(t *testing.T) {
	dir := t.TempDir()
	start := shiftedBlock(t, dir)
	ts := fitTestMuxServer(t, dir)
	importOn(t, ts.s.metrics, start.AddDate(0, 0, 1), typedef.SportRunning, 8000, "07")
	importOn(t, ts.s.metrics, start.AddDate(0, 0, 3), typedef.SportRunning, 8200, "07")
	importOn(t, ts.s.metrics, start.AddDate(0, 0, 2), typedef.SportCycling, 30000, "07")
	importOn(t, ts.s.metrics, start.AddDate(0, 0, 7), typedef.SportRunning, 5000, "07") // week 2's Monday: always ≤ today
	// A run on the last day of week 3 would be in the future; nothing is
	// recorded there, and the row must not even say "ran 0".

	// The label is its own line under the volume: a second block span,
	// which strip() reads as "37 km ran 16.2".
	rows := calendarRows(t, ts)
	if !strings.Contains(strip(rows[0]), "37 km ran 16.2") {
		t.Errorf("week 1 row: want \"37 km\" then \"ran 16.2\" (8 + 8.2, not the 30 km ride), got %s", strip(rows[0]))
	}
	if !strings.Contains(rows[0], "37 km</span>") || !strings.Contains(rows[0], "<span>ran 16.2</span>") {
		t.Errorf("week 1 row: the label must be its own span under the volume, got %s", rows[0])
	}
	if !strings.Contains(strip(rows[1]), "38 km ran 5") {
		t.Errorf("week 2 row (in progress): want \"38 km\" then \"ran 5\", got %s", strip(rows[1]))
	}
	if strings.Contains(rows[2], "ran") {
		t.Errorf("week 3 row (future) must show the plan alone, got %s", strip(rows[2]))
	}
	if !strings.Contains(rows[2], "38 km") {
		t.Errorf("week 3 row lost its prescribed volume: %s", strip(rows[2]))
	}
}

// A past week with nothing recorded, in a block where running WAS recorded,
// says so: "ran 0" is the honest answer to "am I absorbing the plan".
func TestCalendarSaysRanZeroForAnEmptyPastWeek(t *testing.T) {
	dir := t.TempDir()
	start := shiftedBlock(t, dir)
	ts := fitTestMuxServer(t, dir)
	importOn(t, ts.s.metrics, start.AddDate(0, 0, 7), typedef.SportRunning, 5000, "07")
	rows := calendarRows(t, ts)
	if !strings.Contains(strip(rows[0]), "37 km ran 0") {
		t.Errorf("week 1 row: want \"37 km\" then \"ran 0\", got %s", strip(rows[0]))
	}
}

// No metrics cache, or a cache with no running in the block: the pages
// read exactly as they did before recordings existed. The prescribed
// volume stands alone; nothing says "ran".
func TestCalendarWithoutRecordingsShowsThePlanAlone(t *testing.T) {
	t.Run("no metrics db", func(t *testing.T) {
		dir := t.TempDir()
		shiftedBlock(t, dir)
		ts := fitTestMuxServer(t, dir)
		ts.s.metrics = nil
		for i, r := range calendarRows(t, ts) {
			if strings.Contains(r, "ran") {
				t.Errorf("row %d says ran with no metrics cache: %s", i+1, strip(r))
			}
		}
		rec := httptest.NewRecorder()
		ts.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/week/1", nil))
		if rec.Code != 200 || strings.Contains(rec.Body.String(), "· ran") {
			t.Errorf("week page with no metrics cache: %d, says ran=%v", rec.Code, strings.Contains(rec.Body.String(), "· ran"))
		}
	})
	t.Run("nothing running recorded", func(t *testing.T) {
		dir := t.TempDir()
		start := shiftedBlock(t, dir)
		ts := fitTestMuxServer(t, dir)
		importOn(t, ts.s.metrics, start.AddDate(0, 0, 2), typedef.SportCycling, 30000, "07")
		for i, r := range calendarRows(t, ts) {
			if strings.Contains(r, "ran") {
				t.Errorf("row %d says ran when only a ride was recorded: %s", i+1, strip(r))
			}
		}
	})
}

// The week page carries the same figure in its long form.
func TestWeekPageShowsWhatWasRun(t *testing.T) {
	dir := t.TempDir()
	start := shiftedBlock(t, dir)
	ts := fitTestMuxServer(t, dir)
	importOn(t, ts.s.metrics, start.AddDate(0, 0, 1), typedef.SportRunning, 8000, "07")
	importOn(t, ts.s.metrics, start.AddDate(0, 0, 3), typedef.SportRunning, 8200, "07")

	get := func(n string) string {
		rec := httptest.NewRecorder()
		ts.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/week/"+n, nil))
		if rec.Code != 200 {
			t.Fatalf("week %s: %d", n, rec.Code)
		}
		return strings.ReplaceAll(rec.Body.String(), "\u00a0", " ")
	}
	if b := get("1"); !strings.Contains(b, "37 kilometres · ran 16.2</p>") {
		t.Errorf("week 1 page: want \"37 kilometres · ran 16.2\", got %q", subAfter(b, `class="page-sub"`))
	}
	if b := get("3"); strings.Contains(b, "· ran") {
		t.Errorf("week 3 page (future) says ran: %q", subAfter(b, `class="page-sub"`))
	}
}

func strip(html string) string {
	return strings.Join(strings.Fields(regexp.MustCompile(`<[^>]+>`).ReplaceAllString(html, " ")), " ")
}

func subAfter(s, marker string) string {
	if i := strings.Index(s, marker); i >= 0 {
		s = s[i:]
	}
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}
