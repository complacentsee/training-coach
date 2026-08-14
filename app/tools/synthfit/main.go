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

	secs := *mins * 60
	speed := *km * 1000 / float64(secs) // m/s, held flat
	msgs := []proto.Message{
		mesgdef.NewFileId(nil).
			SetType(typedef.FileActivity).
			SetManufacturer(typedef.ManufacturerDevelopment).
			SetTimeCreated(t0).ToMesg(nil),
	}
	for i := 0; i <= secs; i++ {
		frac := float64(i) / float64(secs)
		bpm := uint8(math.Round(lo + (hi-lo)*frac))
		r := mesgdef.NewRecord(nil).
			SetTimestamp(t0.Add(time.Duration(i) * time.Second)).
			SetHeartRate(bpm).
			SetCadence(uint8(*cadence)).
			SetEnhancedSpeed(uint32(math.Round(speed * 1000)))
		if *watts > 0 {
			r.SetPower(uint16(*watts))
		}
		msgs = append(msgs, r.ToMesg(nil))
	}
	msgs = append(msgs, mesgdef.NewSession(nil).
		SetSport(sp).
		SetStartTime(t0).
		SetTimestamp(t0.Add(time.Duration(secs)*time.Second)).
		SetTotalDistance(uint32(math.Round(*km*1000*100))).ToMesg(nil))

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
	fmt.Printf("%s: %s, %.2f km in %d min, HR %.0f→%.0f (%d records, %d bytes)\n",
		*out, *sport, *km, *mins, lo, hi, secs+1, fi.Size())
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
