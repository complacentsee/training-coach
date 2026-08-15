// Command synthfit writes a synthetic activity: a session ridden or run
// exactly to a description, for the fixtures no real archive contains.
//
//	go run ./tools/synthfit -out testdata/cases/x.fit \
//	    -km 13 -mins 70 -hr 138:146 -cadence 86 -start 2026-01-10T07:00:00Z
//
// The negative fixtures are real recordings, because real sessions fail in
// ways worth grading against. The positive control is generated, because
// "held every minute under the cap for the whole prescribed distance" is
// what an athlete is working toward rather than what their history holds —
// and a control has to be unambiguous.
//
// Nothing here is presented as a recording of anyone: manufacturer is
// development, there is no position data of any kind, and the HR walks
// linearly between the two ends given. What it produces is a session whose
// measured numbers are known in advance, which is the only property a
// control needs.
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/muktihari/fit/encoder"
	"github.com/muktihari/fit/profile/mesgdef"
	"github.com/muktihari/fit/profile/typedef"
	"github.com/muktihari/fit/proto"
)

func main() {
	out := flag.String("out", "", "fixture to write")
	km := flag.Float64("km", 13, "distance in kilometres")
	mins := flag.Int("mins", 70, "elapsed minutes")
	hr := flag.String("hr", "138:146", "heart rate, start:end in bpm")
	cadence := flag.Int("cadence", 86, "cadence as recorded (per-leg for a run)")
	watts := flag.Int("watts", 0, "power in watts, 0 for none")
	sport := flag.String("sport", "running", "running or cycling")
	start := flag.String("start", "2026-01-10T07:00:00Z", "start time, RFC3339")
	// An interval session instead of a steady one: warm-up, then reps with
	// jog recoveries, then a cool-down. The athlete does not lap his
	// repetitions, so a fixture must carry them the way a real recording
	// does — buried in the trace, to be found again by measurement.
	reps := flag.Int("reps", 0, "number of repetitions; 0 means a steady session")
	repSecs := flag.Int("rep-secs", 240, "seconds per repetition")
	repMPS := flag.Float64("rep-mps", 3.70, "repetition speed in m/s")
	jogSecs := flag.Int("jog-secs", 120, "seconds of jog recovery between reps")
	easyMPS := flag.Float64("easy-mps", 2.60, "warm-up, jog and cool-down speed in m/s")
	warmSecs := flag.Int("warm-secs", 770, "warm-up seconds")
	coolSecs := flag.Int("cool-secs", 770, "cool-down seconds")
	flag.Parse()
	if *out == "" {
		fmt.Fprintln(os.Stderr, "usage: synthfit -out fixture.fit [flags]")
		os.Exit(2)
	}

	lo, hi, err := parsePair(*hr)
	if err != nil {
		fail(err)
	}
	t0, err := time.Parse(time.RFC3339, *start)
	if err != nil {
		fail(err)
	}
	sp := typedef.SportRunning
	if *sport == "cycling" {
		sp = typedef.SportCycling
	}

	// The session as a list of stretches: (seconds, speed, heart rate).
	type stretch struct {
		secs int
		mps  float64
		bpm  float64
	}
	var plan []stretch
	if *reps > 0 {
		plan = append(plan, stretch{*warmSecs, *easyMPS, lo})
		for i := 0; i < *reps; i++ {
			plan = append(plan, stretch{*repSecs, *repMPS, hi})
			if i < *reps-1 {
				plan = append(plan, stretch{*jogSecs, *easyMPS, lo + (hi-lo)*0.4})
			}
		}
		plan = append(plan, stretch{*coolSecs, *easyMPS, lo})
	} else {
		plan = append(plan, stretch{*mins * 60, *km * 1000 / float64(*mins*60), lo})
	}

	msgs := []proto.Message{
		mesgdef.NewFileId(nil).
			SetType(typedef.FileActivity).
			SetManufacturer(typedef.ManufacturerDevelopment).
			SetTimeCreated(t0).ToMesg(nil),
	}
	sec, dist, bpm := 0, 0.0, lo
	total := 0
	for _, st := range plan {
		total += st.secs
	}
	for _, st := range plan {
		for i := 0; i < st.secs; i++ {
			// Heart rate lags the effort rather than stepping with it, the
			// way a real one does: a rep's cost arrives over its first
			// half-minute and bleeds off through the jog.
			bpm += (st.bpm - bpm) * 0.05
			// A steady session walks its two ends instead, so the flat case
			// still reads as one continuous effort.
			if *reps == 0 {
				bpm = lo + (hi-lo)*float64(sec)/float64(total)
			}
			r := mesgdef.NewRecord(nil).
				SetTimestamp(t0.Add(time.Duration(sec) * time.Second)).
				SetHeartRate(uint8(math.Round(bpm))).
				SetCadence(uint8(*cadence)).
				SetEnhancedSpeed(uint32(math.Round(st.mps * 1000)))
			if *watts > 0 {
				r.SetPower(uint16(*watts))
			}
			msgs = append(msgs, r.ToMesg(nil))
			dist += st.mps
			sec++
		}
	}
	if *reps == 0 {
		dist = *km * 1000
	}
	msgs = append(msgs, mesgdef.NewSession(nil).
		SetSport(sp).
		SetStartTime(t0).
		SetTimestamp(t0.Add(time.Duration(sec)*time.Second)).
		SetTotalDistance(uint32(math.Round(dist*100))).ToMesg(nil))
	secs := sec

	f, err := os.Create(*out)
	if err != nil {
		fail(err)
	}
	if err := encoder.New(f).Encode(&proto.FIT{Messages: msgs}); err != nil {
		f.Close()
		fail(err)
	}
	if err := f.Close(); err != nil {
		fail(err)
	}
	fi, _ := os.Stat(*out)
	fmt.Printf("%s: %s, %.2f km in %d:%02d, HR %.0f-%.0f (%d records, %d bytes)\n",
		*out, *sport, dist/1000, secs/60, secs%60, lo, hi, secs, fi.Size())
}

func parsePair(s string) (float64, float64, error) {
	a, b, ok := strings.Cut(s, ":")
	if !ok {
		return 0, 0, fmt.Errorf("want start:end, got %q", s)
	}
	lo, err := strconv.ParseFloat(a, 64)
	if err != nil {
		return 0, 0, err
	}
	hi, err := strconv.ParseFloat(b, 64)
	return lo, hi, err
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "synthfit:", err)
	os.Exit(1)
}
