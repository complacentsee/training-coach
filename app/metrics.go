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
