package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestSweepComingUp is a viewer, not an assertion: it prints the card for every
// day of the block so a rule about *when* rows appear can be read rather than
// reasoned about. Run it with -v when changing comingUp.
//
//	go test -run TestSweepComingUp -v .
func TestSweepComingUp(t *testing.T) {
	if os.Getenv("SWEEP") == "" {
		t.Skip("set SWEEP=1 to print the whole block")
	}
	d := liveData(t)
	b := d.Blocks[0]
	guides, err := b.ResolveGuides(d.Athlete, 1)
	if err != nil {
		t.Fatal(err)
	}
	dow := [7]string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}
	for wi := 0; wi < b.WeekCount(); wi++ {
		for di := 0; di < 7; di++ {
			day := b.DayOf(wi, di)
			rows := comingUp(b, guides, day)
			var parts []string
			for _, r := range rows {
				tag := ""
				if r.IsBench {
					tag = "BENCH "
				}
				parts = append(parts, fmt.Sprintf("+%d %s%s", r.In, tag, trunc(r.Label, 34)))
			}
			line := strings.Join(parts, "  |  ")
			if line == "" {
				line = "(nothing)"
			}
			fmt.Printf("  W%-2d %s %s   %s\n", wi+1, dow[di], day.Format("2 Jan"), line)
		}
	}
}

// TestSweepFitSteps is the eyeball tool gating every steps batch: per steps
// day it prints the on-watch name and its first-15 dedupe key, every unrolled
// FIT step in raw wire values, and the measured total against the session —
// the reviewer reads it beside the day's detail before anything is pushed.
//
//	SWEEP=1 go test -run TestSweepFitSteps -v .
func TestSweepFitSteps(t *testing.T) {
	if os.Getenv("SWEEP") == "" {
		t.Skip("set SWEEP=1 to print every steps day")
	}
	roleOf := map[uint8]string{}
	for role, in := range fitIntensities {
		roleOf[in] = role
	}
	durName := map[uint8]string{
		fitDurationTime: "time", fitDurationDistance: "dist",
		fitDurationOpen: "open", fitDurationRepeat: "repeat",
	}
	tgtName := map[uint8]string{
		fitTargetSpeed: "speed", fitTargetHR: "hr", fitTargetOpen: "open",
		fitTargetPower: "power",
	}
	for _, dir := range []string{"./data", ""} {
		label := dir
		if dir == "" {
			label = "(embedded defaults)"
		}
		d, err := loadDataset(dir, chicago(t))
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range d.Blocks {
			for wi, w := range b.Weeks {
				for di, sess := range w.Days {
					if len(sess.Steps) == 0 {
						continue
					}
					c := b.ctxFor(d.Athlete, w.N)
					sc := *c
					sc.Session = &sess
					sc.InBlock = true
					rs, err := resolveSteps(&sc, sess)
					if err != nil {
						t.Fatal(err)
					}
					name, err := fitName(w.N, di, sess.Label)
					if err != nil {
						t.Fatal(err)
					}
					key := name
					if len(key) > fitNamePrefixLen {
						key = key[:fitNamePrefixLen]
					}
					fmt.Printf("\n%s  %s  %s\n", label, b.ID, b.DayOf(wi, di).Format("2006-01-02"))
					fmt.Printf("  %q  key %q\n", name, key)
					serial, created := fitIdentity(d.Rev, b.ID, b.DayOf(wi, di))
					for i, st := range fitWorkoutFor(name, rs, fitSportFor(sess.Kind), serial, created).Steps {
						tgt := tgtName[st.TargetType]
						if st.CustomLow != fitInvalidUint32 {
							tgt = fmt.Sprintf("%s %d–%d", tgt, st.CustomLow, st.CustomHigh)
						}
						note := ""
						if st.Notes != "" {
							note = fmt.Sprintf("  note %q", st.Notes)
						}
						fmt.Printf("  %2d  %-8s %-6s %8d  target %-14s val %d%s\n",
							i, roleOf[st.Intensity], durName[st.DurationType], st.DurationValue, tgt, st.TargetValue, note)
					}
					if sess.Kind.IsBike() {
						fmt.Printf("  runs %s of %d:00 prescribed\n", clock(stepsSeconds(rs)), sess.Mins)
					} else {
						fmt.Printf("  measures %s of %s prescribed\n",
							Distance(stepsDistance(rs)).In(d.Athlete.Units), sess.Dist.In(d.Athlete.Units))
					}
				}
			}
		}
	}
}
