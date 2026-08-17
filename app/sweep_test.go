package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestSweepIngestGate is the gate on turning the ingest checks on: run
// fitFramingErr and wholeFileFITCRCOK over every file in a real archive and
// demand that every one passes. A refusal here is a finding about the
// ARCHIVE, not a bug in the check — these bytes are already stored, already
// measured and already graded against, so a file that fails is a file the
// register has been reading past a framing error all along, and that is
// something to report before anything starts refusing uploads.
//
// It needs an archive, which is not in the repo (personal health data), so it
// activates only when RC_ARCHIVE names a directory of .fit files.
//
//	RC_ARCHIVE=/path/to/activities go test -run TestSweepIngestGate -v .
func TestSweepIngestGate(t *testing.T) {
	dir := os.Getenv("RC_ARCHIVE")
	if dir == "" {
		t.Skip("RC_ARCHIVE not set — the whole-archive ingest sweep runs only where an archive lives")
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	var names []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".fit") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatalf("archive %s holds no .fit files", dir)
	}

	// Shape counts, so the sweep says what the archive IS and not only that it
	// passed: how many parts a file is chained from, which header size it
	// carries, and which of the two CRC conventions its trailing bytes match.
	// The last one is the measured Zwift wart, counted rather than assumed.
	parts, headers := map[int]int{}, map[int]int{}
	dataOnlyCRC, wholePartCRC, bothCRC := 0, 0, 0
	framingBad, crcBad := 0, 0

	for _, n := range names {
		body, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		if err := fitFramingErr(body); err != nil {
			t.Errorf("FRAMING %s (%d bytes): %v", n, len(body), err)
			framingBad++
			continue
		}
		if !wholeFileFITCRCOK(body) {
			t.Errorf("CRC %s (%d bytes): neither the data-only nor the whole-part convention matches", n, len(body))
			crcBad++
		}
		np, hs, dataOnly, wholePart := fitShape(body)
		parts[np]++
		headers[hs]++
		switch {
		case dataOnly && wholePart:
			bothCRC++
		case dataOnly:
			dataOnlyCRC++
		case wholePart:
			wholePartCRC++
		}
	}

	t.Logf("ingest sweep: %d files, %d framing refusals, %d CRC refusals", len(names), framingBad, crcBad)
	t.Logf("  parts per file: %s", countsLine(parts))
	t.Logf("  header size:    %s", countsLine(headers))
	t.Logf("  trailing CRC:   %d data-only, %d whole-part (the Zwift convention), %d satisfy both",
		dataOnlyCRC, wholePartCRC, bothCRC)
}

// fitShape reports a well-framed file's part count, its FIRST part's header
// size, and which CRC conventions its LAST part satisfies. Only the sweep
// needs this; nothing in the serving path asks a file to describe itself.
func fitShape(body []byte) (parts, headerSize int, dataOnly, wholePart bool) {
	for o := 0; o < len(body); parts++ {
		hs := int(body[o])
		if parts == 0 {
			headerSize = hs
		}
		dsize := int(binary.LittleEndian.Uint32(body[o+4 : o+8]))
		end := o + hs + dsize + 2
		trailing := binary.LittleEndian.Uint16(body[end-2 : end])
		dataOnly = fitCRC16(0, body[o+hs:end-2]) == trailing
		wholePart = fitCRC16(0, body[o:end-2]) == trailing
		o = end
	}
	return parts, headerSize, dataOnly, wholePart
}

// countsLine renders a small histogram in ascending key order.
func countsLine(m map[int]int) string {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	var out []string
	for _, k := range keys {
		out = append(out, fmt.Sprintf("%d×%d", m[k], k))
	}
	return strings.Join(out, ", ")
}

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
