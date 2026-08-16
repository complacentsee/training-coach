package main

// What an activity LOOKED like: laps, session scalars, the route. This file
// is display-only and deliberately outside the measurement register — no
// grade depends on anything decoded here, decode.go and metrics.go remain
// the authoritative conversion and measurement halves, and there is no
// python mirror to keep in step. A field joins the register when a grade
// depends on it, and not before (Adam, 15 Aug 2026).
//
// Conversion conventions — change them here or not at all:
//
//   - Times are seconds. FIT stores lap and session times at scale 1000, so
//     everything here divides by 1000 and keeps the tenths the watch wrote.
//   - A lap's ELAPSED time includes its stops and its TIMER time does not,
//     and both travel: on 2026-08-12 a 180 s interval carries elapsed 475.8
//     because the athlete stopped inside the rep, and a lap table that
//     showed one of the two would have hidden either the stop or the work.
//     Nothing may assert timer <= elapsed: Zwift writes timer one second
//     LARGER than elapsed, measured across its files.
//   - A lap's pace is computed from its own distance and TIMER time rather
//     than read from avg_speed, because that is the pace the athlete ran
//     rather than a number the device rounded. avg_speed is carried anyway,
//     and it is a component-expansion field: avg_speed(13) expands to
//     enhanced_avg_speed(110). On this watch 110 is present on every lap and
//     13 is the absent one — the opposite of the guess — while on Zwift
//     files muktihari SYNTHESIZES 110 from 13. Expansion is therefore left
//     ON here (decode.go turns it off for a second pass precisely because
//     the register cares about the ulp; display does not), and enhanced is
//     read first with plain avg_speed as the fallback.
//   - Distance is the file's own odometer — session total_distance, and
//     per-lap total_distance — never integrated from speed. This is the
//     opposite choice from fastestSegments, which integrates because it
//     measures stretches the file does not delimit.
//   - Ascent is total_ascent as recorded, whole metres. Laps truncate and do
//     not sum to the session's figure (120 against 124 on one measured run);
//     both are reported as they are, and neither is derived from the other.
//   - Position is degrees from semicircles, and a track is served ONLY when
//     the session was not indoors. A trainer session carries GPS — Zwift
//     writes its own world's, and Watopia is in the Solomon Islands — so
//     indoors() gates the route the way it already gates the weather. An
//     indoor ride reports indoor:true and no track at all, because a South
//     Pacific coastline where a neighbourhood belongs is worse than an
//     honest empty state.
//   - The track is every fixed point, encoded as a Google polyline at five
//     decimal places (~1 m). It is NOT simplified: Douglas-Peucker at a 3 m
//     tolerance replaced 1,904.8 m of a measured out-and-back with a 464.9 m
//     chord while reporting itself in tolerance, and it destroys the index
//     correspondence per-lap colouring needs.
//   - The CRC fallback is decode.go's, for the same measured reason: Zwift
//     writes the whole-file CRC with a zero header-CRC slot.

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/muktihari/fit/decoder"
	"github.com/muktihari/fit/profile/basetype"
	"github.com/muktihari/fit/profile/mesgdef"
	"github.com/muktihari/fit/profile/typedef"
	"github.com/muktihari/fit/profile/untyped/mesgnum"
)

// detailLap is one lap as the watch recorded it. Which fields a file
// carries varies by device and by decade — the 1,133 archive files under
// 200 KB predate most of this — so everything optional is a pointer and an
// absent field is absent, never zero.
type detailLap struct {
	StartS   int     // seconds from the first record
	ElapsedS float64 // includes stops
	TimerS   float64 // excludes them
	DistM    float64
	Trigger  string // distance | manual | time | session_end | …
	// Step is the index of the prescribed workout step this lap was, when
	// the watch was driving a pushed workout. A repeat block's steps repeat,
	// so this names the step and not the rep.
	Step       *int
	AvgHR      *int
	MaxHR      *int
	AvgCadence *int
	AvgPower   *int
	MaxPower   *int
	AscentM    *int
	AvgSpeed   *float64 // m/s, as recorded
	StartLat   *float64
	StartLon   *float64
	EndLat     *float64
	EndLon     *float64
}

// detailSplit is one split_summary row: how much of a run was running and
// how much was walking, by the device's own reckoning. The walk-break
// dilution a grade note otherwise corrects in prose is a stored scalar.
type detailSplit struct {
	Type   string // rwd_run, rwd_walk, …
	TimerS float64
	DistM  float64
	Splits *int
}

// activityDetail is one activity decoded for display.
type activityDetail struct {
	Sport, SubSport string
	Indoor          bool
	StartUTC        time.Time
	ElapsedS        float64 // session total_elapsed_time
	TimerS          float64 // session total_timer_time — the watch's moving time
	DistM           float64
	AscentM         *int
	Laps            []detailLap
	Splits          []detailSplit
	Track           []trackPoint
}

type trackPoint struct{ Lat, Lon float64 }

// scaled turns a FIT integer at a given scale into a float, reporting
// whether the field was present at all.
func scaled(v uint32, invalid uint32, scale float64) (float64, bool) {
	if v == invalid {
		return 0, false
	}
	return float64(v) / scale, true
}

func u8p(v uint8) *int {
	if v == basetype.Uint8Invalid {
		return nil
	}
	n := int(v)
	return &n
}

func u16p(v uint16) *int {
	if v == basetype.Uint16Invalid {
		return nil
	}
	n := int(v)
	return &n
}

func degrees(v int32) *float64 {
	if v == basetype.Sint32Invalid {
		return nil
	}
	d := semicirclesToDegrees(v)
	return &d
}

// decodeDetail walks the stored bytes once for everything a page shows.
// Records are read for their timestamps and positions only; every measured
// quantity comes from the lap and session messages the device wrote.
func decodeDetail(data []byte) (*activityDetail, error) {
	d, err := decodeDetailOpt(data, false)
	if err != nil && strings.Contains(err.Error(), "crc checksum mismatch") && wholeFileFITCRCOK(data) {
		return decodeDetailOpt(data, true)
	}
	return d, err
}

func decodeDetailOpt(data []byte, skipCRC bool) (*activityDetail, error) {
	var opts []decoder.Option
	if skipCRC {
		opts = append(opts, decoder.WithIgnoreChecksum())
	}
	dec := decoder.New(bytes.NewReader(data), opts...)

	out := &activityDetail{Sport: "unknown"}
	var laps []*mesgdef.Lap
	type fix struct {
		t        time.Time
		lat, lon float64
	}
	var fixes []fix
	var t0 time.Time
	var haveT0 bool

	for dec.Next() {
		fit, err := dec.Decode()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		for i := range fit.Messages {
			m := &fit.Messages[i]
			switch m.Num {
			case mesgnum.Record:
				r := mesgdef.NewRecord(m)
				if r.Timestamp.IsZero() {
					continue
				}
				if !haveT0 {
					t0, haveT0 = r.Timestamp, true
				}
				if r.PositionLat != basetype.Sint32Invalid && r.PositionLong != basetype.Sint32Invalid {
					fixes = append(fixes, fix{r.Timestamp,
						semicirclesToDegrees(r.PositionLat), semicirclesToDegrees(r.PositionLong)})
				}
			case mesgnum.Lap:
				laps = append(laps, mesgdef.NewLap(m))
			case mesgnum.Session:
				ses := mesgdef.NewSession(m)
				if out.Sport == "unknown" {
					out.Sport = ses.Sport.String()
				}
				if out.SubSport == "" {
					out.SubSport = ses.SubSport.String()
				}
				// Chained files sum, exactly as the register's distance does.
				if v, ok := scaled(ses.TotalElapsedTime, basetype.Uint32Invalid, 1000); ok {
					out.ElapsedS += v
				}
				if v, ok := scaled(ses.TotalTimerTime, basetype.Uint32Invalid, 1000); ok {
					out.TimerS += v
				}
				if v, ok := scaled(ses.TotalDistance, basetype.Uint32Invalid, 100); ok {
					out.DistM += v
				}
				if a := u16p(ses.TotalAscent); a != nil {
					n := *a
					if out.AscentM != nil {
						n += *out.AscentM
					}
					out.AscentM = &n
				}
			case mesgnum.Sport:
				sp := mesgdef.NewSport(m)
				if out.Sport == "unknown" {
					out.Sport = sp.Sport.String()
				}
				if out.SubSport == "" {
					out.SubSport = sp.SubSport.String()
				}
			case mesgnum.SplitSummary:
				ss := mesgdef.NewSplitSummary(m)
				sp := detailSplit{Type: ss.SplitType.String(), Splits: u16p(ss.NumSplits)}
				sp.TimerS, _ = scaled(ss.TotalTimerTime, basetype.Uint32Invalid, 1000)
				sp.DistM, _ = scaled(ss.TotalDistance, basetype.Uint32Invalid, 100)
				out.Splits = append(out.Splits, sp)
			}
		}
	}
	if !haveT0 {
		return nil, fmt.Errorf("no record messages with a timestamp")
	}
	out.StartUTC = t0.UTC()
	out.Indoor = indoors(out.SubSport)

	for _, l := range laps {
		dl := detailLap{Trigger: l.LapTrigger.String()}
		if !l.StartTime.IsZero() {
			dl.StartS = int(math.Round(l.StartTime.Sub(t0).Seconds()))
		}
		dl.ElapsedS, _ = scaled(l.TotalElapsedTime, basetype.Uint32Invalid, 1000)
		dl.TimerS, _ = scaled(l.TotalTimerTime, basetype.Uint32Invalid, 1000)
		dl.DistM, _ = scaled(l.TotalDistance, basetype.Uint32Invalid, 100)
		// message_index carries flags in its top bits; the step is the
		// masked index, never the raw value.
		if l.WktStepIndex != typedef.MessageIndexInvalid {
			n := int(l.WktStepIndex & typedef.MessageIndexMask)
			dl.Step = &n
		}
		dl.AvgHR, dl.MaxHR = u8p(l.AvgHeartRate), u8p(l.MaxHeartRate)
		dl.AvgCadence = u8p(l.AvgCadence)
		dl.AvgPower, dl.MaxPower = u16p(l.AvgPower), u16p(l.MaxPower)
		dl.AscentM = u16p(l.TotalAscent)
		// Enhanced first, plain as the fallback — the expansion wart above.
		if v, ok := scaled(l.EnhancedAvgSpeed, basetype.Uint32Invalid, 1000); ok {
			dl.AvgSpeed = &v
		} else if v, ok := scaled(uint32(l.AvgSpeed), uint32(basetype.Uint16Invalid), 1000); ok {
			dl.AvgSpeed = &v
		}
		dl.StartLat, dl.StartLon = degrees(l.StartPositionLat), degrees(l.StartPositionLong)
		dl.EndLat, dl.EndLon = degrees(l.EndPositionLat), degrees(l.EndPositionLong)
		out.Laps = append(out.Laps, dl)
	}

	// The route, and only where the sky reaches.
	if !out.Indoor {
		for _, f := range fixes {
			out.Track = append(out.Track, trackPoint{f.lat, f.lon})
		}
	}
	// A file with no session message still has records: fall back to the
	// span the records cover rather than reporting a zero-second activity.
	if out.ElapsedS == 0 && len(fixes) > 0 {
		out.ElapsedS = fixes[len(fixes)-1].t.Sub(t0).Seconds()
	}
	return out, nil
}

/* ── the payload ───────────────────────────────────────────────────────────

detailPayload is the PAGE's response and nothing else. activityMetricsPayload
is the grader's `get_metrics` body and stays exactly as it was: the two were
one builder, so a field added for a page arrived in the grader's context as a
side effect, and a route would have arrived there as ~4k tokens of noise per
turn. Two builders, two audiences, one test pinning the grader's keys.
*/

// paceBasis is one way of dividing a distance by a time, named. All three
// come from the file: elapsed and moving are the session's own two clocks,
// and running-only is the device's split_summary with its walk breaks
// removed. Nothing here invents a moving-time rule — measured on one walk,
// a recording-gap rule gives 616 s and a 0.5 m/s threshold 957 s against the
// 1,204.9 s the watch itself recorded as moving.
type paceBasis struct {
	Basis string  `json:"basis"` // elapsed | moving | running
	Secs  float64 `json:"secs"`
	DistM float64 `json:"dist_m"`
	Pace  string  `json:"pace"`
}

// A lap shorter than this is a marker, not a stretch: the athlete pressed
// the button twice. Measured on a 2018 recording, a 0.2 m lap lasting 1.1 s
// divides out to a pace of 179:05/mi, which is arithmetically correct and
// says nothing about running. Such a lap keeps its distance and its clock
// and is served without a pace.
const (
	lapPaceFloorM = 10
	lapPaceFloorS = 10
)

type lapOut struct {
	N          int       `json:"n"`
	StartS     int       `json:"start_s"`
	Trigger    string    `json:"trigger"`
	Step       *int      `json:"step,omitempty"`
	DistM      float64   `json:"dist_m"`
	Dist       string    `json:"dist,omitempty"`
	ElapsedS   float64   `json:"elapsed_s"`
	TimerS     float64   `json:"timer_s"`
	Pace       string    `json:"pace,omitempty"`
	AvgHR      *int      `json:"avg_hr,omitempty"`
	MaxHR      *int      `json:"max_hr,omitempty"`
	AvgCadence *int      `json:"avg_cadence,omitempty"`
	AvgPower   *int      `json:"avg_power,omitempty"`
	MaxPower   *int      `json:"max_power,omitempty"`
	AscentM    *int      `json:"ascent_m,omitempty"`
	Ascent     string    `json:"ascent,omitempty"`
	Start      []float64 `json:"start,omitempty"`
	End        []float64 `json:"end,omitempty"`
}

type splitOut struct {
	Type   string  `json:"type"`
	TimerS float64 `json:"timer_s"`
	DistM  float64 `json:"dist_m"`
	Dist   string  `json:"dist,omitempty"`
	Pace   string  `json:"pace,omitempty"`
	Splits *int    `json:"splits,omitempty"`
}

type trackOut struct {
	Points   int    `json:"points"`
	Polyline string `json:"polyline"`
}

type detailOut struct {
	Name       string      `json:"name"`
	Date       string      `json:"date"`
	Sport      string      `json:"sport"`
	SubSport   string      `json:"sub_sport,omitempty"`
	Indoor     bool        `json:"indoor"`
	StartUTC   string      `json:"start_utc"`
	ElapsedS   float64     `json:"elapsed_s"`
	ElapsedHMS string      `json:"elapsed_hms"`
	MovingS    float64     `json:"moving_s,omitempty"`
	MovingHMS  string      `json:"moving_hms,omitempty"`
	DistM      float64     `json:"dist_m,omitempty"`
	Dist       string      `json:"dist,omitempty"`
	AscentM    *int        `json:"ascent_m,omitempty"`
	Ascent     string      `json:"ascent,omitempty"`
	Paces      []paceBasis `json:"paces,omitempty"`
	Laps       []lapOut    `json:"laps,omitempty"`
	Splits     []splitOut  `json:"splits,omitempty"`
	Track      *trackOut   `json:"track,omitempty"`
	SHA256     string      `json:"sha256"`
}

// hms is the register's own spelling of a duration, minutes and seconds with
// the minutes unbounded — `elapsed_hms` reads "75:10" for a 4,510 s run in
// three places already, and detail does not get to invent a fourth.
func hms(secs float64) string {
	s := int(math.Round(secs))
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// detailPayload decodes the stored bytes for display. It deliberately does
// NOT read metrics.db: an activity whose import failed still has a lap table
// and a route, and that is one of the empty states this page owes.
func (s *server) detailPayload(name string) (*detailOut, int, string) {
	if !validActivityName(name) {
		return nil, http.StatusBadRequest, "name must be a plain .fit filename"
	}
	b, err := os.ReadFile(filepath.Join(s.activitiesDir(), name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, http.StatusNotFound, "no such activity"
	}
	if err != nil {
		log.Printf("activity-detail %s: %v", name, err)
		return nil, http.StatusInternalServerError, "could not read"
	}
	d, err := decodeDetail(b)
	if err != nil {
		// One line, bounded: a decoder's complaint can carry internals and
		// this body reaches the network.
		msg := err.Error()
		if i := strings.IndexByte(msg, '\n'); i >= 0 {
			msg = msg[:i]
		}
		if len(msg) > 200 {
			msg = msg[:200] + "…"
		}
		log.Printf("activity-detail %s: decode: %v", name, err)
		return nil, http.StatusUnprocessableEntity, "could not decode: " + msg
	}

	ds := s.ds()
	u := ds.Athlete.Units
	run := d.Sport == "running"
	sum := sha256.Sum256(b)

	out := &detailOut{
		Name: name, Date: activityDate(name, d.StartUTC, ds.Loc),
		Sport: d.Sport, SubSport: d.SubSport, Indoor: d.Indoor,
		StartUTC: d.StartUTC.Format("2006-01-02T15:04:05Z"),
		ElapsedS: d.ElapsedS, ElapsedHMS: hms(d.ElapsedS),
		DistM: d.DistM, AscentM: d.AscentM,
		SHA256: hex.EncodeToString(sum[:]),
	}
	if d.TimerS > 0 {
		out.MovingS, out.MovingHMS = d.TimerS, hms(d.TimerS)
	}
	if d.DistM > 0 {
		out.Dist = Distance(d.DistM).InMeasured(u)
	}
	if d.AscentM != nil {
		out.Ascent = Elevation(*d.AscentM).In(u)
	}

	// Three paces, labelled, so nothing has to choose one on the athlete's
	// behalf: his own grade notes already quote both elapsed and moving.
	if run && d.DistM > 0 {
		add := func(basis string, secs, dist float64) {
			if secs > 0 && dist > 0 {
				out.Paces = append(out.Paces, paceBasis{basis, secs, dist,
					Pace(secs / dist).In(u)})
			}
		}
		add("elapsed", d.ElapsedS, d.DistM)
		add("moving", d.TimerS, d.DistM)
		for _, sp := range d.Splits {
			if sp.Type == "rwd_run" {
				add("running", sp.TimerS, sp.DistM)
			}
		}
	}

	for i, l := range d.Laps {
		o := lapOut{N: i + 1, StartS: l.StartS, Trigger: l.Trigger, Step: l.Step,
			DistM: l.DistM, ElapsedS: l.ElapsedS, TimerS: l.TimerS,
			AvgHR: l.AvgHR, MaxHR: l.MaxHR, AvgPower: l.AvgPower, MaxPower: l.MaxPower,
			AscentM: l.AscentM}
		if l.DistM > 0 {
			o.Dist = Distance(l.DistM).InMeasured(u)
			if run && l.TimerS >= lapPaceFloorS && l.DistM >= lapPaceFloorM {
				o.Pace = Pace(l.TimerS / l.DistM).In(u)
			}
		}
		if l.AscentM != nil {
			o.Ascent = Elevation(*l.AscentM).In(u)
		}
		if c := l.AvgCadence; c != nil {
			n := *c
			if run {
				n *= 2 // run records are per-leg; the register doubles at presentation
			}
			o.AvgCadence = &n
		}
		// Lap corners are precise positions too, and a virtual world's are
		// still a lie: gated with the track, not separately.
		if !d.Indoor {
			if l.StartLat != nil && l.StartLon != nil {
				o.Start = []float64{*l.StartLat, *l.StartLon}
			}
			if l.EndLat != nil && l.EndLon != nil {
				o.End = []float64{*l.EndLat, *l.EndLon}
			}
		}
		out.Laps = append(out.Laps, o)
	}

	for _, sp := range d.Splits {
		so := splitOut{Type: sp.Type, TimerS: sp.TimerS, DistM: sp.DistM, Splits: sp.Splits}
		if sp.DistM > 0 {
			so.Dist = Distance(sp.DistM).InMeasured(u)
			if run && sp.TimerS > 0 {
				so.Pace = Pace(sp.TimerS / sp.DistM).In(u)
			}
		}
		out.Splits = append(out.Splits, so)
	}

	if len(d.Track) > 0 {
		out.Track = &trackOut{Points: len(d.Track), Polyline: encodePolyline(d.Track)}
	}
	return out, http.StatusOK, ""
}

// getActivityDetail serves one activity's shape for the page. The stored
// bytes are immutable, so the response is a pure function of (file, plan,
// build) — all three go into the validator, because flipping athlete.json to
// metric re-renders every string in here and a file hash alone would serve
// the old units from a cache. Gzipped when asked for: the origin compresses
// nothing, and the polyline is the first response where that shows.
func (s *server) getActivityDetail(w http.ResponseWriter, r *http.Request) {
	out, code, msg := s.detailPayload(r.URL.Query().Get("name"))
	if code != http.StatusOK {
		http.Error(w, msg, code)
		return
	}
	body, err := json.Marshal(out)
	if err != nil {
		log.Printf("activity-detail %s: %v", out.Name, err)
		http.Error(w, "could not encode", http.StatusInternalServerError)
		return
	}
	etag := etagFor([]byte(out.SHA256 + s.ds().Rev + buildHash))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
	w.Header().Set("Vary", "Accept-Encoding")
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		_, _ = w.Write(body)
		return
	}
	w.Header().Set("Content-Encoding", "gzip")
	gz := gzip.NewWriter(w)
	if _, err := gz.Write(body); err != nil {
		log.Printf("activity-detail %s: gzip: %v", out.Name, err)
	}
	_ = gz.Close()
}

// encodePolyline is Google's polyline algorithm at five decimal places —
// about a metre, measured at 0.675 m worst round-trip error over a real
// 4,495-point run, for 9,025 characters against the 100 KB the same points
// cost as JSON pairs. The origin does not compress, so the encoding is the
// compression.
func encodePolyline(pts []trackPoint) string {
	var b strings.Builder
	var prevLat, prevLon int
	enc := func(v int) {
		v <<= 1
		if v < 0 {
			v = ^v
		}
		for v >= 0x20 {
			b.WriteByte(byte((0x20 | (v & 0x1f)) + 63))
			v >>= 5
		}
		b.WriteByte(byte(v + 63))
	}
	for _, p := range pts {
		lat := int(math.Round(p.Lat * 1e5))
		lon := int(math.Round(p.Lon * 1e5))
		enc(lat - prevLat)
		enc(lon - prevLon)
		prevLat, prevLon = lat, lon
	}
	return b.String()
}
