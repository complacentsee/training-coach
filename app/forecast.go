package main

// The morning's forecast on the today card, and the LT test's gate read
// mechanically. A Minneapolis summer decides what hour a long run can
// start, and the threshold test's "temp + dew point under 140" was prose
// the athlete had to check against a weather app. This asks Open-Meteo's
// FORECAST endpoint — the weather service beside it reads only HISTORY,
// and that stays untouched: what was frozen onto a recording at import
// is a fact about a moment and never comes from here.
//
// Rules, because this is a new outbound call on a page path:
//
//   - The cache is in memory, one place, an hour old at most. A page that
//     finds it fresh reads it; one that finds it stale or empty starts ONE
//     background refresh and renders whatever it has, which may be nothing.
//     No request ever waits on the network.
//   - It is on only where the history lookup is (WEATHER_LOOKUP=on), and
//     FORECAST_BASE_URL points tests at a stub the way WEATHER_BASE_URL does.
//   - Where is "where the athlete runs": the coarse position of the most
//     recent outdoor session the history cache holds, a tenth of a degree.
//     No position, no forecast. Nothing new is recorded about the athlete.
//   - What shows is one line: the three morning hours for the day whose
//     session is outdoors and still to come — today until its run is
//     recorded or the morning is over, then tomorrow — and on an LT-tagged
//     day the gate, evaluated: the first morning hour under 140, or that
//     none is.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const forecastURL = "https://api.open-meteo.com/v1/forecast"
const forecastTTL = time.Hour
const forecastTimeout = 10 * time.Second

// ltGateHeatSum is the threshold test's gate, temp + dew point in °F.
const ltGateHeatSum = 140

// forecastHour is one hour of the provider's forecast, in the athlete's
// zone, in the units the history cache keeps (°F, mph).
type forecastHour struct {
	At      time.Time
	TempF   float64
	DewF    float64
	WindMPH float64
	RainPct int
}

type forecastService struct {
	enabled bool
	http    *http.Client
	now     func() time.Time
	mu      sync.Mutex
	lat     float64
	lon     float64
	at      time.Time
	cache   []forecastHour
	busy    bool
}

func newForecastService(enabled bool) *forecastService {
	return &forecastService{enabled: enabled, http: &http.Client{Timeout: forecastTimeout}, now: time.Now}
}

// hours is the forecast for a place, if the cache holds a fresh one. A
// stale or missing cache starts one background fetch and returns what is
// there — nil the first time — so the page never waits.
func (f *forecastService) hours(lat, lon float64, loc *time.Location) []forecastHour {
	if f == nil || !f.enabled {
		return nil
	}
	lat, lon = coarse(lat), coarse(lon)
	f.mu.Lock()
	defer f.mu.Unlock()
	same := f.lat == lat && f.lon == lon
	fresh := same && f.now().Sub(f.at) < forecastTTL
	if !fresh && !f.busy {
		f.busy = true
		go f.refresh(lat, lon, loc)
	}
	if !same {
		return nil
	}
	return f.cache
}

func (f *forecastService) refresh(lat, lon float64, loc *time.Location) {
	hours, err := f.fetch(lat, lon, loc)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.busy = false
	if err != nil {
		log.Printf("forecast: %v", err)
		return
	}
	f.lat, f.lon, f.at, f.cache = lat, lon, f.now(), hours
}

func (f *forecastService) fetch(lat, lon float64, loc *time.Location) ([]forecastHour, error) {
	q := url.Values{
		"latitude":         {fmt.Sprintf("%.1f", lat)},
		"longitude":        {fmt.Sprintf("%.1f", lon)},
		"hourly":           {"temperature_2m,dew_point_2m,wind_speed_10m,precipitation_probability"},
		"temperature_unit": {"fahrenheit"},
		"wind_speed_unit":  {"mph"},
		"timezone":         {loc.String()},
		"forecast_days":    {"2"},
	}
	base := envOr("FORECAST_BASE_URL", forecastURL)
	ctx, cancel := context.WithTimeout(context.Background(), forecastTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", base+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.http.Do(req)
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
			Wind []float64 `json:"wind_speed_10m"`
			Rain []float64 `json:"precipitation_probability"`
		} `json:"hourly"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	var hours []forecastHour
	for i, ts := range out.Hourly.Time {
		if i >= len(out.Hourly.Temp) || i >= len(out.Hourly.Dew) {
			break
		}
		// Labelled in the zone asked for, the athlete's.
		at, err := time.ParseInLocation("2006-01-02T15:04", ts, loc)
		if err != nil {
			continue
		}
		h := forecastHour{At: at, TempF: out.Hourly.Temp[i], DewF: out.Hourly.Dew[i]}
		if i < len(out.Hourly.Wind) {
			h.WindMPH = out.Hourly.Wind[i]
		}
		if i < len(out.Hourly.Rain) {
			h.RainPct = int(out.Hourly.Rain[i])
		}
		hours = append(hours, h)
	}
	if len(hours) == 0 {
		return nil, fmt.Errorf("no hours in the forecast")
	}
	return hours, nil
}

// lastPosition is the coarse place of the most recent session the weather
// cache holds a reading for — where the athlete runs, to a tenth of a
// degree, derived from the archive rather than declared.
func (m *metricsDB) lastPosition() (lat, lon float64, ok bool) {
	err := m.r.QueryRow(`SELECT lat, lon FROM weather ORDER BY hour_utc DESC LIMIT 1`).Scan(&lat, &lon)
	return lat, lon, err == nil
}

// forecastView is the line the today card shows.
type forecastView struct {
	Day  string // "Today" or "Tomorrow"
	Line string // "6 am 64°/58° · 7 am 66°/59° · 8 am 69°/61° · wind 7 mph · rain 10%"
	Gate string // the LT gate, evaluated, on an LT-tagged day; "" otherwise
}

// forecastFor decides which day's morning to show and renders it. today
// is the athlete-local day; now the athlete-local clock.
func (s *server) forecastFor(d *dataset, blk *Block, today time.Time, now time.Time, todayRecorded bool) *forecastView {
	if s.forecast == nil || !s.forecast.enabled || s.metrics == nil || blk == nil {
		return nil
	}
	outdoor := func(day time.Time) (Session, bool) {
		wk, di, ok := blk.Locate(day)
		if !ok {
			return Session{}, false
		}
		sess := wk.Days[di]
		return sess, sess.Kind.IsRun()
	}
	var target time.Time
	var label string
	var sess Session
	if se, ok := outdoor(today); ok && !todayRecorded && now.Hour() < 12 {
		target, label, sess = today, "Today", se
	} else if se, ok := outdoor(today.AddDate(0, 0, 1)); ok {
		target, label, sess = today.AddDate(0, 0, 1), "Tomorrow", se
	} else {
		return nil
	}
	lat, lon, ok := s.metrics.lastPosition()
	if !ok {
		return nil
	}
	hours := s.forecast.hours(lat, lon, d.Loc)
	if len(hours) == 0 {
		return nil
	}
	byHour := map[int]forecastHour{}
	for _, h := range hours {
		if h.At.Year() == target.Year() && h.At.YearDay() == target.YearDay() {
			byHour[h.At.Hour()] = h
		}
	}
	u := s.units()
	temp := func(f float64) string {
		if u == Metric {
			return fmt.Sprintf("%.0f°", (f-32)*5/9)
		}
		return fmt.Sprintf("%.0f°", f)
	}
	var parts []string
	for _, hr := range []int{6, 7, 8} {
		h, ok := byHour[hr]
		if !ok {
			continue
		}
		parts = append(parts, fmt.Sprintf("%d am %s/%s", hr, temp(h.TempF), temp(h.DewF)))
	}
	if len(parts) == 0 {
		return nil
	}
	if h, ok := byHour[7]; ok {
		if u == Metric {
			parts = append(parts, fmt.Sprintf("wind %.0f km/h", h.WindMPH*1.609344))
		} else {
			parts = append(parts, fmt.Sprintf("wind %.0f mph", h.WindMPH))
		}
		if h.RainPct > 0 {
			parts = append(parts, fmt.Sprintf("rain %d%%", h.RainPct))
		}
	}
	v := &forecastView{Day: label, Line: strings.Join(parts, " · ")}
	if sess.Tag == "LT" {
		v.Gate = ltGate(byHour)
	}
	return v
}

// ltGate evaluates "temp + dew point under 140" over the morning hours,
// 5 to 9 am, and says the first hour it holds — or that none does, with
// the numbers. Stated as a fact for the athlete to act on; the guide's
// own words on the gate are unchanged.
func ltGate(byHour map[int]forecastHour) string {
	var seen []string
	for _, hr := range []int{5, 6, 7, 8, 9} {
		h, ok := byHour[hr]
		if !ok {
			continue
		}
		sum := h.TempF + h.DewF
		if sum < ltGateHeatSum {
			return fmt.Sprintf("LT gate (temp + dew point under %d): %.0f at %d am — go", ltGateHeatSum, sum, hr)
		}
		seen = append(seen, fmt.Sprintf("%.0f at %d am", sum, hr))
	}
	if len(seen) == 0 {
		return ""
	}
	return fmt.Sprintf("LT gate (temp + dew point under %d): not met before 9 am — %s", ltGateHeatSum, strings.Join(seen, ", "))
}
