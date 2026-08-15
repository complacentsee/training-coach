package main

// The weather lookup, offline. What matters here is not that a provider
// answers — it is that the position leaving this machine is coarse, that
// the same hour is asked for once, that being switched off means silence on
// the wire, and that every failure costs the weather and nothing else.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/muktihari/fit/profile/mesgdef"
	"github.com/muktihari/fit/profile/typedef"
	"github.com/muktihari/fit/proto"
)

// archiveStub answers like the provider and records what it was asked.
func archiveStub(t *testing.T, hits *int32, seen *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		q := r.URL.Query()
		if seen != nil {
			*seen = q.Get("latitude") + "," + q.Get("longitude")
		}
		day := q.Get("start_date")
		out := map[string]any{"hourly": map[string]any{
			"time":                 []string{day + "T12:00", day + "T13:00", day + "T14:00"},
			"temperature_2m":       []float64{60, 68.4, 71},
			"dew_point_2m":         []float64{55, 65.6, 66},
			"relative_humidity_2m": []float64{80, 91.4, 84},
			"wind_speed_10m":       []float64{4, 7.5, 9},
		}}
		_ = json.NewEncoder(w).Encode(out)
	}))
}

func weatherUnderTest(t *testing.T, base string, on bool) *weatherService {
	t.Helper()
	db, err := openMetricsDB(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.close)
	t.Setenv("WEATHER_BASE_URL", base)
	mode := "off"
	if on {
		mode = "on"
	}
	t.Setenv("WEATHER_LOOKUP", mode)
	return newWeatherService(db)
}

func TestWeatherReadsTheHourAndCachesIt(t *testing.T) {
	var hits int32
	var asked string
	srv := archiveStub(t, &hits, &asked)
	defer srv.Close()
	w := weatherUnderTest(t, srv.URL, true)

	// 13:47:38 is 79.4% of the way from the 13:00 reading to the 14:00 one,
	// and the answer must be that point rather than either end: the hour a
	// session starts in is up to an hour before it started, always on the
	// cool side of a warming morning.
	when := time.Date(2026, 8, 13, 13, 47, 38, 0, time.UTC)
	c := w.at(44.9778, -93.2650, when)
	if c == nil {
		t.Fatal("no conditions")
	}
	if c.TempF != 70.5 || c.DewF != 65.9 || c.RH != 85 || c.WindMPH != 8.7 {
		t.Errorf("not interpolated to the start: %+v (13:00 was 68.4/65.6, 14:00 was 71/66)", c)
	}
	// The athlete's own measure, so it must be the sum and not something
	// re-derived elsewhere.
	if c.HeatSum != 136.4 {
		t.Errorf("heat sum = %v, want 136.4", c.HeatSum)
	}
	// On the hour exactly, there is nothing to interpolate.
	if onHour := w.at(44.9778, -93.2650, when.Truncate(time.Hour)); onHour == nil ||
		onHour.TempF != 68.4 || onHour.DewF != 65.6 {
		t.Errorf("on the hour = %+v, want the 13:00 reading unchanged", onHour)
	}
	// Halfway is the mean of the two.
	if mid := w.at(44.9778, -93.2650, when.Truncate(time.Hour).Add(30*time.Minute)); mid == nil ||
		mid.TempF != 69.7 {
		t.Errorf("halfway = %+v, want 69.7 between 68.4 and 71", mid)
	}

	// Coarse, before it leaves: a tenth of a degree is about eleven km.
	lat, lon, _ := parseAsked(t, asked)
	if lat != 45.0 || lon != -93.3 {
		t.Errorf("position sent as %v,%v — wanted it rounded to 45.0,-93.3", lat, lon)
	}

	// A position a few hundred metres away rounds to the same place, and
	// every one of these reads the hourly readings already cached — what is
	// stored is the provider's facts, and the interpolation is derived each
	// time from them.
	if c3 := w.at(45.0201, -93.2501, when); c3 == nil || c3.TempF != c.TempF {
		t.Errorf("nearby position missed the cache: %+v", c3)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("%d requests, want 1", n)
	}
}

// TestWeatherCrossesTheDateLine: an evening session is on one calendar day
// where the athlete lives and the next one in UTC. The day asked for and
// the hour matched must come off the SAME clock — ask for the local date
// while matching a UTC label and the reading is simply never found, which
// looks like a provider with no data rather than a bug here.
func TestWeatherCrossesTheDateLine(t *testing.T) {
	chi := chicago(t)
	// 19:30 on the 13th in Chicago is 00:30 on the 14th in UTC.
	local := time.Date(2026, 8, 13, 19, 30, 0, 0, chi)
	wantHour := "2026-08-14T00:00"
	wantDay := "2026-08-14"

	var askedDay, askedTZ string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		askedDay = r.URL.Query().Get("start_date")
		askedTZ = r.URL.Query().Get("timezone")
		_ = json.NewEncoder(w).Encode(map[string]any{"hourly": map[string]any{
			"time":                 []string{"2026-08-14T00:00", "2026-08-14T01:00"},
			"temperature_2m":       []float64{71.5, 70.0},
			"dew_point_2m":         []float64{62.5, 62.0},
			"relative_humidity_2m": []float64{74, 76},
			"wind_speed_10m":       []float64{5, 4},
		}})
	}))
	defer srv.Close()

	w := weatherUnderTest(t, srv.URL, true)
	c := w.at(44.98, -93.27, local)
	if c == nil {
		t.Fatal("no conditions for an evening session")
	}
	if askedDay != wantDay {
		t.Errorf("asked for %s, want %s — the date must be the one the hour is on", askedDay, wantDay)
	}
	if askedTZ != "UTC" {
		t.Errorf("timezone = %q; the labels must be the clock the hour is matched in", askedTZ)
	}
	// 00:30 is halfway between the two readings on the next UTC day.
	if c.TempF != 70.8 || c.DewF != 62.2 || c.HeatSum != 133.0 {
		t.Errorf("read %+v, wanted %s and 01:00 averaged", c, wantHour)
	}
}

// TestWeatherIsFrozenAtImport: the conditions are read once, when the
// activity is imported, and stored on its row. Reading the metrics again —
// a month later, after the provider has revised its reanalysis — must
// return what was captured then, and must not ask anyone anything. A grade
// has to go on citing the weather it was made against.
func TestWeatherIsFrozenAtImport(t *testing.T) {
	var hits int32
	temp := 70.0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		day := r.URL.Query().Get("start_date")
		_ = json.NewEncoder(w).Encode(map[string]any{"hourly": map[string]any{
			"time":                 []string{day + "T12:00", day + "T13:00"},
			"temperature_2m":       []float64{temp, temp},
			"dew_point_2m":         []float64{60, 60},
			"relative_humidity_2m": []float64{70, 70},
			"wind_speed_10m":       []float64{5, 5},
		}})
	}))
	defer srv.Close()
	t.Setenv("WEATHER_BASE_URL", srv.URL)
	t.Setenv("WEATHER_LOOKUP", "on")

	dir := t.TempDir()
	db, err := openMetricsDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.close()
	wx := newWeatherService(db)

	const name = "2026-08-01-12-00-00.fit"
	m, err := db.importOne(name, runWithPosition(t), time.UTC, wx)
	if err != nil {
		t.Fatal(err)
	}
	if m.Weather == nil || m.Weather.TempF != 70.0 {
		t.Fatalf("import did not capture the weather: %+v", m.Weather)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("%d lookups during import, want 1", n)
	}

	// The world moves on: the provider now says something else.
	temp = 99.0

	row, err := db.rowByName(name)
	if err != nil || row == nil {
		t.Fatalf("row: %v", err)
	}
	if row.Weather == nil || row.Weather.TempF != 70.0 {
		t.Errorf("reading the row returned %+v, want the 70.0 captured at import", row.Weather)
	}
	if row.Weather.HeatSum != 130.0 {
		t.Errorf("heat sum = %v, want 130 from the stored parts", row.Weather.HeatSum)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("reading the row asked the provider again (%d calls)", n)
	}
}

// degreesToSemicircles is the inverse of the decoder's conversion.
func degreesToSemicircles(deg float64) int32 {
	return int32(deg / (180.0 / 2147483648.0))
}

// runWithPosition is a short run that knows where it was — the only kind
// weather can be looked up for.
func runWithPosition(t *testing.T) []byte {
	t.Helper()
	msgs := []proto.Message{}
	for i := 0; i <= 10; i++ {
		r := mesgdef.NewRecord(nil).
			SetTimestamp(fixtureT0.Add(time.Duration(i) * time.Second)).
			SetHeartRate(uint8(120 + i)).
			SetEnhancedSpeed(3000).
			// Semicircles: about 45.0N, 93.3W.
			SetPositionLat(degreesToSemicircles(44.98)).
			SetPositionLong(degreesToSemicircles(-93.27))
		msgs = append(msgs, r.ToMesg(nil))
	}
	msgs = append(msgs, sessionMsg(typedef.SportRunning, 100_00))
	return encodeActivityFixture(t, msgs...)
}

// TestVirtualRidesGetNoWeather: a trainer session records a position like
// any other — Zwift writes its own world's, and Watopia sits in the
// Solomon Islands — so an indoor ride was having South Pacific weather
// frozen onto it, 79F with a dew point of 73, ready to explain a heart
// rate by heat that never existed. Indoors is checked before anyone asks.
func TestVirtualRidesGetNoWeather(t *testing.T) {
	var hits int32
	srv := archiveStub(t, &hits, nil)
	defer srv.Close()
	t.Setenv("WEATHER_BASE_URL", srv.URL)
	t.Setenv("WEATHER_LOOKUP", "on")

	db, err := openMetricsDB(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.close()
	wx := newWeatherService(db)

	m, err := db.importOne("2026-08-05-08-30-42.fit", virtualRide(t), time.UTC, wx)
	if err != nil {
		t.Fatal(err)
	}
	if m.Weather != nil {
		t.Errorf("a virtual ride was given the weather at %+v", m.Weather)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Errorf("asked the provider %d times about a room", n)
	}

	for _, sub := range []string{"virtual_activity", "indoor_cycling", "treadmill",
		"indoor_running", "spin", "some_new_indoor_thing", "virtual_whatever"} {
		if !indoors(sub) {
			t.Errorf("indoors(%q) = false", sub)
		}
	}
	for _, sub := range []string{"generic", "road", "trail", "track_cycling", ""} {
		if indoors(sub) {
			t.Errorf("indoors(%q) = true", sub)
		}
	}
}

// virtualRide is a trainer session carrying Watopia's coordinates, the way
// the real ones do.
func virtualRide(t *testing.T) []byte {
	t.Helper()
	msgs := []proto.Message{}
	for i := 0; i <= 10; i++ {
		msgs = append(msgs, mesgdef.NewRecord(nil).
			SetTimestamp(fixtureT0.Add(time.Duration(i)*time.Second)).
			SetHeartRate(uint8(120+i)).
			SetPower(200).
			SetPositionLat(degreesToSemicircles(-11.639)).
			SetPositionLong(degreesToSemicircles(166.982)).ToMesg(nil))
	}
	msgs = append(msgs, mesgdef.NewSession(nil).
		SetSport(typedef.SportCycling).
		SetSubSport(typedef.SubSportVirtualActivity).
		SetStartTime(fixtureT0).
		SetTimestamp(fixtureT0.Add(time.Minute)).
		SetTotalDistance(500_00).ToMesg(nil))
	return encodeActivityFixture(t, msgs...)
}

func TestWeatherOffMakesNoRequest(t *testing.T) {
	var hits int32
	srv := archiveStub(t, &hits, nil)
	defer srv.Close()
	w := weatherUnderTest(t, srv.URL, false)

	if c := w.at(44.98, -93.27, time.Now()); c != nil {
		t.Errorf("switched off and still answered: %+v", c)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Errorf("switched off and still called out %d times", n)
	}
}

// TestWeatherFailuresCostOnlyTheWeather: a provider that errors, hangs up,
// talks nonsense, or has no reading for the hour must return nothing and
// never panic — a grade is made without it.
func TestWeatherFailuresCostOnlyTheWeather(t *testing.T) {
	for _, c := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"500", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", 500) }},
		{"garbage", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "{{{") }},
		{"empty", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"hourly":{"time":[]}}`) }},
		{"short arrays", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"hourly":{"time":["2026-08-13T13:00"],"temperature_2m":[]}}`)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(c.handler)
			defer srv.Close()
			w := weatherUnderTest(t, srv.URL, true)
			if got := w.at(44.98, -93.27, time.Date(2026, 8, 13, 13, 0, 0, 0, time.UTC)); got != nil {
				t.Errorf("returned %+v from a broken provider", got)
			}
		})
	}
	// And with no service at all behind the pointer.
	var none *weatherService
	if got := none.at(1, 2, time.Now()); got != nil {
		t.Errorf("nil service returned %+v", got)
	}
}

func TestCoarsePosition(t *testing.T) {
	for _, c := range []struct{ in, want float64 }{
		{44.9778, 45.0}, {-93.2650, -93.3}, {0, 0}, {44.94, 44.9}, {-0.04, -0.0},
	} {
		if got := coarse(c.in); got != c.want {
			t.Errorf("coarse(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func parseAsked(t *testing.T, s string) (float64, float64, error) {
	t.Helper()
	var lat, lon float64
	var err error
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			if lat, err = strconv.ParseFloat(s[:i], 64); err != nil {
				t.Fatalf("latitude %q: %v", s[:i], err)
			}
			if lon, err = strconv.ParseFloat(s[i+1:], 64); err != nil {
				t.Fatalf("longitude %q: %v", s[i+1:], err)
			}
			return lat, lon, nil
		}
	}
	t.Fatalf("could not read %q as a position", s)
	return 0, 0, nil
}
