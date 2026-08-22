package main

import (
	"encoding/json"
	"fmt"
	"html"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/muktihari/fit/profile/mesgdef"
	"github.com/muktihari/fit/profile/typedef"
	"github.com/muktihari/fit/proto"
)

// A synthetic recording is a list of segments, each a constant or linear
// ramp of speed, HR and power over its seconds, with one lap per segment
// carrying the step index it says. Records are one per second.
type benchSeg struct {
	secs       int
	vel        float64 // m/s
	hr0, hr1   float64 // bpm, linear across the segment
	w0, w1     float64 // watts, linear; 0 = no power
	step       int     // -1: a lap with no step index
	lapTrigger typedef.LapTrigger
}

func benchFixture(t *testing.T, date time.Time, sport typedef.Sport, segs []benchSeg) []byte {
	t.Helper()
	t0 := time.Date(date.Year(), date.Month(), date.Day(), 12, 0, 0, 0, time.UTC)
	msgs := []proto.Message{mesgdefFileID(t0)}
	sec := 0
	var total float64
	for _, sg := range segs {
		for i := 0; i < sg.secs; i++ {
			f := float64(i) / float64(max(sg.secs-1, 1))
			r := mesgdef.NewRecord(nil).
				SetTimestamp(t0.Add(time.Duration(sec) * time.Second)).
				SetHeartRate(uint8(math.Round(sg.hr0 + (sg.hr1-sg.hr0)*f))).
				SetSpeed(uint16(math.Round(sg.vel * 1000))).
				SetCadence(85)
			if sg.w0 > 0 || sg.w1 > 0 {
				r.SetPower(uint16(math.Round(sg.w0 + (sg.w1-sg.w0)*f)))
			}
			msgs = append(msgs, r.ToMesg(nil))
			sec++
		}
		dist := sg.vel * float64(sg.secs)
		lap := mesgdef.NewLap(nil).
			SetStartTime(t0.Add(time.Duration(sec-sg.secs) * time.Second)).
			SetTimestamp(t0.Add(time.Duration(sec) * time.Second)).
			SetTotalElapsedTime(uint32(sg.secs * 1000)).
			SetTotalTimerTime(uint32(sg.secs * 1000)).
			SetTotalDistance(uint32(dist * 100)).
			SetLapTrigger(sg.lapTrigger)
		if sg.step >= 0 {
			lap.SetWktStepIndex(typedef.MessageIndex(sg.step))
		}
		msgs = append(msgs, lap.ToMesg(nil))
		total += dist
	}
	msgs = append(msgs, mesgdef.NewSession(nil).SetSport(sport).SetStartTime(t0).
		SetTimestamp(t0.Add(time.Duration(sec)*time.Second)).
		SetTotalElapsedTime(uint32(sec*1000)).SetTotalTimerTime(uint32(sec*1000)).
		SetTotalDistance(uint32(total*100)).ToMesg(nil))
	return encodeRaw(t, msgs)
}

// An independent statement of the decoupling formula over a window, for
// the tests to check the register against: mean output/HR over each half
// of (lo, hi], one-second samples, all valid.
func refDecoupling(t []int, hr, out []float64, lo, hi float64) float64 {
	mid := lo + (hi-lo)/2
	eff := func(a, b float64) float64 {
		var n, d float64
		for i := 1; i < len(t); i++ {
			ti := float64(t[i])
			if a < ti && ti <= b {
				n += out[i] / hr[i]
				d++
			}
		}
		return n / d
	}
	return (eff(lo, mid)/eff(mid, hi) - 1) * 100
}

func TestEffortStepIsTheLongestActiveStep(t *testing.T) {
	leaf := func(role string, dist float64, secs int) resolvedStep {
		return resolvedStep{Role: role, DistM: dist, Secs: secs}
	}
	cases := []struct {
		name  string
		steps []resolvedStep
		want  int
	}{
		{"time trial", []resolvedStep{leaf("warmup", 3219, 0), leaf("active", 5000, 0), leaf("cooldown", 3219, 0)}, 1},
		{"threshold test", []resolvedStep{leaf("warmup", 0, 900), leaf("active", 0, 1800), leaf("cooldown", 0, 600)}, 1},
		{"longer estimated effort wins", []resolvedStep{leaf("active", 0, 3600), leaf("active", 9656, 0)}, 0},
		{"a 5K outranks a 15-minute active", []resolvedStep{leaf("active", 0, 900), leaf("active", 5000, 0)}, 1},
		{"top-level strides lose to the timed effort", []resolvedStep{leaf("active", 100, 0), leaf("active", 0, 1800)}, 1},
		{"test then hills", []resolvedStep{leaf("active", 9656, 0), {Repeat: 8, Body: []resolvedStep{leaf("active", 0, 8), leaf("recovery", 0, 60)}}}, 0},
		{"no active step", []resolvedStep{leaf("warmup", 0, 600), leaf("cooldown", 0, 600)}, -1},
	}
	for _, c := range cases {
		if got := effortStep(flattenSteps(c.steps)); got != c.want {
			t.Errorf("%s: effort step %d, want %d", c.name, got, c.want)
		}
	}
}

func TestLapWindowSpansTheStepsLaps(t *testing.T) {
	idx := func(i int) *int { return &i }
	d := &activityDetail{Laps: []detailLap{
		{StartS: 0, ElapsedS: 600, DistM: 1500, Step: idx(0)},
		{StartS: 600, ElapsedS: 1000, DistM: 3000, Step: idx(1)},   // the effort, auto-lapped
		{StartS: 1600, ElapsedS: 302.4, DistM: 2000, Step: idx(1)}, // in two by the watch
		{StartS: 1903, ElapsedS: 600, DistM: 1500, Step: idx(2)},
		{StartS: 2503, ElapsedS: 5, DistM: 10},
	}}
	lo, hi, dist, ok := lapWindow(d, 1)
	if !ok || lo != 600 || hi != 1902 || dist != 5000 {
		t.Errorf("window (%d, %d] dist %v ok=%v, want (600, 1902] 5000", lo, hi, dist, ok)
	}
	if _, _, _, ok := lapWindow(d, 7); ok {
		t.Error("a step no lap carries must not produce a window")
	}
}

// The windowed register functions over (lo, hi] of a synthetic stream:
// the whole-file case equals the row's own figures (what the gate pins),
// and a lap's window measures that lap and nothing outside it.
func TestWindowedRegisterFunctions(t *testing.T) {
	date := time.Date(2026, 1, 6, 0, 0, 0, 0, time.UTC)
	data := benchFixture(t, date, typedef.SportRunning, []benchSeg{
		{secs: 600, vel: 2.5, hr0: 120, hr1: 140, w0: 200, w1: 200, step: 0, lapTrigger: typedef.LapTriggerTime},
		{secs: 1800, vel: 3.5, hr0: 160, hr1: 176, w0: 280, w1: 270, step: 1, lapTrigger: typedef.LapTriggerTime},
		{secs: 600, vel: 2.4, hr0: 150, hr1: 135, w0: 190, w1: 190, step: 2, lapTrigger: typedef.LapTriggerTime},
	})
	s, err := decodeActivity(data)
	if err != nil {
		t.Fatal(err)
	}
	m := computeMetrics("x.fit", "2026-01-06", s)
	w, _ := sampleWeights(s.Time)
	hrF := intsToFloats(s.HR)
	hi := float64(s.Time[len(s.Time)-1])

	// Whole file: identical to the row (watts preferred there).
	if got := windowDecoupling(s, w, hrF, intsToFloats(s.Watts), 0, hi); got == nil || m.DecouplingPct == nil || *got != *m.DecouplingPct {
		t.Errorf("whole-file windowDecoupling %v != row decoupling %v", got, m.DecouplingPct)
	}
	// The effort lap alone, against the independent formula.
	lo, hiL := 600.0, 2400.0
	wantPa := refDecoupling(s.Time, hrF, s.Vel, lo, hiL)
	if got := windowDecoupling(s, w, hrF, s.Vel, lo, hiL); got == nil || math.Abs(*got-wantPa) > 1e-9 {
		t.Errorf("Pa:HR over the effort: got %v, want %v", got, wantPa)
	}
	wantPw := refDecoupling(s.Time, hrF, intsToFloats(s.Watts), lo, hiL)
	if got := windowDecoupling(s, w, hrF, intsToFloats(s.Watts), lo, hiL); got == nil || math.Abs(*got-wantPw) > 1e-9 {
		t.Errorf("Pw:HR over the effort: got %v, want %v", got, wantPw)
	}
	if math.Abs(wantPa-wantPw) < 0.5 {
		t.Fatalf("fixture does not separate Pa:HR (%v) from Pw:HR (%v)", wantPa, wantPw)
	}
	// Final 20 min of the effort: HR over (1200, 2400] is the linear ramp's
	// upper two thirds; speed is the constant 3.5.
	var n, d float64
	for i := 1; i < len(s.Time); i++ {
		if ti := float64(s.Time[i]); 1200 < ti && ti <= 2400 {
			n += hrF[i]
			d++
		}
	}
	if got := windowMean(s.Time, w, hrF, hiL-1200, hiL, hrValid); got == nil || math.Abs(*got-n/d) > 1e-9 {
		t.Errorf("final-20 HR: got %v, want %v", got, n/d)
	}
	if got := windowMean(s.Time, w, s.Vel, hiL-1200, hiL, func(float64) bool { return true }); got == nil || math.Abs(*got-3.5) > 0.001 {
		t.Errorf("final-20 velocity: got %v, want 3.5", got)
	}
	// Best 60 s inside the cool-down is the cool-down's flat 190, not the
	// effort's 280 next door.
	if got := windowBest(s.Time, intsToFloats(s.Watts), 2400, 3000, 60); got == nil || math.Abs(*got-190) > 0.01 {
		t.Errorf("windowBest in the cool-down: got %v, want 190", got)
	}
	if got := windowBest(s.Time, intsToFloats(s.Watts), 0, 3000, 60); got == nil || *got < 279 {
		t.Errorf("windowBest over the file: got %v, want the effort's ~280", got)
	}
}

// benchBlock writes a two-week block into dir with one benchmark day of
// each type, dated January 2026 like the embedded example it is built
// from: FTP ride Wed W1, DEC run Thu W1, LT test Tue W2 (pushed workout),
// 5K time trial Sat W2 (pushed workout), goal 21:20.
func benchBlock(t *testing.T, dir string) {
	t.Helper()
	raw, err := os.ReadFile("defaults/blocks/example-base-block.json")
	if err != nil {
		t.Fatal(err)
	}
	var b map[string]any
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	b["id"] = "bench"
	b["goal"] = map[string]any{"event": "5K", "date": "2026-01-18", "target": "21:20"}
	weeks := b["weeks"].([]any)
	w1 := weeks[0].(map[string]any)["days"].([]any)
	w1[2] = map[string]any{"kind": "bike_hard", "label": "FTP ramp test", "tag": "FTP", "time": "40:00"}
	w1[3] = map[string]any{"kind": "easy", "label": "*Decoupling test* — 10 km steady", "tag": "DEC", "dist": "10 km"}
	w2 := weeks[1].(map[string]any)["days"].([]any)
	w2[2] = map[string]any{"kind": "bike_hard", "label": "FTP retest", "tag": "FTP", "time": "40:00"}
	w2[6] = map[string]any{"kind": "quality", "label": "GOAL 5K — RACE", "tag": "RACE", "dist": "11 km"}
	w2[1] = map[string]any{"kind": "quality", "label": "30-MIN LT FIELD TEST", "tag": "LT", "dist": "12 km",
		"steps": []any{
			map[string]any{"role": "warmup", "time": "10:00"},
			map[string]any{"role": "active", "time": "30:00", "hr": []any{160, 185}},
			map[string]any{"role": "cooldown", "time": "10:00"},
		}}
	w2[5] = map[string]any{"kind": "quality", "label": "5K TIME TRIAL", "tag": "TT", "dist": "11 km",
		"steps": []any{
			map[string]any{"role": "warmup", "dist": "3 km"},
			map[string]any{"role": "active", "dist": "5 km", "pace": []any{"4:00/km", "4:30/km"}},
			map[string]any{"role": "cooldown", "dist": "3 km"},
		}}
	out, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "blocks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blocks", "bench.json"), append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	// A tagged day needs its t-<TAG> guide, and a library is wholly the
	// volume's: the embedded one plus the four guides, copied across.
	if err := os.MkdirAll(filepath.Join(dir, "library"), 0o755); err != nil {
		t.Fatal(err)
	}
	copyFile(t, "defaults/library/index.json", filepath.Join(dir, "library", "index.json"))
	graw, err := os.ReadFile("defaults/library/guides.json")
	if err != nil {
		t.Fatal(err)
	}
	var lib map[string]any
	if err := json.Unmarshal(graw, &lib); err != nil {
		t.Fatal(err)
	}
	guides := lib["guides"].(map[string]any)
	for _, tag := range []string{"FTP", "DEC", "LT", "TT", "RACE"} {
		guides["t-"+tag] = map[string]any{"id": "t-" + tag, "title": tag + " test",
			"sections": []any{map[string]any{"label": "Protocol", "text": "As written."}}}
	}
	lraw, err := json.Marshal(lib)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "library", "guides.json"), append(lraw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// benchArchive stores the four days' recordings in the server's archive
// and imports them, returning the expected figures computed independently.
type benchExpect struct {
	ftpW   float64 // week 1 ramp
	ftpW2  float64 // week 2 retest
	paHR   float64
	pwHR   float64
	lthr   float64
	ltVel  float64
	ttS    int
	ttDis  float64
	raceS  int
	raceOK float64 // the race's 5 km stretch, metres
}

func benchArchive(t *testing.T, ts testServer, dir string) benchExpect {
	t.Helper()
	adir := filepath.Join(dir, "activities")
	if err := os.MkdirAll(adir, 0o755); err != nil {
		t.Fatal(err)
	}
	put := func(name string, data []byte) {
		if err := os.WriteFile(filepath.Join(adir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ts.s.metrics.importOne(name, data, time.UTC, nil); err != nil {
			t.Fatalf("import %s: %v", name, err)
		}
	}
	var ex benchExpect
	d := func(day int) time.Time { return time.Date(2026, 1, day, 0, 0, 0, 0, time.UTC) }

	// FTP, Wed 7 Jan: an easy spin, then the ramp 100→300 W over 20 min,
	// as two files. Best 60 s is the ramp's last minute.
	put("2026-01-07-07-00-00.fit", benchFixture(t, d(7), typedef.SportCycling, []benchSeg{
		{secs: 900, vel: 8, hr0: 110, hr1: 120, w0: 120, w1: 120, step: -1, lapTrigger: typedef.LapTriggerManual}}))
	ramp := benchFixture(t, d(7), typedef.SportCycling, []benchSeg{
		{secs: 1200, vel: 8, hr0: 120, hr1: 185, w0: 100, w1: 300, step: -1, lapTrigger: typedef.LapTriggerManual}})
	put("2026-01-07-07-30-00.fit", ramp)
	{
		s, err := decodeActivity(ramp)
		if err != nil {
			t.Fatal(err)
		}
		ex.ftpW = 0.75 * *bestRolling(s.Time, intsToFloats(s.Watts), 60)
	}

	// DEC, Thu 8 Jan: the steady 10 km with power, and a short strides
	// file after it that must not be picked (shorter).
	dec := benchFixture(t, d(8), typedef.SportRunning, []benchSeg{
		{secs: 3000, vel: 3.33, hr0: 150, hr1: 165, w0: 262, w1: 248, step: -1, lapTrigger: typedef.LapTriggerDistance}})
	put("2026-01-08-07-00-00.fit", dec)
	put("2026-01-08-08-00-00.fit", benchFixture(t, d(8), typedef.SportRunning, []benchSeg{
		{secs: 400, vel: 4.5, hr0: 150, hr1: 170, step: -1, lapTrigger: typedef.LapTriggerManual}}))
	{
		s, err := decodeActivity(dec)
		if err != nil {
			t.Fatal(err)
		}
		hi := float64(s.Time[len(s.Time)-1])
		ex.paHR = refDecoupling(s.Time, intsToFloats(s.HR), s.Vel, 0, hi)
		ex.pwHR = refDecoupling(s.Time, intsToFloats(s.HR), intsToFloats(s.Watts), 0, hi)
	}

	// LT, Tue 13 Jan: one file, the pushed workout's three laps. LTHR is
	// the effort's final 20 min: HR ramps 165→181 over the 30 min, so the
	// last two thirds average above the whole effort's mean.
	lt := benchFixture(t, d(13), typedef.SportRunning, []benchSeg{
		{secs: 600, vel: 2.6, hr0: 120, hr1: 140, step: 0, lapTrigger: typedef.LapTriggerTime},
		{secs: 1800, vel: 3.6, hr0: 165, hr1: 181, step: 1, lapTrigger: typedef.LapTriggerTime},
		{secs: 600, vel: 2.5, hr0: 150, hr1: 130, step: 2, lapTrigger: typedef.LapTriggerTime}})
	put("2026-01-13-07-00-00.fit", lt)
	{
		s, err := decodeActivity(lt)
		if err != nil {
			t.Fatal(err)
		}
		var n, c, v float64
		for i := 1; i < len(s.Time); i++ {
			if ti := s.Time[i]; 1200 < ti && ti <= 2400 {
				n += float64(s.HR[i])
				v += s.Vel[i]
				c++
			}
		}
		ex.lthr, ex.ltVel = n/c, v/c
	}

	// TT, Sat 17 Jan: first a false start — the workout begun, six minutes
	// of the 5K step run fast, then saved — and then the real thing: warm-up,
	// the 5K, cool-down, one file with step laps. The false start's lap
	// carries the effort's index but delivered 1.5 km of 5, so it is not
	// the effort; the 5K is 1300 s at 3.846 m/s and its lap's elapsed time
	// is the number.
	put("2026-01-17-07-30-00.fit", benchFixture(t, d(17), typedef.SportRunning, []benchSeg{
		{secs: 300, vel: 3.0, hr0: 125, hr1: 140, step: 0, lapTrigger: typedef.LapTriggerDistance},
		{secs: 360, vel: 4.2, hr0: 160, hr1: 178, step: 1, lapTrigger: typedef.LapTriggerManual}}))
	put("2026-01-17-08-00-00.fit", benchFixture(t, d(17), typedef.SportRunning, []benchSeg{
		{secs: 900, vel: 3.0, hr0: 125, hr1: 145, step: 0, lapTrigger: typedef.LapTriggerDistance},
		{secs: 1300, vel: 5000.0 / 1300, hr0: 170, hr1: 184, step: 1, lapTrigger: typedef.LapTriggerDistance},
		{secs: 900, vel: 2.8, hr0: 150, hr1: 130, step: 2, lapTrigger: typedef.LapTriggerDistance}}))
	ex.ttS, ex.ttDis = 1300, 5000

	// FTP retest, Wed 14 Jan: a steeper ramp, one file.
	ramp2 := benchFixture(t, d(14), typedef.SportCycling, []benchSeg{
		{secs: 1200, vel: 8, hr0: 120, hr1: 186, w0: 100, w1: 320, step: -1, lapTrigger: typedef.LapTriggerManual}})
	put("2026-01-14-07-00-00.fit", ramp2)
	{
		s, err := decodeActivity(ramp2)
		if err != nil {
			t.Fatal(err)
		}
		ex.ftpW2 = 0.75 * *bestRolling(s.Time, intsToFloats(s.Watts), 60)
	}

	// RACE, Sun 18 Jan: run in plain run mode, one file, mile auto-laps
	// with no step index — warm-up, the race at 3.968 m/s (21:00 for 5 km),
	// cool-down. The number is the fastest 5 km stretch, not the file.
	put("2026-01-18-09-00-00.fit", benchFixture(t, d(18), typedef.SportRunning, []benchSeg{
		{secs: 600, vel: 3.0, hr0: 125, hr1: 145, step: -1, lapTrigger: typedef.LapTriggerDistance},
		{secs: 1260, vel: 5000.0 / 1260, hr0: 172, hr1: 186, step: -1, lapTrigger: typedef.LapTriggerDistance},
		{secs: 600, vel: 2.8, hr0: 150, hr1: 130, step: -1, lapTrigger: typedef.LapTriggerDistance}}))
	ex.raceS, ex.raceOK = 1260, 5000
	return ex
}

func benchServer(t *testing.T) (testServer, string, benchExpect) {
	t.Helper()
	dir := t.TempDir()
	benchBlock(t, dir)
	ts := fitTestMuxServer(t, dir)
	ex := benchArchive(t, ts, dir)
	return ts, dir, ex
}

// Every type measures the window the decisions name, from the right file,
// and the rows are what the table holds.
func TestBenchmarksMeasureTheEffortWindow(t *testing.T) {
	ts, _, ex := benchServer(t)
	d := ts.s.ds()
	blk := d.Blocks[0]
	pts := ts.s.benchmarks(d, blk, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
	by := map[string]benchmarkPoint{}
	for _, p := range pts {
		if _, dup := by[p.Tag]; !dup {
			by[p.Tag] = p // the first of a tag: week 1's FTP
		}
	}
	if len(pts) != 6 || len(by) != 5 {
		t.Fatalf("%d benchmark points over %d tags, want 6 over 5 (FTP×2, DEC, LT, TT, RACE): %+v", len(pts), len(by), pts)
	}
	near := func(what string, got *float64, want, tol float64) {
		t.Helper()
		if got == nil || math.Abs(*got-want) > tol {
			t.Errorf("%s: got %v, want %v", what, got, want)
		}
	}
	ftp := by["FTP"]
	if ftp.Row.Name != "2026-01-07-07-30-00.fit" {
		t.Errorf("FTP measured %s, want the ramp file", ftp.Row.Name)
	}
	near("FTP watts", ftp.Row.FTPW, ex.ftpW, 1e-9)
	near("FTP retest watts", pts[len(pts)-3].Row.FTPW, ex.ftpW2, 1e-9) // Wed W2, between LT and TT

	dec := by["DEC"]
	if dec.Row.Name != "2026-01-08-07-00-00.fit" || dec.Row.Lo != 0 {
		t.Errorf("DEC measured %s from %d, want the long run whole", dec.Row.Name, dec.Row.Lo)
	}
	near("DEC Pa:HR", dec.Row.PaHR, ex.paHR, 1e-9)
	near("DEC Pw:HR", dec.Row.PwHR, ex.pwHR, 1e-9)

	lt := by["LT"]
	if lt.Row.Lo != 600 || lt.Row.Hi != 2400 {
		t.Errorf("LT window (%d, %d], want the effort lap (600, 2400]", lt.Row.Lo, lt.Row.Hi)
	}
	near("LTHR", lt.Row.LTHR, ex.lthr, 1e-9)
	near("LT velocity", lt.Row.LTVel, ex.ltVel, 1e-9)

	tt := by["TT"]
	if tt.Row.Name != "2026-01-17-08-00-00.fit" {
		t.Errorf("TT measured %s, want the completed attempt, not the false start that carried the step", tt.Row.Name)
	}
	if tt.Row.TTS == nil || *tt.Row.TTS != ex.ttS {
		t.Errorf("TT seconds %v, want %d — the 5K lap, not the file", tt.Row.TTS, ex.ttS)
	}
	near("TT distance", tt.Row.TTDistM, ex.ttDis, 1)

	race := by["RACE"]
	// The stretch search walks integer-second samples, so the 5 km lands
	// a second either side of the 1260 s it was run in.
	if race.Row.TTS == nil || math.Abs(float64(*race.Row.TTS-ex.raceS)) > 2 {
		t.Errorf("RACE seconds %v, want %d±2 — the fastest 5 km stretch of a plain-mode file, not its %d s", deref(race.Row.TTS), ex.raceS, 2460)
	}
	near("RACE distance", race.Row.TTDistM, ex.raceOK, 5)
	if race.Row.Lo == 0 || race.Row.Hi == 2460 {
		t.Errorf("RACE window (%d, %d] is the whole file", race.Row.Lo, race.Row.Hi)
	}

	// The rows are in the table, stamped, and a second call reads them
	// back rather than recomputing: delete the files and ask again.
	for _, p := range pts {
		row, err := ts.s.metrics.benchmarkGet(blk.ID, p.Date)
		if err != nil || row == nil || row.Version != benchmarkVersion {
			t.Errorf("%s: stored row %v err %v", p.Date, row, err)
		}
	}
	if err := os.RemoveAll(ts.s.activitiesDir()); err != nil {
		t.Fatal(err)
	}
	again := ts.s.benchmarks(d, blk, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
	if len(again) != 6 {
		t.Errorf("cached rows not served without the files: %d points", len(again))
	}
}

// A day whose recordings change is measured again; one whose arithmetic
// version moved is too. Nothing else touches a stored row.
func TestBenchmarkRowsRecomputeOnlyWhenTheDayChanges(t *testing.T) {
	ts, dir, _ := benchServer(t)
	d := ts.s.ds()
	blk := d.Blocks[0]
	today := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	ts.s.benchmarks(d, blk, today)
	row, _ := ts.s.metrics.benchmarkGet(blk.ID, "2026-01-17")
	if row == nil {
		t.Fatal("no TT row")
	}
	// Plant a fake figure: a later read must keep it, because nothing changed.
	v := 999.0
	row.FTPW = &v
	if err := ts.s.metrics.benchmarkPut(row); err != nil {
		t.Fatal(err)
	}
	ts.s.benchmarks(d, blk, today)
	row, _ = ts.s.metrics.benchmarkGet(blk.ID, "2026-01-17")
	if row.FTPW == nil || *row.FTPW != 999 {
		t.Error("an unchanged day was recomputed")
	}
	// A new recording on the day: recomputed, the plant gone.
	name := "2026-01-17-18-00-00.fit"
	data := benchFixture(t, time.Date(2026, 1, 17, 0, 0, 0, 0, time.UTC), typedef.SportRunning, []benchSeg{
		{secs: 700, vel: 2.5, hr0: 120, hr1: 130, step: -1, lapTrigger: typedef.LapTriggerManual}})
	if err := os.WriteFile(filepath.Join(dir, "activities", name), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.s.metrics.importOne(name, data, time.UTC, nil); err != nil {
		t.Fatal(err)
	}
	ts.s.benchmarks(d, blk, today)
	row, _ = ts.s.metrics.benchmarkGet(blk.ID, "2026-01-17")
	if row.FTPW != nil || row.TTS == nil || *row.TTS != 1300 {
		t.Errorf("after a new file the day should be re-measured: ftp %v tt %v", row.FTPW, row.TTS)
	}
	if !strings.Contains(row.Names, name) {
		t.Errorf("the row's names %q do not include the new file", row.Names)
	}
	// A row from an older arithmetic version is re-measured too.
	row.Version = benchmarkVersion - 1
	row.FTPW = &v
	if err := ts.s.metrics.benchmarkPut(row); err != nil {
		t.Fatal(err)
	}
	ts.s.benchmarks(d, blk, today)
	row, _ = ts.s.metrics.benchmarkGet(blk.ID, "2026-01-17")
	if row.Version != benchmarkVersion || row.FTPW != nil {
		t.Errorf("an old-version row was served as is: version %d ftp %v", row.Version, row.FTPW)
	}
}

// A day whose recordings cannot be measured is remembered as such — an
// empty-named row under the day's key — so it is not read and decoded on
// every render, and it yields no point. A new recording on the day lifts it.
func TestBenchmarkDayThatCannotBeMeasuredIsRememberedNotRetried(t *testing.T) {
	ts, dir, _ := benchServer(t)
	d := ts.s.ds()
	blk := d.Blocks[0]
	today := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	// Replace the DEC day's recordings with one two-minute jog: too short
	// to be the test.
	for _, n := range []string{"2026-01-08-07-00-00.fit", "2026-01-08-08-00-00.fit"} {
		if err := os.Remove(filepath.Join(dir, "activities", n)); err != nil {
			t.Fatal(err)
		}
		if _, err := ts.s.metrics.w.Exec(`DELETE FROM activities WHERE name = ?`, n); err != nil {
			t.Fatal(err)
		}
	}
	short := "2026-01-08-09-00-00.fit"
	data := benchFixture(t, time.Date(2026, 1, 8, 0, 0, 0, 0, time.UTC), typedef.SportRunning, []benchSeg{
		{secs: 120, vel: 2.5, hr0: 120, hr1: 130, step: -1, lapTrigger: typedef.LapTriggerManual}})
	if err := os.WriteFile(filepath.Join(dir, "activities", short), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.s.metrics.importOne(short, data, time.UTC, nil); err != nil {
		t.Fatal(err)
	}
	pts := ts.s.benchmarks(d, blk, today)
	for _, p := range pts {
		if p.Tag == "DEC" {
			t.Fatalf("a two-minute jog was measured as the decoupling test: %+v", p.Row)
		}
	}
	row, err := ts.s.metrics.benchmarkGet(blk.ID, "2026-01-08")
	if err != nil || row == nil || row.Name != "" || !strings.Contains(row.Names, short) {
		t.Fatalf("the unmeasurable day is not remembered: row %+v err %v", row, err)
	}
	// Remove the file: a cached negative must not touch the disk.
	if err := os.Remove(filepath.Join(dir, "activities", short)); err != nil {
		t.Fatal(err)
	}
	ts.s.benchmarks(d, blk, today)
	row2, _ := ts.s.metrics.benchmarkGet(blk.ID, "2026-01-08")
	if row2 == nil || row2.Name != "" || row2.Names != row.Names {
		t.Errorf("the negative row was disturbed: %+v", row2)
	}
}

// Inside a window the register's weighting holds: a sample across a
// recording gap carries nothing, and a two-second cadence weights each
// sample by its two seconds. The final-20 HR over an effort with a stop in
// it is the mean of the recorded minutes only.
func TestWindowMeanHonoursTheGapRule(t *testing.T) {
	t0 := time.Date(2026, 1, 6, 12, 0, 0, 0, time.UTC)
	var msgs []proto.Message
	msgs = append(msgs, mesgdefFileID(t0))
	// 0..599 at HR 150 every second; a 300 s hole; 900..1499 at HR 170
	// every second; then 1500..1798 at HR 190 every TWO seconds.
	for sec := 0; sec < 600; sec++ {
		msgs = append(msgs, recordAt(t0, sec, 150, 3000, 85))
	}
	for sec := 900; sec < 1500; sec++ {
		msgs = append(msgs, recordAt(t0, sec, 170, 3000, 85))
	}
	for sec := 1500; sec < 1800; sec += 2 {
		msgs = append(msgs, recordAt(t0, sec, 190, 3000, 85))
	}
	msgs = append(msgs, sessionMsg(typedef.SportRunning, 540_000))
	s, err := decodeActivity(encodeRaw(t, msgs))
	if err != nil {
		t.Fatal(err)
	}
	w, _ := sampleWeights(s.Time)
	hrF := intsToFloats(s.HR)
	hi := float64(s.Time[len(s.Time)-1])
	// Final 20 min: (598, 1798]. Recorded inside it: the sample at 599
	// (150, one second); the resume sample at 900 spans the gap (weight
	// 0); 901..1499 at 170 (599 s); 1500 at 190 (its interval is the one
	// second from 1499); 1502..1798 at 190, 149 samples of two seconds.
	want := (1*150.0 + 599*170 + 1*190 + 149*2*190) / (1 + 599 + 1 + 298)
	got := windowMean(s.Time, w, hrF, hi-1200, hi, hrValid)
	if got == nil || math.Abs(*got-want) > 1e-9 {
		t.Errorf("final-20 HR across a gap: got %v, want %v", deref(got), want)
	}
	// Counting samples instead of seconds, or crediting the gap, gives
	// other numbers; the test is only a test if they differ.
	naive := (600*170.0 + 150*190) / 750
	if math.Abs(naive-want) < 0.5 {
		t.Fatalf("fixture does not separate weighted (%v) from counted (%v)", want, naive)
	}
}

// /trends and the blocks card say the same thing from the same rows.
func TestTrendsPageAndBlocksCardAgree(t *testing.T) {
	ts, _, ex := benchServer(t)
	get := func(path string) string {
		rec := httptest.NewRecorder()
		ts.mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	trends := get("/trends")
	sums := regexp.MustCompile(`<h2>([^<]*)</h2>\s*<span class="bench-sum">([^<]*)</span>`).FindAllStringSubmatch(trends, -1)
	// Four benchmark panels, plus the two best-effort panels every block
	// with runs carries.
	if len(sums) != 6 {
		t.Fatalf("%d panels on /trends, want 6:\n%s", len(sums), trends)
	}
	want := map[string]string{
		"FTP": fmt.Sprintf("%.0f → %.0f W (+%.0f)", pyRound(ex.ftpW, 0), pyRound(ex.ftpW2, 0), pyRound(ex.ftpW2, 0)-pyRound(ex.ftpW, 0)),
	}
	// The race's 5 km stretch is found over integer-second samples, so it
	// reads 21:00 or 21:01 — either is the race, neither is the file's 41:00.
	ttRe := regexp.MustCompile(`^21:40 → 21:0[01] · 0:(20|19) under 21:20$`)
	got := map[string]string{}
	for _, m := range sums {
		got[m[1]] = html.UnescapeString(m[2])
	}
	if got["FTP"] != want["FTP"] {
		t.Errorf("FTP summary %q, want %q", got["FTP"], want["FTP"])
	}
	if !ttRe.MatchString(got["5K time trial"]) {
		t.Errorf("TT summary %q, want %s", got["5K time trial"], ttRe)
	}
	if !strings.HasPrefix(got["Decoupling"], "Pa:HR ") || !strings.Contains(got["Decoupling"], " · Pw:HR ") {
		t.Errorf("Decoupling summary %q should carry both series", got["Decoupling"])
	}
	if want := " bpm · " + Pace(1/ex.ltVel).In(Metric); !strings.HasSuffix(got["Lactate threshold"], want) {
		t.Errorf("LT summary %q should end with the pace over the final 20 min (%q)", got["Lactate threshold"], want)
	}
	if n := strings.Count(trends, "<polyline"); n != 4 {
		t.Errorf("%d polylines, want 4: the two-point FTP series, the TT→RACE series, and the two best-effort panels", n)
	}
	// The RACE is the last point of the time-trial series, and the goal
	// label sits at the left so the race's own label at the right is clear.
	if !regexp.MustCompile(`W2 · 2026-01-18</td><td><strong>21:0[01]</strong>`).MatchString(trends) {
		t.Error("the race is not the last measurement of the time-trial table")
	}
	if !strings.Contains(trends, `class="bench-goal"`) || !strings.Contains(trends, "GOAL 21:20") {
		t.Error("the TT panel should draw the goal line")
	}
	if n := strings.Count(trends, `class="bench-key `); n != 2 {
		t.Errorf("%d legend keys, want 2 (the decoupling panel's two series; single-series panels carry none)", n)
	}

	blocks := get("/blocks")
	card := regexp.MustCompile(`(?s)<p class="block-bench">(.*?)</p>`).FindStringSubmatch(blocks)
	if card == nil {
		t.Fatalf("no benchmark card on /blocks:\n%s", blocks)
	}
	// The card carries the benchmark lines; the best-effort panels are
	// /trends' own.
	for _, title := range []string{"FTP", "Decoupling", "Lactate threshold", "5K time trial"} {
		if !strings.Contains(html.UnescapeString(card[1]), title+" "+got[title]) {
			t.Errorf("blocks card lacks %q: %s", title+" "+got[title], card[1])
		}
	}
	if !strings.Contains(blocks, `href="/trends"`) {
		t.Error("the blocks card should link to /trends")
	}
	if !strings.Contains(trends, `href="/trends"`) {
		t.Error("the nav should carry Trends when the block tags benchmarks")
	}
}

// Without a metrics cache the page still renders, says nothing measured,
// and lists the benchmark days ahead; an untagged block says so.
func TestTrendsWithoutMeasurements(t *testing.T) {
	dir := t.TempDir()
	benchBlock(t, dir)
	ts := fitTestMuxServer(t, dir)
	ts.s.metrics = nil
	rec := httptest.NewRecorder()
	ts.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/trends", nil))
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "bench-sum") {
		t.Errorf("trends without metrics: %d, panels=%v", rec.Code, strings.Contains(rec.Body.String(), "bench-sum"))
	}
	plain := t.TempDir() // the embedded example: no tags
	ts2 := fitTestMuxServer(t, plain)
	rec = httptest.NewRecorder()
	ts2.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/trends", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "tags no benchmark days") {
		t.Errorf("untagged block: %d, body lacks the explanation", rec.Code)
	}
	rec = httptest.NewRecorder()
	ts2.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if strings.Contains(rec.Body.String(), `href="/trends"`) {
		t.Error("the nav must not offer Trends for a block that tags nothing")
	}
}

// Ticks are round numbers a reader can compare against, three to five of
// them inside the range; a time axis steps in clock-friendly units.
func TestNiceTicks(t *testing.T) {
	cases := []struct {
		lo, hi float64
		clock  bool
		want   []float64
	}{
		{1.1, 9.7, false, []float64{2, 4, 6, 8}},
		{205.5, 226.3, false, []float64{210, 215, 220, 225}},
		{0.4, 2.6, false, []float64{0.5, 1, 1.5, 2, 2.5}},
		{1240, 1360, true, []float64{1260, 1290, 1320, 1350}}, // 21:00, 21:30, 22:00, 22:30
		{213.8, 216.2, false, []float64{214, 214.5, 215, 215.5, 216}},
	}
	for _, c := range cases {
		got := niceTicks(c.lo, c.hi, c.clock)
		if len(got) != len(c.want) {
			t.Errorf("niceTicks(%v, %v, %v) = %v, want %v", c.lo, c.hi, c.clock, got, c.want)
			continue
		}
		for i := range got {
			if math.Abs(got[i]-c.want[i]) > 1e-9 {
				t.Errorf("niceTicks(%v, %v, %v) = %v, want %v", c.lo, c.hi, c.clock, got, c.want)
				break
			}
		}
		if len(got) < 2 || len(got) > 5 {
			t.Errorf("niceTicks(%v, %v): %d ticks, want 2–5", c.lo, c.hi, len(got))
		}
	}
}

func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}
