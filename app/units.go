package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Units is the measurement system numbers are *rendered* in. Every quantity is
// stored canonically — metres, seconds per metre, kilograms — and converted on
// the way out, so a block authored in miles reads correctly for an athlete who
// thinks in kilometres and vice versa.
type Units string

const (
	Imperial Units = "imperial"
	Metric   Units = "metric"
)

func (u Units) valid() bool { return u == Imperial || u == Metric }

const (
	metresPerMile = 1609.344
	metresPerKM   = 1000.0
	kgPerPound    = 0.45359237
)

/* ── quantities ────────────────────────────────────────────────────────────

Quantities are authored as strings carrying their own unit — "7 mi", "9:45/mi",
"25 lb". A bare number would have to be interpreted against some ambient
declaration, and encoding/json gives UnmarshalJSON no access to that context;
the unit would then live far from the number that needs it. Self-describing
strings cost a few keystrokes and remove the entire class of bug.
*/

// Distance is canonical metres.
type Distance float64

func ParseDistance(s string) (Distance, error) {
	n, unit, err := splitQty(s)
	if err != nil {
		return 0, err
	}
	switch unit {
	case "mi", "mile", "miles":
		return Distance(n * metresPerMile), nil
	case "km", "k":
		return Distance(n * metresPerKM), nil
	case "m", "metre", "metres", "meter", "meters":
		return Distance(n), nil
	default:
		return 0, fmt.Errorf("distance %q: unknown unit %q (want mi, km or m)", s, unit)
	}
}

// In renders the distance for display. Sub-kilometre values keep their metres,
// because "0.4 km" is not how anyone says 400 m.
func (d Distance) In(u Units) string {
	if u == Metric {
		if m := float64(d); m > 0 && m < metresPerKM {
			return trimNum(math.Round(m), 0) + " m"
		}
		return trimNum(float64(d)/metresPerKM, 1) + " km"
	}
	return trimNum(float64(d)/metresPerMile, 1) + " mi"
}

// InMeasured is In for a RECORDED distance rather than a prescribed one. A
// plan is authored in whole and half miles, so rounding to a tenth is exactly
// right for it and "10 mi" is what the calendar should say. A measurement is
// different: two long runs a hundredth of a mile apart both render "10 mi" at
// that precision, and a grade comparing them then described the difference in
// the wrong direction, because the coarse figure was paired against a finer
// one computed elsewhere.
func (d Distance) InMeasured(u Units) string {
	if u == Metric {
		return trimNum(float64(d)/metresPerKM, 2) + " km"
	}
	return trimNum(float64(d)/metresPerMile, 2) + " mi"
}

// InLong spells the unit out, for prose that reads "27 miles" rather than a
// table cell that reads "27 mi".
func (d Distance) InLong(u Units) string {
	n, unit := float64(d)/metresPerMile, "mile"
	if u == Metric {
		n, unit = float64(d)/metresPerKM, "kilometre"
	}
	s := trimNum(n, 1)
	if s != "1" {
		unit += "s"
	}
	return s + " " + unit
}

func (d *Distance) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf(`distance must be a string like "7 mi" or "12 km": %w`, err)
	}
	v, err := ParseDistance(s)
	*d = v
	return err
}

// Pace is canonical seconds per metre.
type Pace float64

func ParsePace(s string) (Pace, error) {
	i := strings.LastIndex(s, "/")
	if i < 0 {
		return 0, fmt.Errorf(`pace %q: want "M:SS/mi" or "M:SS/km"`, s)
	}
	secs, err := parseClock(strings.TrimSpace(s[:i]))
	if err != nil {
		return 0, fmt.Errorf("pace %q: %w", s, err)
	}
	switch strings.ToLower(strings.TrimSpace(s[i+1:])) {
	case "mi", "mile":
		return Pace(secs / metresPerMile), nil
	case "km", "k":
		return Pace(secs / metresPerKM), nil
	default:
		return 0, fmt.Errorf("pace %q: unknown unit (want /mi or /km)", s)
	}
}

func (p Pace) In(u Units) string {
	per, suffix := metresPerMile, "/mi"
	if u == Metric {
		per, suffix = metresPerKM, "/km"
	}
	return clock(int(math.Round(float64(p)*per))) + suffix
}

func (p *Pace) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf(`pace must be a string like "9:45/mi": %w`, err)
	}
	v, err := ParsePace(s)
	*p = v
	return err
}

// Weight is canonical kilograms.
type Weight float64

func ParseWeight(s string) (Weight, error) {
	n, unit, err := splitQty(s)
	if err != nil {
		return 0, err
	}
	switch unit {
	case "lb", "lbs", "pound", "pounds":
		return Weight(n * kgPerPound), nil
	case "kg", "kgs", "kilo", "kilos":
		return Weight(n), nil
	default:
		return 0, fmt.Errorf("weight %q: unknown unit %q (want lb or kg)", s, unit)
	}
}

// In renders a load to a tenth. Rounding to whole kilos looked tidier for
// dumbbells and quietly turned a bodyweight of 77.1 kg into 77.
func (w Weight) In(u Units) string {
	if u == Metric {
		return trimNum(float64(w), 1) + " kg"
	}
	return trimNum(float64(w)/kgPerPound, 1) + " lb"
}

func (w *Weight) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf(`weight must be a string like "25 lb" or "12 kg": %w`, err)
	}
	v, err := ParseWeight(s)
	*w = v
	return err
}

/* ── parsing helpers ───────────────────────────────────────────────────── */

// splitQty cuts "12.5 kg" into 12.5 and "kg".
func splitQty(s string) (float64, string, error) {
	s = strings.TrimSpace(s)
	i := 0
	for i < len(s) && (s[i] == '-' || s[i] == '+' || s[i] == '.' || (s[i] >= '0' && s[i] <= '9')) {
		i++
	}
	if i == 0 {
		return 0, "", fmt.Errorf("quantity %q: does not start with a number", s)
	}
	n, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, "", fmt.Errorf("quantity %q: %w", s, err)
	}
	unit := strings.ToLower(strings.TrimSpace(s[i:]))
	if unit == "" {
		return 0, "", fmt.Errorf("quantity %q: missing unit", s)
	}
	return n, unit, nil
}

// parseClock reads "9:45" or "615" as seconds.
func parseClock(s string) (float64, error) {
	if i := strings.Index(s, ":"); i >= 0 {
		m, err := strconv.Atoi(strings.TrimSpace(s[:i]))
		if err != nil {
			return 0, fmt.Errorf("bad minutes %q", s[:i])
		}
		sec, err := strconv.Atoi(strings.TrimSpace(s[i+1:]))
		if err != nil || sec < 0 || sec > 59 {
			return 0, fmt.Errorf("bad seconds %q", s[i+1:])
		}
		return float64(m*60 + sec), nil
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("bad time %q", s)
	}
	return n, nil
}

func clock(secs int) string {
	if secs < 0 {
		secs = 0
	}
	return fmt.Sprintf("%d:%02d", secs/60, secs%60)
}

// trimNum formats to dp decimals and drops a trailing ".0", so 7 renders as
// "7" and 6.2 as "6.2".
func trimNum(v float64, dp int) string {
	s := strconv.FormatFloat(v, 'f', dp, 64)
	if dp > 0 {
		s = strings.TrimRight(s, "0")
		s = strings.TrimSuffix(s, ".")
	}
	if s == "-0" {
		s = "0"
	}
	return s
}

/* ── week ranges ───────────────────────────────────────────────────────── */

// WeekRange is an inclusive span of week numbers, written "1-4", "7", or "14+"
// for open-ended. Phases, mesocycles, week-varying variables and guide variants
// are all selected this way.
type WeekRange struct {
	From int
	To   int // 0 means open-ended
}

func ParseWeekRange(s string) (WeekRange, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return WeekRange{}, fmt.Errorf("empty week range")
	}
	if strings.HasSuffix(s, "+") {
		n, err := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(s, "+")))
		if err != nil || n < 1 {
			return WeekRange{}, fmt.Errorf("week range %q: bad start", s)
		}
		return WeekRange{From: n}, nil
	}
	// en dash is what a human authoring this will type half the time
	s = strings.ReplaceAll(s, "–", "-")
	from, to, split := strings.Cut(s, "-")
	a, err := strconv.Atoi(strings.TrimSpace(from))
	if err != nil || a < 1 {
		return WeekRange{}, fmt.Errorf("week range %q: bad start", s)
	}
	if !split {
		return WeekRange{From: a, To: a}, nil
	}
	b, err := strconv.Atoi(strings.TrimSpace(to))
	if err != nil || b < a {
		return WeekRange{}, fmt.Errorf("week range %q: bad end", s)
	}
	return WeekRange{From: a, To: b}, nil
}

func (r WeekRange) Contains(week int) bool {
	return week >= r.From && (r.To == 0 || week <= r.To)
}

func (r *WeekRange) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf(`weeks must be a string like "1-4", "7" or "14+": %w`, err)
	}
	v, err := ParseWeekRange(s)
	*r = v
	return err
}

func (r WeekRange) String() string {
	switch {
	case r.To == 0:
		return strconv.Itoa(r.From) + "+"
	case r.To == r.From:
		return strconv.Itoa(r.From)
	default:
		return strconv.Itoa(r.From) + "-" + strconv.Itoa(r.To)
	}
}

// checkCoverage proves a set of ranges assigns exactly one entry to every week
// of the block. Gaps and overlaps are the failure mode that produces a page
// that renders fine for fifteen weeks and blank for the sixteenth, so it is
// worth catching at load rather than in November.
func checkCoverage(what string, ranges []WeekRange, weeks int) error {
	owner := make([]int, weeks+1) // 1-based; 0 = unassigned
	for i, r := range ranges {
		for w := 1; w <= weeks; w++ {
			if !r.Contains(w) {
				continue
			}
			if owner[w] != 0 {
				return fmt.Errorf("%s: week %d matches both %q and %q",
					what, w, ranges[owner[w]-1], r)
			}
			owner[w] = i + 1
		}
	}
	var gaps []string
	for w := 1; w <= weeks; w++ {
		if owner[w] == 0 {
			gaps = append(gaps, strconv.Itoa(w))
		}
	}
	if len(gaps) > 0 {
		return fmt.Errorf("%s: no entry covers week%s %s",
			what, plural(len(gaps)), strings.Join(gaps, ", "))
	}
	return nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
