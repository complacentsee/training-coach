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

	when := time.Date(2026, 8, 13, 13, 47, 38, 0, time.UTC)
	c := w.at(44.9778, -93.2650, when)
	if c == nil {
		t.Fatal("no conditions")
	}
	if c.TempF != 68.4 || c.DewF != 65.6 || c.RH != 91 || c.WindMPH != 7.5 {
		t.Errorf("read the wrong hour: %+v", c)
	}
	// The athlete's own measure, so it must be the sum and not something
	// re-derived elsewhere.
	if c.HeatSum != 134.0 {
		t.Errorf("heat sum = %v, want 134", c.HeatSum)
	}

	// Coarse, before it leaves: a tenth of a degree is about eleven km.
	lat, lon, _ := parseAsked(t, asked)
	if lat != 45.0 || lon != -93.3 {
		t.Errorf("position sent as %v,%v — wanted it rounded to 45.0,-93.3", lat, lon)
	}

	// The same hour, and a different minute of it, must not ask again.
	if c2 := w.at(44.9778, -93.2650, when.Add(11*time.Minute)); c2 == nil || c2.TempF != c.TempF {
		t.Errorf("cache miss: %+v", c2)
	}
	// A position a few hundred metres away rounds to the same place.
	if c3 := w.at(45.0201, -93.2501, when); c3 == nil || c3.TempF != c.TempF {
		t.Errorf("nearby position missed the cache: %+v", c3)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("%d requests, want 1", n)
	}
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
