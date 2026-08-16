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
	"sort"
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
	Series          []sample // per record, for the chart only
}

// sample is one record's worth of what a chart draws: the clock, the heart,
// the speed and the watts, exactly as recorded.
type sample struct {
	t     time.Time
	hr    int
	watts int
	vel   float64 // m/s; 0 when the record carries none
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
	var t0, tLast time.Time
	var haveT0 bool
	// The chart's raw material, read in the same pass. This is DISPLAY
	// velocity: it takes whatever the record carries and does not re-read the
	// file with expansion off the way the register does, because an ulp of
	// speed is invisible on a 360-pixel chart and a second decode is not
	// free. Nothing here is measured against a target.
	var series []sample

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
				tLast = r.Timestamp
				if r.PositionLat != basetype.Sint32Invalid && r.PositionLong != basetype.Sint32Invalid {
					fixes = append(fixes, fix{r.Timestamp,
						semicirclesToDegrees(r.PositionLat), semicirclesToDegrees(r.PositionLong)})
				}
				sm := sample{t: r.Timestamp}
				if r.HeartRate != basetype.Uint8Invalid {
					sm.hr = int(r.HeartRate)
				}
				if r.Power != basetype.Uint16Invalid {
					sm.watts = int(r.Power)
				}
				switch {
				case r.EnhancedSpeed != basetype.Uint32Invalid:
					sm.vel = float64(r.EnhancedSpeed) / 1000
				case r.Speed != basetype.Uint16Invalid:
					sm.vel = float64(r.Speed) / 1000
				}
				series = append(series, sm)
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
	out.Series = series

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
	// A file whose session carries no total_elapsed_time still has records:
	// the span they cover is the honest fallback. It reads off the RECORDS
	// and not the position fixes, because an indoor ride has none of the
	// latter — measured on a 1,799 s trainer session that reported "0:00
	// elapsed" beside 7.77 miles.
	if out.ElapsedS == 0 && !tLast.IsZero() {
		out.ElapsedS = tLast.Sub(t0).Seconds()
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
	N          int            `json:"n"`
	StartS     int            `json:"start_s"`
	Trigger    string         `json:"trigger"`
	Step       *int           `json:"step,omitempty"`
	DistM      float64        `json:"dist_m"`
	Dist       string         `json:"dist,omitempty"`
	ElapsedS   float64        `json:"elapsed_s"`
	TimerS     float64        `json:"timer_s"`
	Pace       string         `json:"pace,omitempty"`
	AvgHR      *int           `json:"avg_hr,omitempty"`
	MaxHR      *int           `json:"max_hr,omitempty"`
	AvgCadence *int           `json:"avg_cadence,omitempty"`
	AvgPower   *int           `json:"avg_power,omitempty"`
	MaxPower   *int           `json:"max_power,omitempty"`
	AscentM    *int           `json:"ascent_m,omitempty"`
	Ascent     string         `json:"ascent,omitempty"`
	Start      []float64      `json:"start,omitempty"`
	End        []float64      `json:"end,omitempty"`
	Prescribed *prescribedOut `json:"prescribed,omitempty"`
}

type splitOut struct {
	Type   string  `json:"type"`
	TimerS float64 `json:"timer_s"`
	DistM  float64 `json:"dist_m"`
	Dist   string  `json:"dist,omitempty"`
	Pace   string  `json:"pace,omitempty"`
	Splits *int    `json:"splits,omitempty"`
}

// prescribedOut is which step of the pushed workout a lap was, and how the
// delivery compares to what that step asked for. This is the join the plan
// exists for: the watch stamps wkt_step_index on every lap it drove, and the
// block already knows what step 1 of that workout was meant to be.
type prescribedOut struct {
	Index  int    `json:"index"` // wkt_step_index, as recorded
	Role   string `json:"role"`
	Label  string `json:"label"`         // "Rep 2", "Recovery 2", "Warm-up"
	Rep    int    `json:"rep,omitempty"` // 1-based occurrence within its repeat
	OfReps int    `json:"of_reps,omitempty"`
	Dur    string `json:"dur,omitempty"`    // "3:00", or "400 m" on a distance step
	Target string `json:"target,omitempty"` // "239-265 W", "7:41-7:55/mi", "140-150 bpm"
	// Verdict reads against the EFFORT asked for, so "under" is less power,
	// a slower pace or a lower heart rate, and "over" is the reverse. One
	// vocabulary, whichever metric the step targets.
	Verdict string `json:"verdict,omitempty"` // in | under | over | short
}

// sessionOut is the prescription the day carried, so the recording can be
// read against what was asked rather than on its own.
type sessionOut struct {
	Kind      string `json:"kind"`
	Label     string `json:"label"`
	RepsAsked int    `json:"reps_asked,omitempty"`
	RepsDone  int    `json:"reps_done,omitempty"`
	RepsWhat  string `json:"reps_what,omitempty"` // what one rep was: "3:00 @ 239-265 W"
}

// noteOut is one thing the athlete said about the day. Absent entirely when
// nothing was said: a popover that renders an empty "Notes" heading is
// telling the reader something was written when nothing was.
type noteOut struct {
	Kind string `json:"kind,omitempty"` // note | task | issue | skip — where it was said
	Note string `json:"note"`
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
	Session    *sessionOut `json:"session,omitempty"`
	Chart      *chartOut   `json:"chart,omitempty"`
	Notes      []noteOut   `json:"notes,omitempty"`
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
func (s *server) detailPayload(name, blockID string) (*detailOut, int, string) {
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
	out.Chart = buildChart(d, u)
	s.joinPrescription(out, d, blockID)
	// What the athlete said about the day. On 12 Aug 2026 the recording shows
	// two of four reps and a 296 s stop inside the second; only the note says
	// the chain came off and ERG would not re-engage. The numbers cannot
	// carry that and should not be read as if they could.
	for _, e := range s.store.NotesOn(out.Date) {
		out.Notes = append(out.Notes, noteOut{Kind: e.Kind, Note: e.Note})
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
	out, code, msg := s.detailPayload(r.URL.Query().Get("name"), r.URL.Query().Get("block"))
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

/* ── the step join ─────────────────────────────────────────────────────────

A lap the watch drove carries wkt_step_index, and the block knows what that
step asked for. Joining them is the one reading this app can produce and a
general activity site cannot: prescribed against delivered, rep by rep,
rather than a list of laps with no idea what they were meant to be.

Everything here degrades rather than fails. A recording with no step indices
(a Zwift ride is one lap with none), a day outside any block, a sport that
does not match the day's session, steps that will not resolve — each costs
the join and nothing else, and the lap table stands on its own.
*/

// repDeliveredShare is how much of a step's prescribed duration a lap has to
// cover before it counts as that rep having been done. Measured on the
// 12 Aug 2026 ride: the two delivered reps lapped at exactly 180.0 s against
// a 180 s step, and the laps after them were 91.4 s, 2.4 s and 0.8 s — the
// athlete pressing the button, not a third and fourth rep. The watch laps a
// driven step on the step's own boundary, so anything short of nine tenths
// was cut off.
const repDeliveredShare = 0.9

// joinPrescription attaches each lap's prescribed step, and the session it
// belongs to, when the file came from a workout this block pushed.
func (s *server) joinPrescription(out *detailOut, d *activityDetail, blockID string) {
	ds := s.ds()
	if ds == nil {
		return
	}
	date, err := time.ParseInLocation("2006-01-02", out.Date, ds.Loc)
	if err != nil {
		return
	}
	blk, ok := ds.blockFor(blockID, s.day(ds))
	if !ok {
		return
	}
	wk, di, ok := blk.Locate(date)
	if !ok {
		return
	}
	sess := wk.Days[di]
	out.Session = &sessionOut{Kind: string(sess.Kind), Label: sess.Label}

	// A ride recorded on a run day is not that day's session, and pinning its
	// laps to the run's steps would invent a reading. The grader's own test.
	if len(sess.Steps) == 0 || sportOf(sess.Kind) != d.Sport {
		return
	}
	ctx := blk.ctxFor(ds.Athlete, wk.N).forSession(&sess)
	rs, err := resolveSteps(ctx, sess)
	if err != nil {
		log.Printf("activity-detail %s: resolving %s's steps: %v", out.Name, out.Date, err)
		return
	}
	em := flattenSteps(rs)
	u := ds.Athlete.Units

	// Rep numbering is per emitted step: the file repeats step indices
	// (1,2,1,2 on a two-step repeat), so the nth lap carrying index i is the
	// nth time through step i.
	seen := map[int]int{}
	for i := range out.Laps {
		l := &out.Laps[i]
		if l.Step == nil || *l.Step >= len(em) {
			continue
		}
		e := em[*l.Step]
		if e.IsRepeat { // the marker that closes a repeat is not a lap anyone runs
			continue
		}
		seen[*l.Step]++
		p := &prescribedOut{Index: *l.Step, Role: e.Leaf.Role,
			Dur: stepDuration(e.Leaf, u), Target: stepTarget(e.Leaf, u)}
		if e.Group > 0 {
			p.Rep, p.OfReps = seen[*l.Step], e.Reps
		}
		p.Label = stepLabel(e.Leaf.Role, p.Rep)
		// A lap that never covered its step is not judged against the step's
		// band: on 12 Aug the athlete pressed the button twice at the end and
		// a 0.8 s lap at 318 W read as a rep OVER its target. Short is the
		// verdict, and it is the honest one.
		if !delivered(e.Leaf, *l) {
			p.Verdict = "short"
		} else {
			p.Verdict = stepVerdict(e.Leaf, *l, d.Sport == "running")
		}
		l.Prescribed = p
	}

	// The session line: what one rep was, how many were asked for, and how
	// many the recording actually carries.
	for i, e := range em {
		if e.Group == 0 || e.IsRepeat || e.Leaf.Role != "active" {
			continue
		}
		out.Session.RepsAsked = e.Reps
		out.Session.RepsWhat = strings.TrimSpace(stepDuration(e.Leaf, u) + " @ " + stepTarget(e.Leaf, u))
		out.Session.RepsWhat = strings.TrimSuffix(out.Session.RepsWhat, "@")
		for _, l := range out.Laps {
			if l.Prescribed == nil || l.Prescribed.Index != i {
				continue
			}
			if delivered(e.Leaf, l) {
				out.Session.RepsDone++
			}
		}
		break // the first work repeat is the session's headline
	}
}

// delivered reports whether a lap covered enough of its step to count as
// that rep having been run.
func delivered(st resolvedStep, l lapOut) bool {
	switch {
	case st.Secs > 0:
		return l.TimerS >= float64(st.Secs)*repDeliveredShare
	case st.DistM > 0:
		return l.DistM >= st.DistM*repDeliveredShare
	}
	return false
}

func stepDuration(st resolvedStep, u Units) string {
	switch {
	case st.Secs > 0:
		return clock(st.Secs)
	case st.DistM > 0:
		return Distance(st.DistM).In(u)
	}
	return ""
}

func stepTarget(st resolvedStep, u Units) string {
	switch {
	case st.PowerHi > 0:
		return fmt.Sprintf("%d–%d W", st.PowerLo, st.PowerHi)
	case st.PaceFast > 0 && st.PaceSlow > 0:
		return st.PaceSlow.In(u) + "–" + st.PaceFast.In(u)
	case st.HRHi > 0:
		return fmt.Sprintf("%d–%d bpm", st.HRLo, st.HRHi)
	}
	return ""
}

// stepLabel is what the row says it was. A rep is numbered where it repeats;
// a warm-up is a warm-up.
func stepLabel(role string, rep int) string {
	name := map[string]string{
		"active": "Rep", "recovery": "Rec", "rest": "Rest",
		"warmup": "W-up", "cooldown": "Cool",
	}[role]
	if name == "" {
		name = role
	}
	if rep > 0 && (role == "active" || role == "recovery" || role == "rest") {
		return fmt.Sprintf("%s %d", name, rep)
	}
	return name
}

// stepVerdict compares a DELIVERED lap to the band its step asked for, in the
// step's own currency: watts where ERG held the watts, pace where the target
// is a pace, heart rate where that is all the step declared. "under" is
// always less effort than asked and "over" always more — including for pace,
// where the slower number is the smaller effort.
func stepVerdict(st resolvedStep, l lapOut, run bool) string {
	band := func(v, lo, hi float64) string {
		switch {
		case v < lo:
			return "under"
		case v > hi:
			return "over"
		}
		return "in"
	}
	switch {
	case st.PowerHi > 0 && l.AvgPower != nil:
		return band(float64(*l.AvgPower), float64(st.PowerLo), float64(st.PowerHi))
	case st.PaceFast > 0 && st.PaceSlow > 0 && run && l.DistM > 0 && l.TimerS > 0:
		// Pace is seconds per metre: slower is a bigger number and less work,
		// so the comparison inverts against the band's own ends.
		p := l.TimerS / l.DistM
		switch {
		case p > float64(st.PaceSlow):
			return "under"
		case p < float64(st.PaceFast):
			return "over"
		}
		return "in"
	case st.HRHi > 0 && l.AvgHR != nil:
		return band(float64(*l.AvgHR), float64(st.HRLo), float64(st.HRHi))
	}
	return ""
}

/* ── the chart's series ────────────────────────────────────────────────────

A recording is thousands of samples and a phone is 390 pixels wide, so the
series a chart reads is bucketed to about one point per pixel and each point
is the MEDIAN of its bucket. Median rather than mean because a single dropped
heart-rate sample or a GPS speed spike moves a mean and does not move a
median, and because the smoothing and the decimation are then the same
operation rather than two.

A bucket with nothing in it is null, not zero, so the line breaks where the
recording did. Pace is null where the athlete was not moving: a stopped
sample is not a slow pace, it is no pace, and drawing it as one puts a spike
through the axis. That is a drawing rule and it produces no number anyone
reads — the moving-time rule the register refuses to invent stays refused.
*/

// chartPoints is roughly one point per pixel of a phone-width chart.
const chartPoints = 200

type chartOut struct {
	// Unit is what a pace value counts: seconds per mile or per kilometre,
	// in the athlete's own units, so the drawing needs no conversion table.
	Unit  string     `json:"unit,omitempty"`
	Secs  []int      `json:"secs"`
	HR    []*int     `json:"hr,omitempty"`
	Pace  []*float64 `json:"pace,omitempty"` // seconds per Unit
	Watts []*int     `json:"watts,omitempty"`
}

func medianInts(xs []int) *int {
	if len(xs) == 0 {
		return nil
	}
	s := append([]int(nil), xs...)
	sort.Ints(s)
	v := s[len(s)/2]
	return &v
}

func medianFloats(xs []float64) *float64 {
	if len(xs) == 0 {
		return nil
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	v := s[len(s)/2]
	return &v
}

// paceMedianWindowS is how wide the rolling median over speed is. GPS speed
// is jittery in a way heart rate and a trainer's watts are not: at a
// nine-second bucket a steady run draws as a picket fence, and the eye reads
// noise where the athlete held a pace. Thirty seconds is the smallest window
// that renders his 15 Aug long run as the steady effort the mile splits say
// it was — 9:26 to 10:20 across ten miles.
//
// Watts get no window at all, deliberately: ERG holds a square wave, and a
// rolling median would round the corners off the very steps the session was
// prescribed as. The smoothing matches each sensor's noise rather than
// blurring everything equally.
const paceMedianWindowS = 30

// buildChart buckets the samples into an even grid and takes each bucket's
// median, widening the window for speed alone. Heart rate keeps the
// register's dropout rule — under 50 bpm is a dropout everywhere in this
// app, and a chart that drew them would show a heart stopping.
func buildChart(d *activityDetail, u Units) *chartOut {
	if len(d.Series) < 2 {
		return nil
	}
	t0 := d.Series[0].t
	span := d.Series[len(d.Series)-1].t.Sub(t0).Seconds()
	if span <= 0 {
		return nil
	}
	n := chartPoints
	if int(span) < n {
		n = int(span)
	}
	if n < 2 {
		return nil
	}
	width := span / float64(n)

	hrB := make([][]int, n)
	wB := make([][]int, n)
	vB := make([][]float64, n)
	occupied := make([]bool, n) // a bucket with samples of its own
	var haveHR, haveW, haveV bool
	// The speed window reaches beyond its bucket; everything else is read
	// from the bucket it landed in.
	reach := int(math.Ceil(paceMedianWindowS / 2 / width))
	for _, sm := range d.Series {
		at := sm.t.Sub(t0).Seconds()
		i := int(at / width)
		if i < 0 || i >= n {
			continue
		}
		occupied[i] = true
		if hrValid(float64(sm.hr)) {
			hrB[i] = append(hrB[i], sm.hr)
			haveHR = true
		}
		if sm.watts > 0 {
			wB[i] = append(wB[i], sm.watts)
			haveW = true
		}
		if sm.vel > 0 {
			haveV = true
			for j := i - reach; j <= i+reach; j++ {
				if j >= 0 && j < n {
					vB[j] = append(vB[j], sm.vel)
				}
			}
		}
	}

	out := &chartOut{Secs: make([]int, n)}
	per := metresPerMile
	out.Unit = "/mi"
	if u == Metric {
		per, out.Unit = metresPerKM, "/km"
	}
	for i := 0; i < n; i++ {
		out.Secs[i] = int(float64(i) * width)
		if haveHR {
			out.HR = append(out.HR, medianInts(hrB[i]))
		}
		if haveW {
			out.Watts = append(out.Watts, medianInts(wB[i]))
		}
		if haveV {
			var p *float64
			// A bucket with no samples of its own is null however far the
			// window reaches: the line breaks where the recording did, and a
			// smoothing window is not a licence to draw across a stop.
			if v := medianFloats(vB[i]); v != nil && *v > 0 && occupied[i] {
				secs := pyRound(per / *v, 1)
				p = &secs
			}
			out.Pace = append(out.Pace, p)
		}
	}
	if !haveV {
		out.Unit = ""
	}
	return out
}
