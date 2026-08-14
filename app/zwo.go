package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

/*
zwo.go writes Zwift custom workout files (.zwo) from the same resolved steps
the FIT encoder consumes — one source of truth, two dialects, so the watch
and Zwift cannot disagree about a Wednesday.

The formats disagree about one thing on purpose: FIT carries absolute watts
(the plan's measured FTP is the truth), but .zwo only speaks in fractions of
whatever FTP Zwift itself is set to. The exporter divides by the plan's FTP,
which makes "Zwift's FTP setting must match athlete.json" a rule the
documents state rather than a surprise the legs discover.
*/

// zwoFor renders a bike day's steps as a Zwift workout. Warmup and cooldown
// become ramps across their band; a repeat of [work, recovery] becomes
// IntervalsT; every other leaf is a SteadyState at its band's midpoint, or a
// FreeRide if it carries no target. ftp is the plan's measured FTP, the
// denominator of every fraction.
func zwoFor(name string, steps []resolvedStep, ftp int) ([]byte, error) {
	if ftp <= 0 {
		return nil, fmt.Errorf("no FTP to divide by — athlete.json power.ftp is required for .zwo export")
	}
	frac := func(watts int) string {
		return strconv.FormatFloat(float64(watts)/float64(ftp), 'f', 3, 64)
	}
	mid := func(s resolvedStep) int {
		return int(math.Round(float64(s.PowerLo+s.PowerHi) / 2))
	}

	var b strings.Builder
	b.WriteString("<workout_file>\n")
	fmt.Fprintf(&b, "    <author>Run Coach</author>\n")
	fmt.Fprintf(&b, "    <name>%s</name>\n", xmlEscape(name))
	fmt.Fprintf(&b, "    <description>Generated from the plan. Targets are fractions of FTP — Zwift must be set to FTP %d W or every percentage lands on the wrong watts.</description>\n", ftp)
	b.WriteString("    <sportType>bike</sportType>\n")
	b.WriteString("    <workout>\n")

	for i, s := range steps {
		switch {
		case s.Repeat > 0:
			if len(s.Body) == 2 && s.Body[0].Secs > 0 && s.Body[1].Secs > 0 &&
				s.Body[0].PowerLo > 0 && s.Body[1].PowerLo > 0 {
				fmt.Fprintf(&b, "        <IntervalsT Repeat=\"%d\" OnDuration=\"%d\" OffDuration=\"%d\" OnPower=\"%s\" OffPower=\"%s\"/>\n",
					s.Repeat, s.Body[0].Secs, s.Body[1].Secs, frac(mid(s.Body[0])), frac(mid(s.Body[1])))
				continue
			}
			// A body IntervalsT cannot express is unrolled — same steps,
			// longer spelling.
			for n := 0; n < s.Repeat; n++ {
				for _, leaf := range s.Body {
					if err := zwoLeaf(&b, leaf, frac, mid); err != nil {
						return nil, fmt.Errorf("steps[%d]: %w", i, err)
					}
				}
			}
		case s.Role == "warmup" && s.PowerLo > 0:
			fmt.Fprintf(&b, "        <Warmup Duration=\"%d\" PowerLow=\"%s\" PowerHigh=\"%s\"/>\n",
				s.Secs, frac(s.PowerLo), frac(s.PowerHi))
		case s.Role == "cooldown" && s.PowerLo > 0:
			// Zwift ramps a Cooldown downward from PowerHigh to PowerLow.
			fmt.Fprintf(&b, "        <Cooldown Duration=\"%d\" PowerLow=\"%s\" PowerHigh=\"%s\"/>\n",
				s.Secs, frac(s.PowerLo), frac(s.PowerHi))
		default:
			if err := zwoLeaf(&b, s, frac, mid); err != nil {
				return nil, fmt.Errorf("steps[%d]: %w", i, err)
			}
		}
	}

	b.WriteString("    </workout>\n")
	b.WriteString("</workout_file>\n")
	return []byte(b.String()), nil
}

func zwoLeaf(b *strings.Builder, s resolvedStep, frac func(int) string, mid func(resolvedStep) int) error {
	if s.Secs <= 0 {
		return fmt.Errorf("a .zwo step must be timed")
	}
	if s.PowerLo <= 0 {
		// No target: Zwift's FreeRide keeps the clock without ERG.
		fmt.Fprintf(b, "        <FreeRide Duration=\"%d\"/>\n", s.Secs)
		return nil
	}
	fmt.Fprintf(b, "        <SteadyState Duration=\"%d\" Power=\"%s\"/>\n", s.Secs, frac(mid(s)))
	return nil
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;").Replace(s)
}

// zwoSlug is the download filename, sharing the FIT slug's base so the two
// exports of one day sort together.
func zwoSlug(name string) string { return fitSlugBase(name) + ".zwo" }
