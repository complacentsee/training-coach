package main

// What the conditions were where a session started. Heat is the loudest
// uncontrolled variable in an HR-governed plan: the same effort on a humid
// morning costs several beats, and a grade that does not know the weather
// reads that as a worse session. The athlete has been recording it by hand
// as a heat sum — air temperature plus dew point, both Fahrenheit — so
// that convention is what this reports.
//
// It is OFF unless WEATHER_LOOKUP=on. The stack that grades turns it on;
// the one that only records the athlete's training makes no outbound calls
// at all, and that is a property worth keeping deliberate.
//
// Privacy: the run's start position is rounded to a tenth of a degree —
// about eleven kilometres — before it leaves the machine. That is far finer
// than weather varies and far coarser than a home address. Nothing stores
// the precise position: it is read from the archive's bytes for this
// lookup and dropped. The provider (Open-Meteo) requires no account and no
// key, so the request carries no identity beyond a coarse coordinate and a
// past date.
//
// Every failure here is silent and total: no weather, no grade input, never
// an error that reaches the athlete. A session graded without the weather
// is a session graded the way every session was graded before this existed.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const weatherArchiveURL = "https://archive-api.open-meteo.com/v1/archive"

// weatherLookupTimeout bounds the call. Weather is a nicety; a grade must
// not wait on it.
const weatherLookupTimeout = 20 * time.Second

type conditions struct {
	TempF   float64 `json:"temp_f"`
	DewF    float64 `json:"dew_f"`
	RH      int     `json:"humidity_pct"`
	WindMPH float64 `json:"wind_mph"`
	// HeatSum is temp + dew point in Fahrenheit, the athlete's own measure:
	// under about 120 is comfortable, 130 and up is a real tax on pace at a
	// given heart rate.
	HeatSum float64 `json:"heat_sum"`
	Source  string  `json:"source"`
}

// indoorSubSports are the ways of training where the weather outside is
// not the weather. A virtual ride still records a position: Zwift writes
// its own world's coordinates, and a session in Watopia looks from the
// outside like a ride in the South Pacific — which was frozen onto a
// basement FTP test as 79F with a dew point of 73 before this existed.
var indoorSubSports = map[string]bool{
	"virtual_activity": true, "indoor_cycling": true, "indoor_running": true,
	"indoor_rowing": true, "indoor_walking": true, "treadmill": true,
	"spin": true, "elliptical": true, "stair_climbing": true,
}

// indoors reports whether a session happened somewhere the sky does not
// reach. The substring test catches the sub-sports this list has not met.
func indoors(subSport string) bool {
	if indoorSubSports[subSport] {
		return true
	}
	return strings.Contains(subSport, "indoor") || strings.Contains(subSport, "virtual")
}

// coarse rounds a coordinate to a tenth of a degree.
func coarse(v float64) float64 { return math.Round(v*10) / 10 }

type weatherService struct {
	db      *metricsDB
	http    *http.Client
	enabled bool
}

func newWeatherService(db *metricsDB) *weatherService {
	return &weatherService{
		db:      db,
		http:    &http.Client{Timeout: weatherLookupTimeout},
		enabled: envOr("WEATHER_LOOKUP", "off") == "on",
	}
}

// at reports the conditions where and when a session started.
//
// The provider reports on the hour and sessions do not start on the hour,
// so the two readings either side are interpolated to the actual minute.
// Truncating to the hour instead is wrong in a consistent direction: on the
// morning of one measured run the temperature climbed 4.4°F between 08:00
// and 09:00, and reading 08:00 for an 08:43 start understated the heat by
// over two degrees — always cooler than it was, on exactly the mornings
// where heat matters most. What is cached is the provider's hourly
// readings, which are facts; the interpolation is derived at every use.
//
// A miss of any kind returns nil.
func (w *weatherService) at(lat, lon float64, when time.Time) *conditions {
	if w == nil || !w.enabled || w.db == nil {
		return nil
	}
	la, lo := coarse(lat), coarse(lon)
	t := when.UTC()
	h0 := t.Truncate(time.Hour)
	h1 := h0.Add(time.Hour)

	a, b := w.cached(la, lo, h0), w.cached(la, lo, h1)
	if a == nil || b == nil {
		if err := w.fetchDay(la, lo, h0); err != nil {
			log.Printf("weather %.1f,%.1f %s: %v", la, lo, h0.Format(time.RFC3339), err)
		}
		a, b = w.cached(la, lo, h0), w.cached(la, lo, h1)
	}
	if a == nil {
		return nil
	}
	if b == nil {
		// The hour after is off the end of the archive; the hour the
		// session began in is still the honest answer.
		return finish(a, "open-meteo, hour of the start")
	}
	f := float64(t.Sub(h0)) / float64(time.Hour)
	c := &conditions{
		TempF:   a.TempF + (b.TempF-a.TempF)*f,
		DewF:    a.DewF + (b.DewF-a.DewF)*f,
		WindMPH: a.WindMPH + (b.WindMPH-a.WindMPH)*f,
		RH:      int(math.Round(float64(a.RH) + float64(b.RH-a.RH)*f)),
	}
	return finish(c, "open-meteo, interpolated to the start")
}

// finish rounds a reading for reporting and states where it came from.
func finish(c *conditions, source string) *conditions {
	out := *c
	out.TempF = pyRound(out.TempF, 1)
	out.DewF = pyRound(out.DewF, 1)
	out.WindMPH = pyRound(out.WindMPH, 1)
	out.HeatSum = pyRound(out.TempF+out.DewF, 1)
	out.Source = source
	return &out
}

func (w *weatherService) cached(la, lo float64, hour time.Time) *conditions {
	var c conditions
	err := w.db.r.QueryRow(`SELECT temp_f, dew_f, humidity_pct, wind_mph FROM weather
		WHERE lat=? AND lon=? AND hour_utc=?`,
		la, lo, hour.Format(time.RFC3339)).Scan(&c.TempF, &c.DewF, &c.RH, &c.WindMPH)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		log.Printf("weather cache: %v", err)
		return nil
	}
	return &c
}

func (w *weatherService) store(la, lo float64, hour time.Time, c *conditions) {
	w.db.mu.Lock()
	defer w.db.mu.Unlock()
	if _, err := w.db.w.Exec(`INSERT OR REPLACE INTO weather VALUES(?,?,?,?,?,?,?,?)`,
		la, lo, hour.Format(time.RFC3339), c.TempF, c.DewF, c.RH, c.WindMPH,
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		log.Printf("weather cache write: %v", err)
	}
}

// fetchDay reads the hours around a session and caches every one of them.
// The range runs to the next day because a session late in the UTC day
// needs the hour after it to interpolate, and because the whole day costs
// the same one request as a single hour does.
func (w *weatherService) fetchDay(la, lo float64, hour time.Time) error {
	day := hour.Format("2006-01-02")
	next := hour.Add(24 * time.Hour).Format("2006-01-02")
	q := url.Values{
		"latitude":         {fmt.Sprintf("%.1f", la)},
		"longitude":        {fmt.Sprintf("%.1f", lo)},
		"start_date":       {day},
		"end_date":         {next},
		"hourly":           {"temperature_2m,dew_point_2m,relative_humidity_2m,wind_speed_10m"},
		"temperature_unit": {"fahrenheit"},
		"wind_speed_unit":  {"mph"},
		"timezone":         {"UTC"},
	}
	base := envOr("WEATHER_BASE_URL", weatherArchiveURL) // tests point this at a stub
	ctx, cancel := context.WithTimeout(context.Background(), weatherLookupTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", base+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := w.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", resp.Status, body)
	}
	var out struct {
		Hourly struct {
			Time []string  `json:"time"`
			Temp []float64 `json:"temperature_2m"`
			Dew  []float64 `json:"dew_point_2m"`
			RH   []float64 `json:"relative_humidity_2m"`
			Wind []float64 `json:"wind_speed_10m"`
		} `json:"hourly"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return err
	}
	kept := 0
	for i, ts := range out.Hourly.Time {
		if i >= len(out.Hourly.Temp) || i >= len(out.Hourly.Dew) {
			break
		}
		// The provider labels its hours in the zone it was asked for, which
		// is UTC, so this parses as UTC and the hours line up with the ones
		// looked up.
		at, err := time.ParseInLocation("2006-01-02T15:04", ts, time.UTC)
		if err != nil {
			continue
		}
		c := &conditions{TempF: out.Hourly.Temp[i], DewF: out.Hourly.Dew[i]}
		if i < len(out.Hourly.RH) {
			c.RH = int(math.Round(out.Hourly.RH[i]))
		}
		if i < len(out.Hourly.Wind) {
			c.WindMPH = out.Hourly.Wind[i]
		}
		w.store(la, lo, at, c)
		kept++
	}
	if kept == 0 {
		return fmt.Errorf("no hourly readings returned")
	}
	return nil
}
