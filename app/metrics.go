package main

// The measurement register: deterministic workout metrics, the arithmetic
// behind a grade. THIS FILE IS THE REGISTER'S AUTHORITATIVE HOME — it moved
// here from tools/grade_metrics.py on 14 Aug 2026, and that script is now
// the cross-validation mirror: a convention changes here and in the mirror
// together, or not at all. The conversion half (how FIT bytes become the
// streams this file consumes) lives on decode.go.
//
// Pinned conventions — change them here or not at all:
//
//   - Every share is TIME-WEIGHTED: sample i covers time[i] − time[i−1]
//     seconds. Never count samples; resampled streams only look uniform.
//     A non-positive interval contributes nothing — clocks can step
//     backwards at a chained file's seam, and negative seconds subtracting
//     from a share is how the histogram and the stream computation were
//     once able to disagree about the same run.
//   - HR dropouts: samples under 50 bpm are excluded from all HR statistics
//     and the excluded share is reported (dropout_share). Over 5% excluded,
//     a grade note must say the HR numbers are contaminated.
//   - Runs are graded on the whole run, warm-up, strides and all — that is
//     what a legend's "share of minutes under the cap" has always meant.
//   - Bikes: the in-band share is measured after the first 600 s, because a
//     ride should spend its warm-up below the band; the cap applies to the
//     whole ride.
//   - Drift is second-half mean HR minus first-half mean HR, split at half
//     the elapsed time.
//   - Decoupling is (first-half output/HR)/(second-half output/HR) − 1, in
//     percent, using watts when present and velocity otherwise; absent both
//     it is omitted, never approximated. On a bike the first 600 s are
//     excluded — the warm-up's rising HR reads as decoupling and swamps the
//     signal. A run keeps its whole trace because the DEC protocol is a
//     cold start by design.
//   - Run cadence is recorded per-leg and reported DOUBLED at presentation;
//     the stored average is as recorded.
//   - The time-at-value histograms store EVERY sample's dt at its value —
//     bpm 0 (missing) included — so any threshold rule applies losslessly
//     at query time. Validity (bpm ≥ 50) is the query's rule, not the
//     histogram's.
//   - Anchors (HR bands, FTP, weight) are read only at query/grade time,
//     from athlete.json as it is at that moment — never at import, never
//     from an older document. Import stores anchor-free aggregates only.

import (
	"math"
	"math/big"
	"sort"
)

// activityMetrics is one activity's anchor-free aggregate row plus its
// time-at-value histograms — what the import pipeline writes to the DB.
type activityMetrics struct {
	Name     string
	Date     string // the training day, YYYY-MM-DD
	Sport    string
	StartUTC string // RFC3339
	ElapsedS int
	Records  int

	AvgHR         *float64
	MaxHR         *int
	DropoutShare  *float64
	HRDrift       *float64
	DecouplingPct *float64
	AvgCadence    *float64
	AvgPower      *float64
	DistanceM     *float64

	HRHist    map[int]int // bpm → seconds, ALL samples including <50
	PowerHist map[int]int // exact watts → seconds

	SHA256 string
}

// hrValid is the dropout rule's one boundary.
func hrValid(v float64) bool { return v >= 50 }

// weightedMean is (total seconds kept, mean) over paired samples, sample i
// covering t[i]−t[i−1] seconds. Returns nil when nothing qualifies.
func weightedMean(t []int, vals []float64, keep func(float64) bool) (float64, *float64) {
	var tot, totV float64
	for i := 1; i < len(t); i++ {
		dt := float64(t[i] - t[i-1])
		if dt <= 0 {
			continue
		}
		if keep(vals[i]) {
			tot += dt
			totV += dt * vals[i]
		}
	}
	if tot == 0 {
		return 0, nil
	}
	m := totV / tot
	return tot, &m
}

// timeShare is the time-weighted share of valid samples satisfying pred.
func timeShare(t []int, vals []float64, pred, valid func(float64) bool) *float64 {
	var num, den float64
	for i := 1; i < len(t); i++ {
		dt := float64(t[i] - t[i-1])
		if dt <= 0 {
			continue
		}
		if valid(vals[i]) {
			den += dt
			if pred(vals[i]) {
				num += dt
			}
		}
	}
	if den == 0 {
		return nil
	}
	v := num / den
	return &v
}

func intsToFloats(xs []int) []float64 {
	out := make([]float64, len(xs))
	for i, x := range xs {
		out[i] = float64(x)
	}
	return out
}

// computeMetrics runs the register over one decoded activity. date is the
// training day the caller derived (device name first, start time second) —
// the register itself is date-blind.
func computeMetrics(name, date string, s *activityStreams) *activityMetrics {
	a := &activityMetrics{Name: name, Date: date, Sport: s.Sport,
		StartUTC: s.StartUTC.Format("2006-01-02T15:04:05Z"),
		ElapsedS: s.Time[len(s.Time)-1], Records: len(s.Time),
		DistanceM: s.DistM, HRHist: map[int]int{}}
	all := func(float64) bool { return true }
	hrF := intsToFloats(s.HR)

	if s.HaveHR {
		a.DropoutShare = timeShare(s.Time, hrF, func(v float64) bool { return !hrValid(v) }, all)
		_, a.AvgHR = weightedMean(s.Time, hrF, hrValid)
		mx := 0
		for _, v := range s.HR {
			if v >= 50 && v > mx {
				mx = v
			}
		}
		if mx > 0 {
			a.MaxHR = &mx
		}
		// Drift: valid samples split at half the elapsed time, a weighted
		// mean within each half's own filtered list.
		half := float64(a.ElapsedS) / 2
		var t1, t2 []int
		var v1, v2 []float64
		for i, ti := range s.Time {
			if !hrValid(hrF[i]) {
				continue
			}
			if float64(ti) <= half {
				t1, v1 = append(t1, ti), append(v1, hrF[i])
			} else {
				t2, v2 = append(t2, ti), append(v2, hrF[i])
			}
		}
		if len(t1) > 0 && len(t2) > 0 {
			_, m1 := weightedMean(t1, v1, hrValid)
			_, m2 := weightedMean(t2, v2, hrValid)
			if m1 != nil && m2 != nil {
				d := *m2 - *m1
				a.HRDrift = &d
			}
		}
		// Histogram: every sample's dt at its bpm, missing included as 0.
		for i := 1; i < len(s.Time); i++ {
			dt := s.Time[i] - s.Time[i-1]
			if dt > 0 {
				a.HRHist[s.HR[i]] += dt
			}
		}
		// Decoupling: watts preferred, else velocity; bike drops the first
		// 600 s per the register's warm-up rule.
		var output []float64
		if s.HaveWatts {
			output = intsToFloats(s.Watts)
		} else if s.HaveVel {
			output = s.Vel
		}
		if output != nil && len(t1) > 0 && len(t2) > 0 {
			start := 0.0
			if s.Sport == "cycling" {
				start = 600
			}
			mid := start + (float64(a.ElapsedS)-start)/2
			e1 := halfEfficiency(s.Time, hrF, output, start, mid)
			e2 := halfEfficiency(s.Time, hrF, output, mid, float64(a.ElapsedS))
			if e1 != nil && e2 != nil && *e1 != 0 && *e2 != 0 {
				d := (*e1 / *e2 - 1) * 100
				a.DecouplingPct = &d
			}
		}
	}
	if s.HaveWatts {
		_, a.AvgPower = weightedMean(s.Time, intsToFloats(s.Watts), all)
		a.PowerHist = map[int]int{}
		for i := 1; i < len(s.Time); i++ {
			dt := s.Time[i] - s.Time[i-1]
			if dt > 0 {
				a.PowerHist[s.Watts[i]] += dt
			}
		}
	}
	if s.HaveCad {
		_, a.AvgCadence = weightedMean(s.Time, intsToFloats(s.Cad),
			func(v float64) bool { return v > 0 })
	}
	return a
}

// profilePoint is one bucket of the session's shape: where the work
// actually sat over time.
type profilePoint struct {
	Min  int      `json:"min"`            // minutes from the first sample
	W    *float64 `json:"w,omitempty"`    // mean watts over the bucket
	HR   *float64 `json:"hr,omitempty"`   // mean valid HR over the bucket
	Pace string   `json:"pace,omitempty"` // mean pace, athlete's units, runs only
}

// sessionProfile buckets the ride or run into at most maxPoints even spans
// and reports mean power and HR in each. Aggregates cannot show execution:
// an average, a peak and a best minute describe a ramp to failure and a
// ride with one surge identically, and the difference is the whole verdict
// on a test day. The shape is what separates them — and on an interval day
// it is what shows whether the intervals were ridden at all.
func sessionProfile(s *activityStreams, maxPoints int, u Units) []profilePoint {
	if len(s.Time) < 2 || maxPoints < 1 {
		return nil
	}
	elapsed := s.Time[len(s.Time)-1]
	if elapsed <= 0 {
		return nil
	}
	// Whole minutes per bucket, so the axis reads in the units the
	// prescription is written in.
	mins := (elapsed + 59) / 60
	perBucket := (mins + maxPoints - 1) / maxPoints
	if perBucket < 1 {
		perBucket = 1
	}
	span := perBucket * 60

	hrF := intsToFloats(s.HR)
	var wF []float64
	if s.HaveWatts {
		wF = intsToFloats(s.Watts)
	}
	// Pace, not speed, and in the athlete's own units, because that is how
	// the prescription states its targets — a grader comparing "9:41/mi" to
	// "9:45/mi" is doing the same arithmetic the athlete does.
	pacing := s.Sport == "running" && s.HaveVel

	var out []profilePoint
	for start := 0; start < elapsed; start += span {
		end := start + span
		var wSum, wDur, hSum, hDur, vSum, vDur float64
		for i := 1; i < len(s.Time); i++ {
			ti := s.Time[i]
			if ti <= start || ti > end {
				continue
			}
			dt := float64(s.Time[i] - s.Time[i-1])
			if dt <= 0 {
				continue
			}
			if wF != nil {
				wSum += dt * wF[i]
				wDur += dt
			}
			if s.HaveHR && hrValid(hrF[i]) {
				hSum += dt * hrF[i]
				hDur += dt
			}
			if pacing {
				vSum += dt * s.Vel[i]
				vDur += dt
			}
		}
		p := profilePoint{Min: start / 60}
		if wDur > 0 {
			v := pyRound(wSum/wDur, 1)
			p.W = &v
		}
		if hDur > 0 {
			v := pyRound(hSum/hDur, 1)
			p.HR = &v
		}
		if vDur > 0 && vSum > 0 {
			p.Pace = Pace(vDur / vSum).In(u)
		}
		if p.W != nil || p.HR != nil || p.Pace != "" {
			out = append(out, p)
		}
	}
	return out
}

// segment is one measured stretch of a run: what it covered, how long it
// took, and what it cost.
type segment struct {
	StartS int     `json:"start_s"` // seconds from the first sample
	Secs   float64 `json:"secs"`
	DistM  float64 `json:"dist_m"`
	Pace   string  `json:"pace"`
	AvgHR  float64 `json:"avg_hr,omitempty"`
}

// fastestSegments finds the count fastest non-overlapping stretches of a
// given length — the reps, when a session was run as reps.
//
// It exists because the athlete does not lap his repetitions: the watch
// records one auto-lap a mile and nothing else, so a session of eight 200s
// arrives as a single smooth trace with the reps buried in it, and mile
// splits average each rep together with its recovery. Asking the stream for
// the eight fastest 200 m stretches recovers what the laps would have said,
// from data that is already there.
//
// Distance is integrated from the speed stream rather than read from the
// odometer, so it measures the same thing everything else here does. Either
// meters or secs is given, never both: reps are prescribed as a distance
// ("8×200") or as a duration ("4×5:00").
func fastestSegments(s *activityStreams, meters float64, secs int, count int, u Units) []segment {
	if count < 1 || len(s.Time) < 2 || !s.HaveVel || (meters <= 0 && secs <= 0) {
		return nil
	}
	// Cumulative distance and, for the HR of a stretch, cumulative HR·time.
	n := len(s.Time)
	dist := make([]float64, n)
	hrSum := make([]float64, n)
	hrDur := make([]float64, n)
	for i := 1; i < n; i++ {
		dt := float64(s.Time[i] - s.Time[i-1])
		if dt <= 0 {
			dt = 0
		}
		dist[i] = dist[i-1] + s.Vel[i]*dt
		hrSum[i], hrDur[i] = hrSum[i-1], hrDur[i-1]
		if s.HaveHR && hrValid(float64(s.HR[i])) {
			hrSum[i] += dt * float64(s.HR[i])
			hrDur[i] += dt
		}
	}

	type cand struct{ i, j int }
	var cands []cand
	j := 0
	for i := 0; i < n; i++ {
		if meters > 0 {
			for j < n && dist[j]-dist[i] < meters {
				j++
			}
		} else {
			for j < n && s.Time[j]-s.Time[i] < secs {
				j++
			}
		}
		if j >= n {
			break
		}
		cands = append(cands, cand{i, j})
	}
	// Fastest first: least time for the distance, or most distance for the
	// time.
	sort.Slice(cands, func(a, b int) bool {
		ca, cb := cands[a], cands[b]
		if meters > 0 {
			return s.Time[ca.j]-s.Time[ca.i] < s.Time[cb.j]-s.Time[cb.i]
		}
		return dist[ca.j]-dist[ca.i] > dist[cb.j]-dist[cb.i]
	})

	var out []segment
	var taken []cand
	for _, c := range cands {
		if len(out) == count {
			break
		}
		overlaps := false
		for _, t := range taken {
			if c.i < t.j && t.i < c.j {
				overlaps = true
				break
			}
		}
		if overlaps {
			continue
		}
		taken = append(taken, c)
		d := dist[c.j] - dist[c.i]
		t := float64(s.Time[c.j] - s.Time[c.i])
		seg := segment{StartS: s.Time[c.i], Secs: pyRound(t, 1), DistM: pyRound(d, 1)}
		if d > 0 {
			seg.Pace = Pace(t / d).In(u)
		}
		if hd := hrDur[c.j] - hrDur[c.i]; hd > 0 {
			seg.AvgHR = pyRound((hrSum[c.j]-hrSum[c.i])/hd, 1)
		}
		out = append(out, seg)
	}
	// Chronological, so a reader sees them as they were run — whether they
	// held or faded is the point.
	sort.Slice(out, func(a, b int) bool { return out[a].StartS < out[b].StartS })
	return out
}

// bestRolling is the highest time-weighted mean of vals over any window of
// `window` seconds. Recording gaps count as elapsed time, as everywhere
// else in the register. Returns nil when the trace is shorter than the
// window. This is what a ramp test is actually judged on — an average over
// the whole ride says "steady Z2" about a session that climbed to failure,
// which is how a valid test gets read as a soft one.
func bestRolling(t []int, vals []float64, window int) *float64 {
	if len(t) < 2 || t[len(t)-1]-t[0] < window {
		return nil
	}
	// Prefix sums over the same dt weighting the rest of the register uses.
	sum := make([]float64, len(t))
	dur := make([]float64, len(t))
	for i := 1; i < len(t); i++ {
		dt := float64(t[i] - t[i-1])
		if dt <= 0 {
			dt = 0
		}
		sum[i] = sum[i-1] + dt*vals[i]
		dur[i] = dur[i-1] + dt
	}
	var best *float64
	j := 0
	for i := 0; i < len(t); i++ {
		for j < len(t) && t[j]-t[i] < window {
			j++
		}
		if j >= len(t) {
			break
		}
		if d := dur[j] - dur[i]; d > 0 {
			if m := (sum[j] - sum[i]) / d; best == nil || m > *best {
				v := m
				best = &v
			}
		}
	}
	return best
}

// pyRound mirrors python's round(): the float's exact value rounded to the
// nearest multiple of 10^-n, ties to even. Computed over exact rationals —
// the obvious RoundToEven(x*10^n)/10^n rounds the intermediate product and
// measurably diverges from python on tie quotients the register actually
// produces (2007/20 → 100.4 vs python's 100.3; 2049/20 → 102.4 vs 102.5).
// The mirror rounds its output, so the register rounds identically where
// numbers are presented — never where they are stored.
func pyRound(x float64, n int) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return x
	}
	r := new(big.Rat).SetFloat64(x)
	pow := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
	r.Mul(r, new(big.Rat).SetInt(pow)) // exact x·10ⁿ
	num, den := r.Num(), r.Denom()
	q, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	twice := new(big.Int).Lsh(new(big.Int).Abs(rem), 1)
	if c := twice.Cmp(den); c > 0 || (c == 0 && q.Bit(0) == 1) {
		if num.Sign() >= 0 {
			q.Add(q, big.NewInt(1))
		} else {
			q.Sub(q, big.NewInt(1))
		}
	}
	f, _ := new(big.Rat).SetFrac(q, pow).Float64()
	return f
}

// halfEfficiency is the mean output/HR over (lo, hi], valid HR and positive
// output only — decoupling's building block.
func halfEfficiency(t []int, hr, output []float64, lo, hi float64) *float64 {
	var num, den float64
	for i := 1; i < len(t); i++ {
		ti := float64(t[i])
		dt := float64(t[i] - t[i-1])
		if dt <= 0 {
			continue
		}
		if lo < ti && ti <= hi && hrValid(hr[i]) && output[i] > 0 {
			num += dt * (output[i] / hr[i])
			den += dt
		}
	}
	if den == 0 {
		return nil
	}
	v := num / den
	return &v
}

/* ── anchor-dependent grade inputs, computed at query time ─────────────── */

// runGradeShare is the share of valid HR time at or under the cap — the
// number a run legend's bands read.
func runGradeShare(s *activityStreams, cap int) *float64 {
	if !s.HaveHR {
		return nil
	}
	hrF := intsToFloats(s.HR)
	return timeShare(s.Time, hrF, func(v float64) bool { return v <= float64(cap) }, hrValid)
}

// runFirst20Mean is the mean HR over the first 1200 s (valid samples,
// weighted within the filtered list — the register's drift-halves idiom).
func runFirst20Mean(s *activityStreams) *float64 {
	if !s.HaveHR {
		return nil
	}
	var t []int
	var v []float64
	for i, ti := range s.Time {
		if ti <= 1200 && hrValid(float64(s.HR[i])) {
			t, v = append(t, ti), append(v, float64(s.HR[i]))
		}
	}
	if len(t) == 0 {
		return nil
	}
	_, m := weightedMean(t, v, hrValid)
	return m
}

// bikeGradeInput is the in-band share after the 600 s warm-up (measured
// over the filtered after-warm-up list) and the whole-ride seconds over the
// cap, rounded — the numbers a bike judgment reads.
func bikeGradeInput(s *activityStreams, lo, hi, cap int) (inBand *float64, secsOver *int) {
	if !s.HaveHR {
		return nil, nil
	}
	hrF := intsToFloats(s.HR)
	var tw []int
	var vw []float64
	for i, ti := range s.Time {
		if ti > 600 && hrValid(hrF[i]) {
			tw, vw = append(tw, ti), append(vw, hrF[i])
		}
	}
	inBand = timeShare(tw, vw, func(v float64) bool { return float64(lo) <= v && v <= float64(hi) },
		func(float64) bool { return true })
	kept, _ := weightedMean(s.Time, hrF, hrValid)
	over := timeShare(s.Time, hrF, func(v float64) bool { return v > float64(cap) }, hrValid)
	if over != nil {
		n := int(pyRound(*over*kept, 0)) // banker's, as the mirror rounds
		secsOver = &n
	}
	return inBand, secsOver
}
