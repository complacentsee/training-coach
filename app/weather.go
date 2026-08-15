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

// at reports the conditions where and when a session started, from the
// cache when it has them and from the provider otherwise. A miss of any
// kind returns nil.
func (w *weatherService) at(lat, lon float64, when time.Time) *conditions {
	if w == nil || !w.enabled || w.db == nil {
		return nil
	}
	la, lo := coarse(lat), coarse(lon)
	hour := when.UTC().Truncate(time.Hour)
	if c := w.cached(la, lo, hour); c != nil {
		return c
	}
	c, err := w.fetch(la, lo, hour)
	if err != nil {
		log.Printf("weather %.1f,%.1f %s: %v", la, lo, hour.Format(time.RFC3339), err)
		return nil
	}
	w.store(la, lo, hour, c)
	return c
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
	c.HeatSum = pyRound(c.TempF+c.DewF, 1)
	c.Source = "open-meteo (cached)"
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

func (w *weatherService) fetch(la, lo float64, hour time.Time) (*conditions, error) {
	day := hour.Format("2006-01-02")
	q := url.Values{
		"latitude":         {fmt.Sprintf("%.1f", la)},
		"longitude":        {fmt.Sprintf("%.1f", lo)},
		"start_date":       {day},
		"end_date":         {day},
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
		return nil, err
	}
	resp, err := w.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", resp.Status, body)
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
		return nil, err
	}
	want := hour.Format("2006-01-02T15:04")
	for i, t := range out.Hourly.Time {
		if t != want {
			continue
		}
		if i >= len(out.Hourly.Temp) || i >= len(out.Hourly.Dew) {
			break
		}
		c := &conditions{
			TempF: out.Hourly.Temp[i], DewF: out.Hourly.Dew[i],
			Source: "open-meteo",
		}
		if i < len(out.Hourly.RH) {
			c.RH = int(math.Round(out.Hourly.RH[i]))
		}
		if i < len(out.Hourly.Wind) {
			c.WindMPH = out.Hourly.Wind[i]
		}
		c.HeatSum = pyRound(c.TempF+c.DewF, 1)
		return c, nil
	}
	return nil, fmt.Errorf("no reading for %s", want)
}
