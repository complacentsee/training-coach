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
//   - A sample whose interval spans a RECORDING GAP (dt ≥ recordingGapS,
//     the same threshold stopsIn reports stops with) carries ZERO weight in
//     every statistic and histogram — since 17 Aug 2026. One second of
//     post-resume data is not evidence about the twelve minutes the watch
//     was stopped, and weighting it as if it were turned a phone call into
//     "rode too easy": a 741 s gap sample at HR 108 dragged a mid-band ride
//     from 70.7% in-band to 54.1%, average power from 122.8 W to 98.6, and
//     manufactured a −11.9 drift. Statistics are therefore MOVING-time
//     statistics; elapsed_s alone keeps the wall clock, and moving_s says
//     how much of it was recorded work.
//   - Windowed statistics (drift halves, first-20-min, the bike's
//     after-warm-up window) select samples by PREDICATE on the original
//     stream, each keeping its own weight — never by building a sublist and
//     re-weighting inside it, which silently bridged excluded time onto the
//     next kept sample and dropped each window's first sample entirely.
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
	// MovingS is the elapsed seconds minus every recording gap — the time
	// the statistics are weighted over. Equal to ElapsedS on a file with no
	// stops, which is most of them.
	MovingS int
	Records int

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

	// Weather is frozen at import rather than derived on each read: it is
	// what the conditions WERE, and the provider's reanalysis is revised.
	Weather *conditions

	SHA256 string
}

// hrValid is the dropout rule's one boundary.
func hrValid(v float64) bool { return v >= 50 }

// sampleWeights is every statistic's clock: w[i] is the seconds sample i
// covers, zero when the interval is non-positive (a chained file's seam) or
// spans a recording gap (the watch was stopped — one post-resume second is
// not evidence about the minutes before it). moving is their sum, the
// recorded work the wall clock held.
func sampleWeights(t []int) (w []int, moving int) {
	gapS := recordingGapS(t)
	w = make([]int, len(t))
	for i := 1; i < len(t); i++ {
		if dt := t[i] - t[i-1]; dt > 0 && dt < gapS {
			w[i] = dt
			moving += dt
		}
	}
	return w, moving
}

// weightedMean is (total seconds kept, mean) over paired samples, sample i
// covering w[i] seconds. Returns nil when nothing qualifies.
func weightedMean(w []int, vals []float64, keep func(float64) bool) (float64, *float64) {
	var tot, totV float64
	for i := 1; i < len(w); i++ {
		if w[i] > 0 && keep(vals[i]) {
			tot += float64(w[i])
			totV += float64(w[i]) * vals[i]
		}
	}
	if tot == 0 {
		return 0, nil
	}
	m := totV / tot
	return tot, &m
}

// timeShare is the time-weighted share of valid samples satisfying pred.
func timeShare(w []int, vals []float64, pred, valid func(float64) bool) *float64 {
	var num, den float64
	for i := 1; i < len(w); i++ {
		if w[i] > 0 && valid(vals[i]) {
			den += float64(w[i])
			if pred(vals[i]) {
				num += float64(w[i])
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
	w, moving := sampleWeights(s.Time)
	a := &activityMetrics{Name: name, Date: date, Sport: s.Sport,
		StartUTC: s.StartUTC.Format("2006-01-02T15:04:05Z"),
		ElapsedS: s.Time[len(s.Time)-1], MovingS: moving, Records: len(s.Time),
		DistanceM: s.DistM, HRHist: map[int]int{}}
	all := func(float64) bool { return true }
	hrF := intsToFloats(s.HR)

	if s.HaveHR {
		a.DropoutShare = timeShare(w, hrF, func(v float64) bool { return !hrValid(v) }, all)
		_, a.AvgHR = weightedMean(w, hrF, hrValid)
		mx := 0
		for _, v := range s.HR {
			if v >= 50 && v > mx {
				mx = v
			}
		}
		if mx > 0 {
			a.MaxHR = &mx
		}
		// Drift: second-half mean minus first-half mean, split at half the
		// elapsed time — each sample selected where it sits and keeping its
		// own weight, per the windowed-statistics convention.
		half := float64(a.ElapsedS) / 2
		var w1, w2 float64
		var s1, s2 float64
		for i := 1; i < len(w); i++ {
			if w[i] == 0 || !hrValid(hrF[i]) {
				continue
			}
			if float64(s.Time[i]) <= half {
				w1 += float64(w[i])
				s1 += float64(w[i]) * hrF[i]
			} else {
				w2 += float64(w[i])
				s2 += float64(w[i]) * hrF[i]
			}
		}
		if w1 > 0 && w2 > 0 {
			d := s2/w2 - s1/w1
			a.HRDrift = &d
		}
		// Histogram: every weighted sample's seconds at its bpm, missing
		// included as 0. A gap-spanning sample has no seconds to deposit.
		for i := 1; i < len(w); i++ {
			if w[i] > 0 {
				a.HRHist[s.HR[i]] += w[i]
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
		if output != nil && w1 > 0 && w2 > 0 {
			a.DecouplingPct = windowDecoupling(s, w, hrF, output, 0, float64(a.ElapsedS))
		}
	}
	if s.HaveWatts {
		_, a.AvgPower = weightedMean(w, intsToFloats(s.Watts), all)
		a.PowerHist = map[int]int{}
		for i := 1; i < len(w); i++ {
			if w[i] > 0 {
				a.PowerHist[s.Watts[i]] += w[i]
			}
		}
	}
	if s.HaveCad {
		_, a.AvgCadence = weightedMean(w, intsToFloats(s.Cad),
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

// A recording gap is an interval no sample describes: the recording
// stopped — auto-pause, a paused timer, a device that lost the plot — and
// the sample at the far end reports the moment it resumed, not the minutes
// in between. The threshold is the file's own, because a device's recording
// rate is its own: smart recording writes a sample when something changes,
// so twelve seconds between samples on a file whose median interval is four
// describes twelve steady seconds, while the same twelve on a 1 Hz file are
// eleven samples that do not exist.
// Measured 15 Aug 2026 over twelve archived files and the committed corpus:
// smart-recording intervals reach 17 s against medians of 2-4 s, while
// every stop measured runs 57 s or longer against a median of 1.
const (
	gapFloorS      = 10 // never call anything shorter a gap
	gapCadenceMult = 5  // …nor anything this close to the file's own rate
)

// recordingGapS is the interval at which this file stopped recording rather
// than merely sampled slowly.
func recordingGapS(t []int) int {
	dts := make([]int, 0, len(t))
	for i := 1; i < len(t); i++ {
		if dt := t[i] - t[i-1]; dt > 0 {
			dts = append(dts, dt)
		}
	}
	if len(dts) == 0 {
		return gapFloorS
	}
	sort.Ints(dts)
	if g := dts[len(dts)/2] * gapCadenceMult; g > gapFloorS {
		return g
	}
	return gapFloorS
}

// describedDistance integrates the speed stream into cumulative metres and
// counts recording gaps alongside it: gaps[i] is how many fall at or before
// sample i, so a window (i, j] spans one exactly when gaps[j] > gaps[i].
//
// A gap adds no distance, because none is known. Carrying the far sample's
// speed across one measured +27.5% on the 12 Aug 2026 ride — 9,551 m
// integrated against a 7,490 m odometer, all of it inside a single 296 s
// stop. Dropping the gap lands within 0.07% of that odometer, and the
// gap-free files stay where they already were, inside 0.15%. A non-positive
// interval is a gap too: a chained file's seam can step the clock backwards,
// and whatever ground was covered across it is not in this file either.
func describedDistance(s *activityStreams) (dist []float64, gaps []int) {
	n := len(s.Time)
	dist, gaps = make([]float64, n), make([]int, n)
	gapS := recordingGapS(s.Time)
	// A stream with no speed still has gaps to count: a trainer session with
	// no speed sensor carries Vel nil, and reading it as a distance of zero
	// is right where reading it at all was a crash.
	haveVel := len(s.Vel) == n
	for i := 1; i < n; i++ {
		dt := s.Time[i] - s.Time[i-1]
		dist[i], gaps[i] = dist[i-1], gaps[i-1]
		if dt <= 0 || dt >= gapS {
			gaps[i]++
			continue
		}
		if haveVel {
			dist[i] += s.Vel[i] * float64(dt)
		}
	}
	return dist, gaps
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
//
// A stretch spanning a recording gap is never a candidate: how far the
// athlete travelled while the recording was stopped is not in the file, and
// a stretch whose distance is unknown cannot be ranked against one whose
// distance is measured. Asked for more stretches than the trace can offer
// gap-free, it returns the ones it has.
func fastestSegments(s *activityStreams, meters float64, secs int, count int, u Units) []segment {
	if count < 1 || len(s.Time) < 2 || !s.HaveVel || (meters <= 0 && secs <= 0) {
		return nil
	}
	// Cumulative distance and, for the HR of a stretch, cumulative HR·time.
	// HR keeps counting through a gap, as time-weighting does everywhere in
	// the register — no stretch that spans one survives to report it.
	n := len(s.Time)
	dist, gaps := describedDistance(s)
	hrSum := make([]float64, n)
	hrDur := make([]float64, n)
	for i := 1; i < n; i++ {
		dt := float64(s.Time[i] - s.Time[i-1])
		if dt <= 0 {
			dt = 0
		}
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
		if gaps[j] > gaps[i] {
			continue
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

// bestEffort is the fewest seconds any gap-free stretch of meters took in
// the recording, nil when it holds none that long. The best-effort trend
// (/trends) asks every run for its fastest mile and its fastest 5 km; the
// arithmetic is fastestSegments' with count 1, mirrored in
// grade_metrics.py as fastest_1mi_s / fastest_5k_s and pinned by the gate
// — the first mirror fastestSegments has had.
func bestEffort(s *activityStreams, meters float64) *int {
	segs := fastestSegments(s, meters, 0, 1, Imperial)
	if len(segs) == 0 {
		return nil
	}
	v := int(math.Round(segs[0].Secs))
	return &v
}

// bestRolling is the highest time-weighted mean of vals over any window of
// `window` seconds. Recording gaps count as ELAPSED time here, deliberately
// outside the gap rule: this is max-seeking, a window spanning a stop can
// only lose, and both registers keep it that way in step. Returns nil when
// the trace is shorter than the
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
func halfEfficiency(t, w []int, hr, output []float64, lo, hi float64) *float64 {
	var num, den float64
	for i := 1; i < len(t); i++ {
		ti := float64(t[i])
		if w[i] > 0 && lo < ti && ti <= hi && hrValid(hr[i]) && output[i] > 0 {
			num += float64(w[i]) * (output[i] / hr[i])
			den += float64(w[i])
		}
	}
	if den == 0 {
		return nil
	}
	v := num / den
	return &v
}

// windowDecoupling is decoupling over the window (lo, hi] of stream
// seconds: the first half's mean output/HR over the second half's, minus
// one, in percent. The halves split at the window's midpoint; on a bike
// the window's first 600 s are excluded, the register's warm-up rule.
// Over (0, elapsed] this IS the row's decoupling_pct — computeMetrics calls
// it so — and over a lap's span it is the same measurement of one step,
// which is what a decoupling test run inside a longer file needs. Mirrored
// as decoupling_pa_pct / decoupling_pw_pct in grade_metrics.py, which
// pins the output choice the caller makes here: velocity for Pa:HR, watts
// for Pw:HR.
func windowDecoupling(s *activityStreams, w []int, hr, output []float64, lo, hi float64) *float64 {
	start := lo
	if s.Sport == "cycling" {
		start += 600
	}
	if start >= hi {
		return nil
	}
	mid := start + (hi-start)/2
	e1 := halfEfficiency(s.Time, w, hr, output, start, mid)
	e2 := halfEfficiency(s.Time, w, hr, output, mid, hi)
	if e1 == nil || e2 == nil || *e1 == 0 || *e2 == 0 {
		return nil
	}
	d := (*e1 / *e2 - 1) * 100
	return &d
}

// windowMean is the weighted mean of vals over the samples with lo < t ≤ hi
// that keep pass, each sample keeping its own weight — the windowed-
// statistics convention. nil when nothing qualifies. The final twenty
// minutes of a threshold test is windowMean over (hi-1200, hi].
func windowMean(t, w []int, vals []float64, lo, hi float64, keep func(float64) bool) *float64 {
	var num, den float64
	for i := 1; i < len(t); i++ {
		ti := float64(t[i])
		if w[i] > 0 && lo < ti && ti <= hi && keep(vals[i]) {
			num += float64(w[i]) * vals[i]
			den += float64(w[i])
		}
	}
	if den == 0 {
		return nil
	}
	v := num / den
	return &v
}

// windowBest is bestRolling over the samples inside [lo, hi] — the best
// 60 s of a ramp test that sits in one lap of a longer file. Over the whole
// stream it is bestRolling itself.
func windowBest(t []int, vals []float64, lo, hi int, window int) *float64 {
	i, j := 0, len(t)
	for i < len(t) && t[i] < lo {
		i++
	}
	for j > i && t[j-1] > hi {
		j--
	}
	if j-i < 2 {
		return nil
	}
	return bestRolling(t[i:j], vals[i:j], window)
}

/* ── anchor-dependent grade inputs, computed at query time ─────────────── */

// runGradeShare is the share of valid HR time at or under the cap — the
// number a run legend's bands read.
func runGradeShare(s *activityStreams, cap int) *float64 {
	if !s.HaveHR {
		return nil
	}
	w, _ := sampleWeights(s.Time)
	hrF := intsToFloats(s.HR)
	return timeShare(w, hrF, func(v float64) bool { return v <= float64(cap) }, hrValid)
}

// runFirst20Mean is the mean HR over the first 1200 s — valid samples
// selected in place, each keeping its own weight.
func runFirst20Mean(s *activityStreams) *float64 {
	if !s.HaveHR {
		return nil
	}
	w, _ := sampleWeights(s.Time)
	var tot, sum float64
	for i := 1; i < len(w); i++ {
		if w[i] > 0 && s.Time[i] <= 1200 && hrValid(float64(s.HR[i])) {
			tot += float64(w[i])
			sum += float64(w[i]) * float64(s.HR[i])
		}
	}
	if tot == 0 {
		return nil
	}
	m := sum / tot
	return &m
}

// bikeGradeInput is the in-band share after the 600 s warm-up (measured
// over the filtered after-warm-up list) and the whole-ride seconds over the
// cap, rounded — the numbers a bike judgment reads.
func bikeGradeInput(s *activityStreams, lo, hi, cap int) (inBand *float64, secsOver *int) {
	if !s.HaveHR {
		return nil, nil
	}
	w, _ := sampleWeights(s.Time)
	hrF := intsToFloats(s.HR)
	// The after-warm-up window, selected in place with each sample's own
	// weight — the windowed-statistics convention.
	var num, den float64
	for i := 1; i < len(w); i++ {
		if w[i] > 0 && s.Time[i] > 600 && hrValid(hrF[i]) {
			den += float64(w[i])
			if float64(lo) <= hrF[i] && hrF[i] <= float64(hi) {
				num += float64(w[i])
			}
		}
	}
	if den > 0 {
		v := num / den
		inBand = &v
	}
	kept, _ := weightedMean(w, hrF, hrValid)
	over := timeShare(w, hrF, func(v float64) bool { return v > float64(cap) }, hrValid)
	if over != nil {
		n := int(pyRound(*over*kept, 0)) // banker's, as the mirror rounds
		secsOver = &n
	}
	return inBand, secsOver
}

// stop is one interruption the RECORDING itself states: an interval the
// device did not sample through, because the timer stopped. Where it
// happened is reported in distance as well as in time, because that is how
// an athlete finds it again — "at 9.3 miles" is a place on a route, where
// "at 90:49" is a place in a clock nobody replays.
type stop struct {
	AtS     int     `json:"at_s"`      // seconds from the first sample
	AtHMS   string  `json:"at_hms"`    // the same, spoken
	AtDistM float64 `json:"at_dist_m"` // ground covered before it
	AtDist  string  `json:"at_dist"`   // the same, in the athlete's units
	Secs    int     `json:"secs"`
}

// maxStops bounds the list. A session with more interruptions than this has
// a story the count tells better than an enumeration.
const maxStops = 20

// stopsIn finds every recording gap and says where it fell. It reports only
// what the file states: a gap is the clock stopping, which is auto-pause or
// a paused watch. Standing still with the timer RUNNING is invisible here
// and stays invisible — finding it needs a speed threshold, and inventing a
// movement rule is exactly what this register refuses to do (measured on one
// walk: a gap rule gives 616 s where a 0.5 m/s threshold gives 957 s against
// a timer time of 1,204.9 s).
func stopsIn(s *activityStreams, u Units) ([]stop, int) {
	if len(s.Time) < 2 {
		return nil, 0
	}
	dist, _ := describedDistance(s)
	gapS := recordingGapS(s.Time)
	var out []stop
	total := 0
	for i := 1; i < len(s.Time); i++ {
		dt := s.Time[i] - s.Time[i-1]
		if dt < gapS {
			continue
		}
		total += dt
		if len(out) >= maxStops {
			continue
		}
		st := stop{AtS: s.Time[i-1], AtHMS: clock(s.Time[i-1]), Secs: dt}
		if s.HaveVel {
			st.AtDistM = pyRound(dist[i-1], 1)
			st.AtDist = Distance(dist[i-1]).InMeasured(u)
		}
		out = append(out, st)
	}
	return out, total
}
