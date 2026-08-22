package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A stub Open-Meteo forecast: two days of hours in the zone asked for,
// temperatures climbing through each morning. calls counts requests.
func forecastStub(t *testing.T, calls *int32, morning func(day, hour int) (temp, dew float64)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		loc, err := time.LoadLocation(r.URL.Query().Get("timezone"))
		if err != nil {
			http.Error(w, "bad timezone", 400)
			return
		}
		if r.URL.Query().Get("temperature_unit") != "fahrenheit" || r.URL.Query().Get("forecast_days") != "2" {
			http.Error(w, "unexpected query "+r.URL.RawQuery, 400)
			return
		}
		now := time.Now().In(loc)
		day0 := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
		var times []string
		var temp, dew, wind, rain []float64
		for d := 0; d < 2; d++ {
			for h := 0; h < 24; h++ {
				times = append(times, day0.AddDate(0, 0, d).Add(time.Duration(h)*time.Hour).Format("2006-01-02T15:04"))
				tp, dw := morning(d, h)
				temp, dew = append(temp, tp), append(dew, dw)
				wind, rain = append(wind, 7), append(rain, 10)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"hourly": map[string]any{
			"time": times, "temperature_2m": temp, "dew_point_2m": dew, "wind_speed_10m": wind, "precipitation_probability": rain}})
	}))
}

// A server on a block that holds today, with a position in the weather
// cache (where the athlete last ran) and the forecast on.
func forecastServer(t *testing.T, stub *httptest.Server) (testServer, time.Time) {
	t.Helper()
	t.Setenv("FORECAST_BASE_URL", stub.URL)
	dir := t.TempDir()
	start := shiftedBlock(t, dir)
	ts := fitTestMuxServer(t, dir)
	ts.s.forecast = newForecastService(true)
	if _, err := ts.s.metrics.w.Exec(`INSERT INTO weather VALUES(51.5,-0.1,'2026-08-20T12:00:00Z',70,60,70,5,'2026-08-20T13:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	return ts, start
}

// waitForecast lets the background fetch land: the first read kicks it
// off and returns nothing, by design.
func waitForecast(t *testing.T, f *forecastService) {
	t.Helper()
	for i := 0; i < 200; i++ {
		f.mu.Lock()
		have := len(f.cache) > 0
		f.mu.Unlock()
		if have {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the forecast never arrived")
}

// The first page view starts a fetch and shows nothing; the next shows
// the three morning hours for today's run; an hour later the cache is
// stale and ONE refresh runs in the background while the page keeps
// showing what it has.
func TestForecastIsCachedAndNeverWaitedOn(t *testing.T) {
	var calls int32
	stub := forecastStub(t, &calls, func(day, hour int) (float64, float64) { return 60 + float64(hour), 55 })
	defer stub.Close()
	ts, _ := forecastServer(t, stub)
	d := ts.s.ds()
	blk := d.Current(ts.s.day(d))
	today := ts.s.day(d)
	morning := time.Date(today.Year(), today.Month(), today.Day(), 5, 30, 0, 0, d.Loc)

	// Find a day in the block whose session is a run: the shifted example
	// block runs Tue/Wed/Fri/Sat.
	var runDay time.Time
	for i := 0; i < 7; i++ {
		dd := today.AddDate(0, 0, -i)
		if wk, di, ok := blk.Locate(dd); ok && wk.Days[di].Kind.IsRun() {
			runDay = dd
			break
		}
	}
	if runDay.IsZero() {
		t.Skip("no run day in the last week of the fixture")
	}
	first := ts.s.forecastFor(d, blk, runDay, morning, false)
	if first != nil {
		t.Errorf("the first read should return nothing and start the fetch, got %+v", first)
	}
	waitForecast(t, ts.s.forecast)
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("%d requests, want 1", calls)
	}
	// The stub's days are real today/tomorrow; runDay may be earlier in the
	// week, so ask about a day the stub covers by pinning runDay to today
	// when today is a run, else tomorrow is asked for by forecastFor itself.
	v := ts.s.forecastFor(d, blk, today, morning, false)
	if v == nil {
		t.Fatal("no forecast after the fetch landed")
	}
	// 6 am = 66°F, 7 am = 67°F, 8 am = 68°F, dew 55°F throughout — and the
	// fixture athlete is metric, so 19°/13°, 19°/13°, 20°/13°, 11 km/h.
	if !strings.Contains(v.Line, "6 am 19°/13° · 7 am 19°/13° · 8 am 20°/13° · wind 11 km/h · rain 10%") {
		t.Errorf("line %q", v.Line)
	}
	if v.Day != "Today" && v.Day != "Tomorrow" {
		t.Errorf("day %q", v.Day)
	}
	// Stale: an hour on, the page still reads the old forecast and exactly
	// one refresh runs.
	ts.s.forecast.now = func() time.Time { return time.Now().Add(2 * forecastTTL) }
	for i := 0; i < 5; i++ {
		if ts.s.forecastFor(d, blk, today, morning, false) == nil {
			t.Error("a stale cache must still serve while it refreshes")
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&calls) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("%d requests after going stale, want exactly 2 (single-flight)", n)
	}
}

// Which morning: today's while its run is ahead and the morning is young;
// tomorrow's once the run is recorded or the morning is over; nothing
// when neither day's session is outdoors; nothing with no position.
func TestForecastPicksTheNextOutdoorMorning(t *testing.T) {
	var calls int32
	stub := forecastStub(t, &calls, func(day, hour int) (float64, float64) { return 60 + float64(10*day) + float64(hour), 50 })
	defer stub.Close()
	ts, _ := forecastServer(t, stub)
	d := ts.s.ds()
	today := ts.s.day(d)
	blk := d.Current(today)
	ts.s.forecastFor(d, blk, today, today.Add(6*time.Hour), false)
	waitForecast(t, ts.s.forecast)

	wkT, diT, _ := blk.Locate(today)
	wkN, diN, okN := blk.Locate(today.AddDate(0, 0, 1))
	todayRun := wkT.Days[diT].Kind.IsRun()
	tomorrowRun := okN && wkN.Days[diN].Kind.IsRun()

	early := ts.s.forecastFor(d, blk, today, today.Add(6*time.Hour), false)
	late := ts.s.forecastFor(d, blk, today, today.Add(15*time.Hour), false)
	recorded := ts.s.forecastFor(d, blk, today, today.Add(6*time.Hour), true)
	switch {
	case todayRun:
		if early == nil || early.Day != "Today" || !strings.Contains(early.Line, "6 am 19°/10°") { // 66°F/50°F, metric
			t.Errorf("early on a run day: %+v", early)
		}
		if tomorrowRun {
			if late == nil || late.Day != "Tomorrow" || !strings.Contains(late.Line, "6 am 24°/10°") { // 76°F/50°F
				t.Errorf("afternoon of a run day with a run tomorrow: %+v", late)
			}
			if recorded == nil || recorded.Day != "Tomorrow" {
				t.Errorf("run recorded, run tomorrow: %+v", recorded)
			}
		} else if late != nil || recorded != nil {
			t.Errorf("afternoon / recorded with no run tomorrow: %+v %+v", late, recorded)
		}
	case tomorrowRun:
		if early == nil || early.Day != "Tomorrow" {
			t.Errorf("not a run today, run tomorrow: %+v", early)
		}
	default:
		if early != nil {
			t.Errorf("no outdoor session today or tomorrow: %+v", early)
		}
	}
	// No position, no forecast.
	if _, err := ts.s.metrics.w.Exec(`DELETE FROM weather`); err != nil {
		t.Fatal(err)
	}
	if v := ts.s.forecastFor(d, blk, today, today.Add(6*time.Hour), false); v != nil {
		t.Errorf("without a position: %+v", v)
	}
	// Switched off: nothing, and no request.
	before := atomic.LoadInt32(&calls)
	ts.s.forecast = newForecastService(false)
	if v := ts.s.forecastFor(d, blk, today, today.Add(6*time.Hour), false); v != nil || atomic.LoadInt32(&calls) != before {
		t.Errorf("disabled: %+v, calls %d→%d", v, before, calls)
	}
}

// The LT gate, evaluated: the first morning hour under 140, or none with
// the numbers; only on an LT-tagged day.
func TestLTGateIsEvaluatedMechanically(t *testing.T) {
	hours := func(sums ...float64) map[int]forecastHour {
		m := map[int]forecastHour{}
		for i, s := range sums {
			m[5+i] = forecastHour{TempF: s - 60, DewF: 60}
		}
		return m
	}
	if got := ltGate(hours(144, 139, 141, 145, 150)); got != "LT gate (temp + dew point under 140): 139 at 6 am — go" {
		t.Errorf("gate passes at 6: %q", got)
	}
	if got := ltGate(hours(141, 143, 146, 149, 152)); got != "LT gate (temp + dew point under 140): not met before 9 am — 141 at 5 am, 143 at 6 am, 146 at 7 am, 149 at 8 am, 152 at 9 am" {
		t.Errorf("gate fails all morning: %q", got)
	}
	if got := ltGate(map[int]forecastHour{}); got != "" {
		t.Errorf("no hours: %q", got)
	}

	// On the page: an LT-tagged run tomorrow carries the gate line.
	var calls int32
	stub := forecastStub(t, &calls, func(day, hour int) (float64, float64) { return 70 + float64(hour), 66 })
	defer stub.Close()
	ts, start := forecastServer(t, stub)
	d := ts.s.ds()
	today := ts.s.day(d)
	blk := d.Current(today)
	// Tag tomorrow's session LT on disk (with its guide) and reload.
	raw, err := os.ReadFile(filepath.Join(ts.s.dataDir, "blocks", "shifted.json"))
	if err != nil {
		t.Fatal(err)
	}
	var b map[string]any
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	tomorrow := today.AddDate(0, 0, 1)
	wi := DaysBetween(start, tomorrow) / 7
	di := DaysBetween(start, tomorrow) % 7
	weeks := b["weeks"].([]any)
	days := weeks[wi].(map[string]any)["days"].([]any)
	days[di] = map[string]any{"kind": "quality", "label": "30-MIN LT FIELD TEST", "tag": "LT", "dist": "12 km"}
	out, _ := json.Marshal(b)
	if err := os.WriteFile(filepath.Join(ts.s.dataDir, "blocks", "shifted.json"), append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	benchGuides(t, ts.s.dataDir)
	if err := ts.s.reload(); err != nil {
		t.Fatal(err)
	}
	d = ts.s.ds()
	blk = d.Current(today)
	ts.s.forecastFor(d, blk, today, today.Add(15*time.Hour), false)
	waitForecast(t, ts.s.forecast)
	v := ts.s.forecastFor(d, blk, today, today.Add(15*time.Hour), false)
	if v == nil || v.Day != "Tomorrow" {
		t.Fatalf("tomorrow's LT day: %+v", v)
	}
	// temp 70+h, dew 66: 5 am 141, 6 am 142 … none under 140 before 9.
	if !strings.HasPrefix(v.Gate, "LT gate (temp + dew point under 140): not met before 9 am — 141 at 5 am") {
		t.Errorf("gate on the page: %q", v.Gate)
	}
	// The page, with the clock at 3 pm so the card looks to tomorrow.
	ts.s.clock = func() time.Time { return today.Add(15 * time.Hour) }
	rec := httptest.NewRecorder()
	ts.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if !strings.Contains(rec.Body.String(), `class="forecast"`) || !strings.Contains(rec.Body.String(), "LT gate") {
		i := strings.Index(rec.Body.String(), "card-hd")
		if i < 0 {
			i = 0
		}
		t.Errorf("the today card does not carry the forecast line with the gate; card starts: %s", rec.Body.String()[i:min(i+600, len(rec.Body.String()))])
	}
}

// benchGuides writes the t- guides a tagged block needs into the fixture
// library, the way benchBlock does.
func benchGuides(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "library"), 0o755); err != nil {
		t.Fatal(err)
	}
	copyFile(t, "defaults/library/index.json", filepath.Join(dir, "library", "index.json"))
	graw, err := os.ReadFile("defaults/library/guides.json")
	if err != nil {
		t.Fatal(err)
	}
	var lib map[string]any
	if err := json.Unmarshal(graw, &lib); err != nil {
		t.Fatal(err)
	}
	guides := lib["guides"].(map[string]any)
	for _, tag := range []string{"FTP", "DEC", "LT", "TT", "RACE"} {
		guides["t-"+tag] = map[string]any{"id": "t-" + tag, "title": tag + " test",
			"sections": []any{map[string]any{"label": "Protocol", "text": "As written."}}}
	}
	lraw, err := json.Marshal(lib)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "library", "guides.json"), append(lraw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
