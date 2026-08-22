package main

// The benchmark timeline: what the block's test days measured, in series,
// against the goal. A block tags its test sessions — FTP, DEC, LT, TT,
// RACE — and the results of those days were until 21 Aug 2026 wherever a
// grade note or a hand-posted metric entry had put them, which is to say
// in prose, retyped, and in one case wrong (the 5 Aug W/kg on a stale
// bodyweight). This derives each day's number from its recording, the way
// every other number the athlete sees is derived, and keeps the rows in
// metrics.db beside the activities they were measured from.
//
// What each tag's number IS was decided by the athlete on 21 Aug 2026 —
// one answer per type, not to be re-litigated:
//
//   FTP   watts: 75% of the best 60 s of power in the ramp.
//   DEC   decoupling, in percent, BOTH ways: Pa:HR (pace against heart
//         rate) and Pw:HR (running power against heart rate).
//   LT    LTHR over the final 20 min of the 30-min effort, the pace over
//         the same window beside it.
//   TT    the effort's elapsed time against the goal, the race included
//   RACE  as the last point of the same series.
//
// The EFFORT WINDOW is the whole question. A test day is a warm-up, the
// effort and a cool-down, and the archive holds both shapes: three files
// where the watch was stopped between them, and one file whose laps carry
// the pushed workout's step indices. So: where the session ran a pushed
// workout and a recording's laps name its steps, the window is the span of
// the laps carrying the longest active step — read from the file exactly
// as detail.go reads it for the page, a READ field, no arithmetic. Where
// no recording does, the day's recordings of the session's sport are
// chosen among by the type's own rule and measured whole: the ride with
// the best 60 s for FTP, the longest run for DEC, the fastest run of at
// least ten minutes for LT, TT and RACE. Measurement inside the window is
// the register's (metrics.go: windowDecoupling, windowMean, windowBest),
// mirrored in grade_metrics.py over whole files and pinned by the
// acceptance gate; the window bounds are parameters to it. The stretch
// search a free-run effort uses (fastestSegments) had no mirror until the
// best-effort trend gave it one on 22 Aug 2026 (bestEffort).
//
// Rows cache in the benchmarks table keyed by block and date, stamped with
// the names of the day's recordings and benchmarkVersion: a file added to
// the day or a change in this file's arithmetic recomputes. A bump of the
// activities schema drops the table too, because the register's own
// conventions (the gap rule, say) feed these measurements and a bump is
// how a change there announces itself. Eleven files a block, decoded once
// each — no rebuild, no reconcile, computed on the first render that
// needs them; a day that cannot be measured is remembered as such under
// the same key, so it is not decoded again until its recordings change.
// The table is derived and disposable like the rest of metrics.db.

import (
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// benchmarkVersion stamps every row. Bump it when the arithmetic or the
// window rule changes shape; every row then recomputes on its next read.
const benchmarkVersion = 1

// benchmarkRow is one tagged day's measurement, as the table holds it.
// Every measure the window allows is kept, whichever the tag presents:
// a ramp's decoupling and a time trial's best minute cost nothing and
// answer the next question without a recompute.
type benchmarkRow struct {
	Block, Date, Tag string
	Names            string // the day's recordings of the sport, sorted, joined by "," — the cache key
	Name             string // the recording measured
	Lo, Hi           int    // the effort window, stream seconds (lo, hi]
	FTPW             *float64
	PaHR, PwHR       *float64 // decoupling, percent
	LTHR, LTVel      *float64 // final-20-min mean HR and velocity (m/s)
	TTS              *int     // the window's elapsed seconds
	TTDistM          *float64 // the window's distance
	Version          int
}

// benchmarkSpec is one tag the timeline knows how to measure. The set is
// closed here, like an issue's tone: a block may tag a day anything, and
// an unknown tag is simply not on the timeline.
type benchmarkSpec struct {
	Tag   string
	Title string // the panel's name; a time trial's is named after the goal
	// pick chooses among the day's recordings of the session's sport when
	// no pushed workout marks the effort; -1 when none qualifies.
	pick func(c []benchCand) int
	// effort is true for the types whose number is an EFFORT inside a
	// recording (LT, TT, RACE): a fallback file is then searched for the
	// fastest stretch of the prescribed length rather than measured whole.
	effort bool
	// minSecs is the shortest window that can be this test at all — a
	// stride file or a two-second lap is never a time trial.
	minSecs int
}

// benchCand is one of a day's recordings, decoded once.
type benchCand struct {
	meta    activityMetrics
	data    []byte
	streams *activityStreams
	detail  *activityDetail
}

func (c *benchCand) load() error {
	if c.streams != nil {
		return nil
	}
	s, err := decodeActivity(c.data)
	if err != nil {
		return err
	}
	d, err := decodeDetail(c.data)
	if err != nil {
		return err
	}
	c.streams, c.detail = s, d
	return nil
}

var benchmarkSpecs = []benchmarkSpec{
	{Tag: "FTP", Title: "FTP", minSecs: 300, pick: func(c []benchCand) int {
		best, at := 0.0, -1
		for i := range c {
			if c[i].load() != nil || !c[i].streams.HaveWatts {
				continue
			}
			if b := bestRolling(c[i].streams.Time, intsToFloats(c[i].streams.Watts), 60); b != nil && *b > best {
				best, at = *b, i
			}
		}
		return at
	}},
	{Tag: "DEC", Title: "Decoupling", minSecs: 600, pick: func(c []benchCand) int {
		best, at := 0.0, -1
		for i := range c {
			if d := c[i].meta.DistanceM; d != nil && *d > best {
				best, at = *d, i
			}
		}
		return at
	}},
	{Tag: "LT", Title: "Lactate threshold", minSecs: 600, pick: fastestRun, effort: true},
	{Tag: "TT", Title: "Time trial", minSecs: 300, pick: fastestRun, effort: true},
	{Tag: "RACE", Title: "Time trial", minSecs: 300, pick: fastestRun, effort: true},
}

// ttTitle names the time-trial panel after the block's goal: "5K time
// trial" for a 5K block, "10K time trial" for a 10K one — data, not a
// constant, the way the embedded example's 10K proves.
func ttTitle(goal Goal) string {
	if goal.Event != "" {
		return goal.Event + " time trial"
	}
	return "Time trial"
}

// goalMetres reads the goal event's distance where it is a standard one,
// so a time trial authored without steps still has a length to search
// for. Zero when the event is not one of these.
func goalMetres(event string) float64 {
	switch strings.ToLower(strings.TrimSpace(event)) {
	case "5k", "5 k", "5km", "5 km":
		return 5000
	case "10k", "10 k", "10km", "10 km":
		return 10000
	case "half", "half marathon", "21.1k", "21.1 km":
		return 21097.5
	case "marathon", "42.2k", "42.2 km":
		return 42195
	case "mile", "1 mile", "1 mi":
		return 1609.344
	}
	return 0
}

// fastestRun is the recording with the highest mean velocity among those of
// at least ten minutes' moving time — on a test day that is the effort,
// never the warm-up or the cool-down, and never a strides file.
func fastestRun(c []benchCand) int {
	best, at := 0.0, -1
	for i := range c {
		m := c[i].meta
		if m.MovingS < 600 || m.DistanceM == nil {
			continue
		}
		if v := *m.DistanceM / float64(m.MovingS); v > best {
			best, at = v, i
		}
	}
	return at
}

func benchmarkSpecFor(tag string) *benchmarkSpec {
	for i := range benchmarkSpecs {
		if benchmarkSpecs[i].Tag == tag {
			return &benchmarkSpecs[i]
		}
	}
	return nil
}

// effortStep is the index, in the emitted workout, of the longest active
// step: the effort a test day's pushed workout drives. Steps inside a
// repeat are strides and hills, never the test, so a top-level active
// step wins over any of them; among those, length decides, a distance
// leaf estimated at an easy 3 m/s so a 30-minute effort outranks a 100 m
// stride and a 5K outranks a 15-minute warm-up. -1 when the workout has
// no active step at all.
func effortStep(em []emittedStep) int {
	est := func(l resolvedStep) float64 {
		if l.Secs > 0 {
			return float64(l.Secs)
		}
		return l.DistM / 3.0
	}
	at, best := -1, 0.0
	for _, top := range []bool{true, false} {
		for i, e := range em {
			if e.IsRepeat || e.Leaf.Role != "active" || (e.Group == 0) != top {
				continue
			}
			if v := est(e.Leaf); at < 0 || v > best {
				at, best = i, v
			}
		}
		if at >= 0 {
			return at
		}
	}
	return -1
}

// deliveredWindow reports whether a window covered its step: at least
// repDeliveredShare of the prescribed time or distance, the same bar
// detail.go sets a rep. An abandoned test is short, not a record.
func deliveredWindow(leaf resolvedStep, secs int, dist float64) bool {
	switch {
	case leaf.Secs > 0:
		return float64(secs) >= float64(leaf.Secs)*repDeliveredShare
	case leaf.DistM > 0:
		return dist >= leaf.DistM*repDeliveredShare
	}
	return false
}

// lapWindow is the span of the laps carrying step index idx in a detail
// decode, with their distance: (lo, hi] in stream seconds. ok is false
// when no lap names the step.
func lapWindow(d *activityDetail, idx int) (lo, hi int, dist float64, ok bool) {
	lo, hi = math.MaxInt, 0
	for _, l := range d.Laps {
		if l.Step == nil || *l.Step != idx {
			continue
		}
		ok = true
		if l.StartS < lo {
			lo = l.StartS
		}
		if end := l.StartS + int(math.Round(l.ElapsedS)); end > hi {
			hi = end
		}
		dist += l.DistM
	}
	if !ok {
		return 0, 0, 0, false
	}
	return lo, hi, dist, true
}

// measureBenchmark fills a row from one recording's window. Every measure
// the streams allow is taken; the tag decides what is shown.
func measureBenchmark(row *benchmarkRow, s *activityStreams, lo, hi int, dist *float64) {
	w, _ := sampleWeights(s.Time)
	flo, fhi := float64(lo), float64(hi)
	all := func(float64) bool { return true }
	if s.HaveWatts {
		watts := intsToFloats(s.Watts)
		// FTP is a cycling number: the mirror pins best_60s on rides only,
		// and a run's device power has no FTP to be 75% of.
		if s.Sport == "cycling" {
			if b := windowBest(s.Time, watts, lo, hi, 60); b != nil {
				v := 0.75 * *b
				row.FTPW = &v
			}
		}
		if s.HaveHR {
			row.PwHR = windowDecoupling(s, w, intsToFloats(s.HR), watts, flo, fhi)
		}
	}
	if s.HaveHR {
		hrF := intsToFloats(s.HR)
		if s.HaveVel {
			row.PaHR = windowDecoupling(s, w, hrF, s.Vel, flo, fhi)
		}
		row.LTHR = windowMean(s.Time, w, hrF, math.Max(flo, fhi-1200), fhi, hrValid)
	}
	if s.HaveVel {
		row.LTVel = windowMean(s.Time, w, s.Vel, math.Max(flo, fhi-1200), fhi, all)
	}
	secs := hi - lo
	row.TTS = &secs
	row.TTDistM = dist
	row.Lo, row.Hi = lo, hi
}

// benchmarkPoint is one measured day, with the prescription it answered.
type benchmarkPoint struct {
	Date  string
	Week  int
	Tag   string
	Label string // the session label, emphasis stripped
	Row   benchmarkRow
}

// benchmarks is the block's measured test days, oldest first — cached rows
// where the day's recordings and the version still match, computed and
// stored otherwise. A day with no recording of the right sport is not a
// point; a day whose recording cannot be measured is logged and skipped,
// never a half-row. nil without a metrics cache.
func (s *server) benchmarks(d *dataset, blk *Block, today time.Time) []benchmarkPoint {
	if s.metrics == nil {
		return nil
	}
	var out []benchmarkPoint
	for wi, wk := range blk.Weeks {
		for di, sess := range wk.Days {
			spec := benchmarkSpecFor(sess.Tag)
			if spec == nil || blk.DayOf(wi, di).After(today) {
				continue
			}
			date := blk.DayOf(wi, di).Format("2006-01-02")
			rows, err := s.metrics.byDate(date)
			if err != nil {
				log.Printf("benchmarks %s: %v", date, err)
				continue
			}
			var cands []benchCand
			var names []string
			for _, r := range rows {
				if r.Sport == sportOf(sess.Kind) {
					cands = append(cands, benchCand{meta: r})
					names = append(names, r.Name)
				}
			}
			if len(cands) == 0 {
				continue
			}
			sort.Strings(names)
			key := strings.Join(names, ",")
			row, err := s.metrics.benchmarkGet(blk.ID, date)
			if err != nil {
				log.Printf("benchmarks %s: %v", date, err)
				continue
			}
			if row == nil || row.Names != key || row.Version != benchmarkVersion || row.Tag != sess.Tag {
				row, err = s.computeBenchmark(d, blk, wk, di, sess, spec, cands, key)
				if err != nil {
					// Remembered under the same key, so the day is not read
					// and decoded again on every render: it is measured
					// afresh only when its recordings change.
					log.Printf("benchmarks %s (%s): %v", date, sess.Tag, err)
					row = &benchmarkRow{Block: blk.ID, Date: date, Tag: sess.Tag, Names: key, Version: benchmarkVersion}
				}
				if err := s.metrics.benchmarkPut(row); err != nil {
					log.Printf("benchmarks %s: storing: %v", date, err)
				}
			}
			if row.Name == "" {
				continue // a day that could not be measured
			}
			out = append(out, benchmarkPoint{Date: date, Week: wk.N, Tag: sess.Tag,
				Label: stripEmph(sess.Label), Row: *row})
		}
	}
	return out
}

// computeBenchmark finds the day's effort window and measures it.
func (s *server) computeBenchmark(d *dataset, blk *Block, wk *Week, di int, sess Session,
	spec *benchmarkSpec, cands []benchCand, key string) (*benchmarkRow, error) {
	date := blk.DayOf(wk.N-1, di).Format("2006-01-02")
	for i := range cands {
		data, err := os.ReadFile(filepath.Join(s.activitiesDir(), cands[i].meta.Name))
		if err != nil {
			return nil, err
		}
		cands[i].data = data
	}
	row := &benchmarkRow{Block: blk.ID, Date: date, Tag: sess.Tag, Names: key, Version: benchmarkVersion}

	// A pushed workout names its effort: the laps that carry its step. Every
	// recording of the day is looked at, a window counts only if it
	// delivered the step, and the best of those wins — a false start saved
	// after six minutes of the 5K does not beat the 5K run after it.
	var leaf *resolvedStep
	if len(sess.Steps) > 0 {
		ctx := blk.ctxFor(d.Athlete, wk.N).forSession(&sess)
		rs, err := resolveSteps(ctx, sess)
		if err != nil {
			return nil, fmt.Errorf("resolving steps: %w", err)
		}
		em := flattenSteps(rs)
		if idx := effortStep(em); idx >= 0 {
			l := em[idx].Leaf
			leaf = &l
			at, bestLo, bestHi, bestDist := -1, 0, 0, 0.0
			for i := range cands {
				if err := cands[i].load(); err != nil {
					continue
				}
				lo, hi, dist, ok := lapWindow(cands[i].detail, idx)
				if !ok || hi-lo < spec.minSecs || !deliveredWindow(l, hi-lo, dist) {
					continue
				}
				if at < 0 || (l.DistM > 0 && dist > bestDist) || (l.DistM == 0 && hi-lo > bestHi-bestLo) {
					at, bestLo, bestHi, bestDist = i, lo, hi, dist
				}
			}
			if at >= 0 {
				row.Name = cands[at].meta.Name
				measureBenchmark(row, cands[at].streams, bestLo, bestHi, &bestDist)
				return row, nil
			}
		}
	}
	// Otherwise the type's own rule picks a recording. For the types whose
	// number is an effort inside it — a threshold test, a time trial — the
	// window is the fastest stretch of the prescribed length the register
	// can find in it (fastestSegments, mirrored), because a race run in
	// plain run mode is one file of warm-up, race and cool-down and its
	// elapsed time is not the race's. With nothing prescribed and no
	// standard goal distance, the file is measured whole.
	at := spec.pick(cands)
	if at < 0 {
		return nil, fmt.Errorf("no recording of the day qualifies as the %s effort", spec.Tag)
	}
	c := &cands[at]
	if err := c.load(); err != nil {
		return nil, err
	}
	hi := c.streams.Time[len(c.streams.Time)-1]
	if hi < spec.minSecs {
		return nil, fmt.Errorf("%s is %d s, too short to be the %s effort", c.meta.Name, hi, spec.Tag)
	}
	row.Name = c.meta.Name
	if spec.effort {
		metres, secs := 0.0, 0
		switch {
		case leaf != nil && leaf.DistM > 0:
			metres = leaf.DistM
		case leaf != nil && leaf.Secs > 0:
			secs = leaf.Secs
		case spec.Tag == "TT" || spec.Tag == "RACE":
			metres = goalMetres(blk.Goal.Event)
		}
		if metres > 0 || secs > 0 {
			segs := fastestSegments(c.streams, metres, secs, 1, s.units())
			if len(segs) == 0 {
				return nil, fmt.Errorf("%s holds no gap-free stretch of the prescribed %s effort", c.meta.Name, spec.Tag)
			}
			sg := segs[0]
			lo, hi := sg.StartS, sg.StartS+int(math.Round(sg.Secs))
			if hi-lo < spec.minSecs {
				return nil, fmt.Errorf("%s's fastest stretch is %d s, too short to be the %s effort", c.meta.Name, hi-lo, spec.Tag)
			}
			dist := sg.DistM
			measureBenchmark(row, c.streams, lo, hi, &dist)
			return row, nil
		}
	}
	measureBenchmark(row, c.streams, 0, hi, c.streams.DistM)
	return row, nil
}

/* ── presentation ──────────────────────────────────────────────────────── */

// clockOf renders seconds as m:ss or h:mm:ss.
func clockOf(secs int) string {
	if secs < 0 {
		secs = -secs
	}
	if secs >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", secs/3600, secs%3600/60, secs%60)
	}
	return fmt.Sprintf("%d:%02d", secs/60, secs%60)
}

// seriesPoint is one plotted value with the text that names it.
type seriesPoint struct {
	Date  string
	Week  int
	Value float64
	Text  string // the value as the athlete reads it
	Aside string // what sits beside it: the LT pace, the TT distance
	Name  string // the recording measured
}

// benchSeries is one line on a panel.
type benchSeries struct {
	Key    string // "pa", "pw", or "" for a single series
	Label  string
	Points []seriesPoint
}

// benchPanel is one chart: one benchmark type, its series, and the goal.
type benchPanel struct {
	Tag      string
	Title    string
	Unit     string
	Series   []benchSeries
	Goal     *float64 // a reference line, TT only
	GoalText string
	Summary  string // "214 → 221 W (+7)" / "21:58 · 0:38 short of 21:20"
	// Dense is a series with a point per run rather than per test day:
	// only the best and the latest point carry a printed value, the rest
	// are markers, and the table below holds every number.
	Dense bool
}

// benchPanels folds points into panels in a fixed order, only the types
// the block measured. The units are the athlete's.
func benchPanels(points []benchmarkPoint, u Units, goal Goal) []benchPanel {
	var out []benchPanel
	add := func(p benchPanel) {
		if len(p.Series) > 0 {
			out = append(out, p)
		}
	}
	// FTP
	{
		var pts []seriesPoint
		for _, p := range points {
			if p.Tag == "FTP" && p.Row.FTPW != nil {
				v := pyRound(*p.Row.FTPW, 0)
				pts = append(pts, seriesPoint{Date: p.Date, Week: p.Week, Value: v,
					Text: fmt.Sprintf("%.0f W", v), Name: p.Row.Name})
			}
		}
		if len(pts) > 0 {
			add(benchPanel{Tag: "FTP", Title: "FTP", Unit: "W",
				Series:  []benchSeries{{Points: pts}},
				Summary: deltaSummary(pts, "%.0f", " W")})
		}
	}
	// DEC: two series, each its own line
	{
		var pa, pw []seriesPoint
		for _, p := range points {
			if p.Tag != "DEC" {
				continue
			}
			if p.Row.PaHR != nil {
				v := pyRound(*p.Row.PaHR, 1)
				pa = append(pa, seriesPoint{Date: p.Date, Week: p.Week, Value: v, Text: fmt.Sprintf("%.1f%%", v), Name: p.Row.Name})
			}
			if p.Row.PwHR != nil {
				v := pyRound(*p.Row.PwHR, 1)
				pw = append(pw, seriesPoint{Date: p.Date, Week: p.Week, Value: v, Text: fmt.Sprintf("%.1f%%", v), Name: p.Row.Name})
			}
		}
		p := benchPanel{Tag: "DEC", Title: "Decoupling", Unit: "%"}
		var sum []string
		if len(pa) > 0 {
			p.Series = append(p.Series, benchSeries{Key: "pa", Label: "Pa:HR", Points: pa})
			sum = append(sum, "Pa:HR "+deltaSummary(pa, "%.1f", "%"))
		}
		if len(pw) > 0 {
			p.Series = append(p.Series, benchSeries{Key: "pw", Label: "Pw:HR", Points: pw})
			sum = append(sum, "Pw:HR "+deltaSummary(pw, "%.1f", "%"))
		}
		p.Summary = strings.Join(sum, " · ")
		add(p)
	}
	// LT
	{
		var pts []seriesPoint
		for _, p := range points {
			if p.Tag == "LT" && p.Row.LTHR != nil {
				v := pyRound(*p.Row.LTHR, 0)
				sp := seriesPoint{Date: p.Date, Week: p.Week, Value: v, Text: fmt.Sprintf("%.0f bpm", v), Name: p.Row.Name}
				if p.Row.LTVel != nil && *p.Row.LTVel > 0 {
					sp.Aside = Pace(1 / *p.Row.LTVel).In(u)
				}
				pts = append(pts, sp)
			}
		}
		if len(pts) > 0 {
			s := deltaSummary(pts, "%.0f", " bpm")
			if a := pts[len(pts)-1].Aside; a != "" {
				s += " · " + a
			}
			add(benchPanel{Tag: "LT", Title: "Lactate threshold", Unit: "bpm",
				Series: []benchSeries{{Points: pts}}, Summary: s})
		}
	}
	// TT + RACE: one series, the goal as the line
	{
		var pts []seriesPoint
		for _, p := range points {
			if (p.Tag == "TT" || p.Tag == "RACE") && p.Row.TTS != nil {
				sp := seriesPoint{Date: p.Date, Week: p.Week, Value: float64(*p.Row.TTS),
					Text: clockOf(*p.Row.TTS), Name: p.Row.Name}
				if p.Row.TTDistM != nil {
					sp.Aside = Distance(*p.Row.TTDistM).InMeasured(u)
				}
				pts = append(pts, sp)
			}
		}
		if len(pts) > 0 {
			p := benchPanel{Tag: "TT", Title: ttTitle(goal), Unit: "time",
				Series: []benchSeries{{Points: pts}}}
			last := pts[len(pts)-1]
			p.Summary = last.Text
			if len(pts) > 1 {
				p.Summary = pts[0].Text + " → " + last.Text
			}
			if gf, err := parseClock(goal.Target); err == nil && goal.Target != "" {
				g := int(gf)
				gv := float64(g)
				p.Goal, p.GoalText = &gv, goal.Target
				switch gap := int(last.Value) - g; {
				case gap > 0:
					p.Summary += fmt.Sprintf(" · %s short of %s", clockOf(gap), goal.Target)
				case gap == 0:
					p.Summary += " · at " + goal.Target
				default:
					p.Summary += fmt.Sprintf(" · %s under %s", clockOf(-gap), goal.Target)
				}
			}
			add(p)
		}
	}
	return out
}

// deltaSummary is "214 → 221 W (+7)" for a series, or the single value.
// The sign is the arithmetic one: a decoupling that fell reads (−2.2%),
// and whether that is good is the reader's, not a flipped sign's.
func deltaSummary(pts []seriesPoint, numFmt, unit string) string {
	first, last := pts[0].Value, pts[len(pts)-1].Value
	if len(pts) == 1 {
		return fmt.Sprintf(numFmt, last) + unit
	}
	d := last - first
	sign := "+"
	if d < 0 {
		sign = "−"
	}
	return fmt.Sprintf(numFmt+" → "+numFmt+"%s (%s"+numFmt+")", first, last, unit, sign, math.Abs(d))
}

/* ── the chart ─────────────────────────────────────────────────────────── */

// chartPoint is a plotted point in SVG user units.
type chartPoint struct {
	X, Y float64
	// LabelY is where the value prints: above the marker, or below it when
	// another series' point on the same date would be printed across.
	LabelY float64
	Label  string // what prints beside the marker; "" on a dense series' ordinary points
	Text   string
	Date   string
	Week   int
	Aside  string
	Name   string
}

type chartSeries struct {
	Key      string
	Label    string
	Points   []chartPoint
	Polyline string
}

type chartTick struct {
	Y    float64
	Text string
}

type chartPanel struct {
	benchPanel
	W, H, L, T, PW, PH float64 // frame and plot box
	Lines              []chartSeries
	Ticks              []chartTick
	GoalY              *float64
	XLabels            []chartPoint // week labels along the bottom
	Baseline           float64      // the plot's bottom edge
}

// niceTicks is three to five round values inside [lo, hi]: the step is
// 1, 2 or 5 times a power of ten (a power of 60, then 300 s, for a time
// axis), the first tick the first multiple at or above lo. A reader
// compares a point to a line that says 5, not 5.4.
func niceTicks(lo, hi float64, clock bool) []float64 {
	span := hi - lo
	if span <= 0 {
		return nil
	}
	var steps []float64
	if clock {
		steps = []float64{1, 2, 5, 10, 15, 30, 60, 120, 300, 600, 900, 1800, 3600}
	} else {
		mag := math.Pow(10, math.Floor(math.Log10(span)))
		for _, m := range []float64{0.01, 0.02, 0.05, 0.1, 0.2, 0.5, 1, 2, 5, 10} {
			steps = append(steps, m*mag)
		}
	}
	step := steps[len(steps)-1]
	for _, st := range steps {
		if span/st <= 5 {
			step = st
			break
		}
	}
	var out []float64
	for v := math.Ceil(lo/step) * step; v <= hi+1e-9; v += step {
		out = append(out, v)
	}
	if len(out) < 2 { // a range narrower than any step: halve until it holds two
		return niceTicks(lo, hi+span, clock)[:2]
	}
	return out
}

// chartOf lays a panel out: x is the block's span, week 1 to its last
// day, so every panel shares one time axis; y spans the values and the
// goal with a little room. Round ticks, the line 2 px, markers 8 px — the
// marks thin, the axes recessive, the values on the points because with
// two to four measurements the labels are the data. The frame is 360
// units wide like trend.js's, so at a phone's 319 px card the 10 px text
// is still 9 px; a 640 frame halved it to 5.
func chartOf(p benchPanel, blk *Block) chartPanel {
	const W, H, L, T, R, B = 360.0, 200.0, 40.0, 16.0, 12.0, 26.0
	c := chartPanel{benchPanel: p, W: W, H: H, L: L, T: T, PW: W - L - R, PH: H - T - B}
	c.Baseline = T + c.PH
	start := blk.StartDate()
	days := float64(blk.WeekCount()*7 - 1)
	if days < 1 {
		days = 1
	}
	x := func(date string) float64 {
		t, err := time.Parse("2006-01-02", date)
		if err != nil {
			return L
		}
		return L + c.PW*float64(DaysBetween(start, t))/days
	}
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, s := range p.Series {
		for _, pt := range s.Points {
			lo, hi = math.Min(lo, pt.Value), math.Max(hi, pt.Value)
		}
	}
	if p.Goal != nil {
		lo, hi = math.Min(lo, *p.Goal), math.Max(hi, *p.Goal)
	}
	if hi-lo < 1e-9 {
		lo, hi = lo-1, hi+1
	}
	pad := (hi - lo) * 0.18
	lo, hi = lo-pad, hi+pad
	y := func(v float64) float64 { return T + c.PH*(1-(v-lo)/(hi-lo)) }
	fmtTick := func(v float64) string {
		if p.Unit == "time" {
			return clockOf(int(math.Round(v)))
		}
		if p.Unit == "%" {
			return fmt.Sprintf("%.1f", v)
		}
		return fmt.Sprintf("%.0f", v)
	}
	for _, v := range niceTicks(lo, hi, p.Unit == "time") {
		c.Ticks = append(c.Ticks, chartTick{Y: y(v), Text: fmtTick(v)})
	}
	if p.Goal != nil {
		gy := y(*p.Goal)
		c.GoalY = &gy
	}
	for _, s := range p.Series {
		cs := chartSeries{Key: s.Key, Label: s.Label}
		var pl []string
		for _, pt := range s.Points {
			cp := chartPoint{X: x(pt.Date), Y: y(pt.Value), Text: pt.Text, Date: pt.Date,
				Week: pt.Week, Aside: pt.Aside, Name: pt.Name}
			cp.LabelY = cp.Y - 9
			cp.Label = pt.Text
			cs.Points = append(cs.Points, cp)
			pl = append(pl, fmt.Sprintf("%.1f,%.1f", cp.X, cp.Y))
		}
		if p.Dense && len(cs.Points) > 2 {
			// The best of a time series is its smallest value, which on
			// this axis is the LOWEST marker (largest Y); it and the latest
			// keep their labels, the rest are markers with a table beneath.
			best := 0
			for i := range cs.Points {
				if cs.Points[i].Y > cs.Points[best].Y {
					best = i
				}
			}
			for i := range cs.Points {
				if i != best && i != len(cs.Points)-1 {
					cs.Points[i].Label = ""
				}
			}
		}
		cs.Polyline = strings.Join(pl, " ")
		c.Lines = append(c.Lines, cs)
	}
	// Two series measured on one day print two labels at one x. When the
	// points are within a label's height of each other the lower one's
	// label goes under its marker instead of across the upper one.
	for i := range c.Lines {
		for j := range c.Lines[i].Points {
			a := &c.Lines[i].Points[j]
			for k := 0; k < i; k++ {
				for _, b := range c.Lines[k].Points {
					if b.Date == a.Date && math.Abs(b.Y-a.Y) < 16 {
						if a.Y >= b.Y {
							a.LabelY = a.Y + 17
						} else {
							a.LabelY = a.Y - 9
						}
					}
				}
			}
		}
	}
	// Week labels: every fourth week, and the last.
	n := blk.WeekCount()
	for w := 1; w <= n; w++ {
		if w == 1 || w%4 == 0 || w == n {
			c.XLabels = append(c.XLabels, chartPoint{X: x(blk.DayOf(w-1, 0).Format("2006-01-02")),
				Text: "W" + strconv.Itoa(w), Week: w})
		}
	}
	return c
}

/* ── the best-effort trend ─────────────────────────────────────────────── */

// effortPanels is the best-effort trend: every run's fastest mile and
// fastest 5 km stretch across the block, the day's best where a day holds
// several runs, from the efforts table. A fitness trace between test days
// from data already owned; the 5 km panel carries the goal line when the
// goal is a 5K.
func (s *server) effortPanels(blk *Block, today time.Time) []benchPanel {
	if s.metrics == nil || len(blk.Weeks) == 0 {
		return nil
	}
	last := blk.DayOf(len(blk.Weeks)-1, 6)
	if last.After(today) {
		last = today
	}
	days, err := s.metrics.effortsByDate(blk.DayOf(0, 0).Format("2006-01-02"), last.Format("2006-01-02"))
	if err != nil {
		log.Printf("trends: reading efforts: %v", err)
		return nil
	}
	if len(days) == 0 {
		return nil
	}
	dates := make([]string, 0, len(days))
	for d := range days {
		dates = append(dates, d)
	}
	sort.Strings(dates)
	weekOf := func(date string) int {
		t, err := time.ParseInLocation("2006-01-02", date, blk.location())
		if err != nil {
			return 0
		}
		if wk, _, ok := blk.Locate(t); ok {
			return wk.N
		}
		return 0
	}
	build := func(title string, pick func(effortDay) *int, goalM float64) benchPanel {
		p := benchPanel{Tag: title, Title: title, Unit: "time", Dense: true}
		var pts []seriesPoint
		for _, d := range dates {
			v := pick(days[d])
			if v == nil {
				continue
			}
			pts = append(pts, seriesPoint{Date: d, Week: weekOf(d), Value: float64(*v), Text: clockOf(*v)})
		}
		if len(pts) == 0 {
			return p
		}
		p.Series = []benchSeries{{Points: pts}}
		best := pts[0]
		for _, x := range pts {
			if x.Value < best.Value {
				best = x
			}
		}
		lastPt := pts[len(pts)-1]
		p.Summary = fmt.Sprintf("best %s (W%d)", best.Text, best.Week)
		if lastPt.Date != best.Date {
			p.Summary += fmt.Sprintf(" · latest %s (W%d)", lastPt.Text, lastPt.Week)
		}
		if goalM > 0 && goalMetres(blk.Goal.Event) == goalM {
			if gf, err := parseClock(blk.Goal.Target); err == nil && blk.Goal.Target != "" {
				gv := float64(int(gf))
				p.Goal, p.GoalText = &gv, blk.Goal.Target
			}
		}
		return p
	}
	var out []benchPanel
	if p := build("Fastest mile", func(e effortDay) *int { return e.MileS }, 0); len(p.Series) > 0 {
		out = append(out, p)
	}
	if p := build("Fastest 5 km stretch", func(e effortDay) *int { return e.K5S }, 5000); len(p.Series) > 0 {
		out = append(out, p)
	}
	return out
}
