package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

/*
A run session may carry "steps": the structured elaboration of its Detail,
served as a Garmin FIT workout file. The prescription's prose stays the source
a human reads; the steps are the same session in typed durations and targets,
and the loader holds the two to agreement so the label/detail drift bug cannot
reopen one layer down.

Every string field except role is a template resolved against the day's full
context, like every other data string. The split below mirrors the loader's
two passes: validateSteps checks shape with no athlete in scope (from
Block.validate), resolveSteps renders and checks meaning (from dryRun, for
every week × day — and from the /fit handler at request time, so validation
and serving cannot disagree).
*/

// maxFitSteps is the watch's budget of emitted workout_step messages (leaves
// plus one per repeat). Past it the watch rejects or truncates silently, so
// the loader refuses first. The encoder deliberately does not know this
// number — see fit.go's error contract.
const maxFitSteps = 50

// minUntargetedActiveSecs is where an untargeted timed active step stops
// being a stride and starts being a rep with no countdown: the Epix lacks the
// step-time-remaining data field, so a five-minute effort with no pace or HR
// band shows no gauge and no clock. Distance steps always show distance
// remaining, so only time is gated.
const minUntargetedActiveSecs = 120

// maxStepNoteLen is what the watch will actually display of a step note.
const maxStepNoteLen = 200

// WorkoutStep is one entry in a session's steps: a leaf (one measured or
// timed segment) or a repeat wrapping leaves. JSON has no union type, so one
// struct carries both forms and validateSteps refuses a step that is both or
// neither.
type WorkoutStep struct {
	// the leaf form
	Role  string       `json:"role,omitempty"`  // warmup active recovery cooldown rest — the label the watch shows
	Dist  string       `json:"dist,omitempty"`  // exactly one of dist or time; runs only
	Time  string       `json:"time,omitempty"`  // clock string; the colon is required
	Pace  []string     `json:"pace,omitempty"`  // [fast, slow] — the paceRange convention; runs only
	HR    []HRBound    `json:"hr,omitempty"`    // [lo, hi] bpm
	Power []PowerBound `json:"power,omitempty"` // [lo, hi] watts — bikes only; what ERG drives to
	Note  string       `json:"note,omitempty"`  // the only prescription text the watch shows

	// the repeat form
	Repeat int           `json:"repeat,omitempty"` // ≥ 2
	Steps  []WorkoutLeaf `json:"steps,omitempty"`
}

// WorkoutLeaf is a repeat's body item: a WorkoutStep minus the repeat fields,
// so a repeat inside a repeat is unrepresentable in the types rather than
// merely refused. check-schema flags the stray "repeat" key; what survives
// decoding is a role-less body item, which validateSteps catches with the
// nesting hint.
type WorkoutLeaf struct {
	Role  string       `json:"role"`
	Dist  string       `json:"dist,omitempty"`
	Time  string       `json:"time,omitempty"`
	Pace  []string     `json:"pace,omitempty"`
	HR    []HRBound    `json:"hr,omitempty"`
	Power []PowerBound `json:"power,omitempty"`
	Note  string       `json:"note,omitempty"`
}

// HRBound is one end of a heart-rate band: a bare bpm number or a template
// string that resolves to one. It is held as the string form so a literal and
// a template go down the same resolution path at load.
type HRBound string

func (h *HRBound) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*h = HRBound(s)
		return nil
	}
	var n int
	if err := json.Unmarshal(b, &n); err == nil {
		*h = HRBound(strconv.Itoa(n))
		return nil
	}
	return fmt.Errorf(`hr bound must be a bpm number or a template string like "{{.Athlete.HR.easyLo}}"`)
}

// PowerBound is one end of a power band: a bare watts number or a template
// resolving to one — {{pct 62 .Athlete.Power.ftp}} is the idiom, so a band
// authored as a percentage keeps tracking the measured FTP.
type PowerBound string

func (p *PowerBound) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*p = PowerBound(s)
		return nil
	}
	var n int
	if err := json.Unmarshal(b, &n); err == nil {
		*p = PowerBound(strconv.Itoa(n))
		return nil
	}
	return fmt.Errorf(`power bound must be a watts number or a template string like "{{pct 62 .Athlete.Power.ftp}}"`)
}

/* ── structural validation (no athlete in scope) ───────────────────────── */

// validateSteps is the shape half of the steps rules, hooked into
// Block.validate's day loop. Anything that needs a resolved value — units,
// bands, budgets in metres — waits for resolveSteps.
func (s Session) validateSteps() error {
	if len(s.Steps) == 0 {
		return nil
	}
	if !s.Kind.IsRun() && !s.Kind.IsBike() {
		return fmt.Errorf("steps on a %q session — steps are for runs and trainer rides", s.Kind)
	}
	isBike := s.Kind.IsBike()
	emitted := 0
	for i, st := range s.Steps {
		isRepeat := st.Repeat != 0 || len(st.Steps) > 0
		isLeaf := st.Role != "" || st.Dist != "" || st.Time != "" ||
			len(st.Pace) > 0 || len(st.HR) > 0 || len(st.Power) > 0 || st.Note != ""
		if isRepeat && isLeaf {
			return fmt.Errorf("steps[%d] mixes repeat and leaf fields — a step is one or the other", i)
		}
		if !isRepeat {
			if err := validateLeaf(st.leaf(), false, isBike); err != nil {
				return fmt.Errorf("steps[%d]: %w", i, err)
			}
			emitted++
			continue
		}
		if st.Repeat < 2 {
			return fmt.Errorf("steps[%d]: repeat is %d — a repeat runs its body at least twice", i, st.Repeat)
		}
		if len(st.Steps) == 0 {
			return fmt.Errorf("steps[%d]: repeat %d has an empty body", i, st.Repeat)
		}
		for j, lf := range st.Steps {
			if err := validateLeaf(lf, true, isBike); err != nil {
				return fmt.Errorf("steps[%d].steps[%d]: %w", i, j, err)
			}
		}
		emitted += len(st.Steps) + 1 // the body, plus the repeat message itself
	}
	if emitted > maxFitSteps {
		return fmt.Errorf("steps emit %d FIT messages — the watch takes at most %d", emitted, maxFitSteps)
	}
	return nil
}

func (st WorkoutStep) leaf() WorkoutLeaf {
	return WorkoutLeaf{Role: st.Role, Dist: st.Dist, Time: st.Time,
		Pace: st.Pace, HR: st.HR, Power: st.Power, Note: st.Note}
}

func validateLeaf(l WorkoutLeaf, inRepeat, isBike bool) error {
	if l.Role == "" {
		if inRepeat {
			return fmt.Errorf("no role — repeats cannot nest, and every leaf names one of warmup/active/recovery/cooldown/rest")
		}
		return fmt.Errorf("no role — want one of warmup/active/recovery/cooldown/rest")
	}
	if strings.Contains(l.Role, "{{") {
		// The role is the FIT intensity enum, which IS the label the watch
		// displays; it is the one field the dialect keeps literal.
		return fmt.Errorf("role %q is a template — roles are literal", l.Role)
	}
	if _, ok := fitIntensities[l.Role]; !ok {
		return fmt.Errorf("unknown role %q — want one of warmup/active/recovery/cooldown/rest", l.Role)
	}
	if isBike {
		// Trainer work is timed and power-shaped: distance and pace belong
		// to the road, and ERG drives watts.
		if l.Dist != "" {
			return fmt.Errorf("dist on a bike step — trainer steps are timed")
		}
		if l.Time == "" {
			return fmt.Errorf("a bike step needs a time")
		}
		if len(l.Pace) > 0 {
			return fmt.Errorf("pace on a bike step — the trainer takes power")
		}
	} else {
		if len(l.Power) > 0 {
			return fmt.Errorf("power on a run step — run power is not in the dialect")
		}
		if (l.Dist == "") == (l.Time == "") {
			return fmt.Errorf("want exactly one of dist or time")
		}
	}
	targets := 0
	for _, n := range []int{len(l.Pace), len(l.HR), len(l.Power)} {
		if n > 0 {
			targets++
		}
	}
	if targets > 1 {
		return fmt.Errorf("more than one of pace, hr and power — a step carries at most one target")
	}
	if len(l.Pace) > 0 && len(l.Pace) != 2 {
		return fmt.Errorf("pace has %d elements, want two: [fast, slow]", len(l.Pace))
	}
	if len(l.HR) > 0 && len(l.HR) != 2 {
		return fmt.Errorf("hr has %d elements, want two: [lo, hi]", len(l.HR))
	}
	if len(l.Power) > 0 && len(l.Power) != 2 {
		return fmt.Errorf("power has %d elements, want two: [lo, hi]", len(l.Power))
	}
	return nil
}

/* ── resolution (dryRun and the /fit handler) ──────────────────────────── */

// resolvedStep is a step after templates: canonical units, ready for the
// encoder. A repeat carries its body and nothing else.
type resolvedStep struct {
	Role     string
	DistM    float64 // metres; set on a distance leaf
	Secs     int     // seconds; set on a time leaf
	PaceFast Pace    // s/m; zero when the step has no pace band
	PaceSlow Pace
	HRLo     int // bpm; zero when the step has no HR band
	HRHi     int
	PowerLo  int // watts; zero when the step has no power band
	PowerHi  int
	Note     string // emphasis-stripped and transliterated, exactly as the file will carry it

	Repeat int
	Body   []resolvedStep
}

// emittedStep is one workout_step as the FILE will carry it. The flattening
// is the numbering: a recorded lap's wkt_step_index is an index into this
// slice, so the encoder and anything reading a lap back must agree here or
// the join silently addresses the wrong step. One function, both readers.
type emittedStep struct {
	Leaf     resolvedStep // the step itself; meaningless on a repeat marker
	IsRepeat bool         // the trailing marker that closes a repeat
	Times    int          // how many times, on the marker
	First    int          // index of the repeat's first body step, on the marker
	Group    int          // 1-based repeat this leaf belongs to; 0 outside one
	Reps     int          // that repeat's count, on a leaf inside one
}

// flattenSteps is the dialect's one statement of emitted order: repeats
// body-first with the repeat step trailing, exactly as the watch expects and
// as maxFitSteps counts.
func flattenSteps(steps []resolvedStep) []emittedStep {
	var out []emittedStep
	group := 0
	for _, s := range steps {
		if s.Repeat > 0 {
			group++
			first := len(out)
			for _, b := range s.Body {
				out = append(out, emittedStep{Leaf: b, Group: group, Reps: s.Repeat})
			}
			out = append(out, emittedStep{IsRepeat: true, Times: s.Repeat, First: first})
			continue
		}
		out = append(out, emittedStep{Leaf: s})
	}
	return out
}

// resolveSteps renders a session's steps against the day's context and checks
// everything only a resolved value can show. It assumes validateSteps has
// passed, which is true for anything the loader accepted.
func resolveSteps(sc *Ctx, sess Session) ([]resolvedStep, error) {
	out := make([]resolvedStep, 0, len(sess.Steps))
	for i, st := range sess.Steps {
		if st.Repeat > 0 {
			r := resolvedStep{Repeat: st.Repeat}
			for j, lf := range st.Steps {
				rl, err := resolveLeaf(sc, lf)
				if err != nil {
					return nil, fmt.Errorf("steps[%d].steps[%d]: %w", i, j, err)
				}
				r.Body = append(r.Body, rl)
			}
			out = append(out, r)
			continue
		}
		rl, err := resolveLeaf(sc, st.leaf())
		if err != nil {
			return nil, fmt.Errorf("steps[%d]: %w", i, err)
		}
		out = append(out, rl)
	}
	return out, nil
}

func resolveLeaf(sc *Ctx, l WorkoutLeaf) (resolvedStep, error) {
	r := resolvedStep{Role: l.Role}
	// A template that renders to nothing is as fatal as one that fails: an
	// empty dist would otherwise surface as a parse error two steps removed
	// from the anchor that vanished this week.
	resolve := func(what, s string) (string, error) {
		v, err := sc.resolve(s)
		if err != nil {
			return "", fmt.Errorf("%s: %w", what, err)
		}
		if strings.TrimSpace(v) == "" {
			return "", fmt.Errorf("%s %q resolved to nothing this week", what, s)
		}
		return v, nil
	}

	if l.Dist != "" {
		v, err := resolve("dist", l.Dist)
		if err != nil {
			return r, err
		}
		d, err := ParseDistance(v)
		if err != nil {
			return r, err
		}
		if d <= 0 {
			return r, fmt.Errorf("dist %q is not a positive distance", v)
		}
		r.DistM = float64(d)
	}
	if l.Time != "" {
		v, err := resolve("time", l.Time)
		if err != nil {
			return r, err
		}
		secs, err := parseStepTime(v)
		if err != nil {
			return r, err
		}
		r.Secs = secs
	}
	if len(l.Pace) == 2 {
		fast, err := resolvePace(sc, l.Pace[0])
		if err != nil {
			return r, fmt.Errorf("pace[0]: %w", err)
		}
		slow, err := resolvePace(sc, l.Pace[1])
		if err != nil {
			return r, fmt.Errorf("pace[1]: %w", err)
		}
		// "0:00/km" parses to a zero pace, and a zero PaceFast is the
		// encoder's "no band" signal — an unset anchor would silently strip
		// the target from the file instead of failing here.
		if fast <= 0 || slow <= 0 {
			return r, fmt.Errorf("pace band [%q, %q] contains a non-positive pace", l.Pace[0], l.Pace[1])
		}
		// Fast first everywhere, matching paceRange. FIT wants the band as
		// speeds, low first — that inversion is the encoder's job, never the
		// data's, so authoring stays consistent across the whole repo.
		if fast > slow {
			return r, fmt.Errorf("pace band [%q, %q] is slow-first — write the fast end first, as paceRange does",
				l.Pace[0], l.Pace[1])
		}
		r.PaceFast, r.PaceSlow = fast, slow
	}
	if len(l.HR) == 2 {
		lo, err := resolveHRBound(sc, l.HR[0])
		if err != nil {
			return r, fmt.Errorf("hr[0]: %w", err)
		}
		hi, err := resolveHRBound(sc, l.HR[1])
		if err != nil {
			return r, fmt.Errorf("hr[1]: %w", err)
		}
		// FIT custom HR is bpm+100, and small raw values mean %max — a bound
		// outside a human range would encode cleanly and coach the wrong
		// effort invisibly, which is why this is fatal rather than clamped.
		for _, n := range []int{lo, hi} {
			if n < 30 || n > 250 {
				return r, fmt.Errorf("hr bound %d is outside 30–250 bpm", n)
			}
		}
		if lo > hi {
			return r, fmt.Errorf("hr band [%d, %d] is high-first — write the low end first", lo, hi)
		}
		r.HRLo, r.HRHi = lo, hi
	}
	if len(l.Power) == 2 {
		lo, err := resolvePowerBound(sc, l.Power[0])
		if err != nil {
			return r, fmt.Errorf("power[0]: %w", err)
		}
		hi, err := resolvePowerBound(sc, l.Power[1])
		if err != nil {
			return r, fmt.Errorf("power[1]: %w", err)
		}
		// FIT power values at or under 1000 mean %FTP, so an implausible
		// wattage would not just coach wrong — it could flip encodings.
		// 20–1000 W brackets any human hour on a trainer.
		for _, n := range []int{lo, hi} {
			if n < 20 || n > 1000 {
				return r, fmt.Errorf("power bound %d is outside 20–1000 W", n)
			}
		}
		if lo > hi {
			return r, fmt.Errorf("power band [%d, %d] is high-first — write the low end first", lo, hi)
		}
		// A zero-width band spams under/over-power alerts on every sample —
		// ridden and confirmed 12 Aug 2026. ERG drives to the middle either
		// way, so the band's width is pure alert tolerance: give it some.
		if lo == hi {
			return r, fmt.Errorf("power band [%d, %d] has no width — the watch alerts on every sample; give the target a tolerance", lo, hi)
		}
		r.PowerLo, r.PowerHi = lo, hi
	}
	if l.Note != "" {
		v, err := resolve("note", l.Note)
		if err != nil {
			return r, err
		}
		note, err := fitTransliterate(stripEmph(v))
		if err != nil {
			return r, fmt.Errorf("note: %w", err)
		}
		if len(note) > maxStepNoteLen {
			return r, fmt.Errorf("note is %d characters after processing — the watch shows at most %d", len(note), maxStepNoteLen)
		}
		r.Note = note
	}
	if r.Role == "active" && r.Secs >= minUntargetedActiveSecs &&
		r.PaceFast == 0 && r.HRLo == 0 && r.PowerLo == 0 {
		return r, fmt.Errorf("an active %s step with no pace, hr or power target — the watch would show no countdown and no gauge; target it or make it a distance step", clock(r.Secs))
	}
	return r, nil
}

func resolvePowerBound(sc *Ctx, p PowerBound) (int, error) {
	v, err := sc.resolve(string(p))
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("%q did not resolve to a watts number", string(p))
	}
	return n, nil
}

func resolvePace(sc *Ctx, s string) (Pace, error) {
	v, err := sc.resolve(s)
	if err != nil {
		return 0, err
	}
	if strings.TrimSpace(v) == "" {
		return 0, fmt.Errorf("%q resolved to nothing this week", s)
	}
	return ParsePace(v)
}

func resolveHRBound(sc *Ctx, h HRBound) (int, error) {
	v, err := sc.resolve(string(h))
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("%q did not resolve to a bpm number", string(h))
	}
	return n, nil
}

// parseStepTime reads a step duration as seconds: "0:20", "3:00", "75:00",
// "1:15:00". units.go's parseClock is strictly M:SS and, worse for this use,
// reads a bare "180" as seconds — a plausible typo for three minutes — so
// step times require the colon.
func parseStepTime(s string) (int, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf(`time %q: want "M:SS" or "H:MM:SS" — the colon is required`, s)
	}
	total := 0
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("time %q: bad component %q", s, p)
		}
		if i > 0 && n > 59 {
			return 0, fmt.Errorf("time %q: %q must be under 60", s, p)
		}
		total = total*60 + n
	}
	if total <= 0 {
		return 0, fmt.Errorf("time %q: not a positive duration", s)
	}
	return total, nil
}

// stepsDistance is the metres the distance leaves add up to, repeats
// multiplied. Time leaves contribute nothing — their distance depends on the
// pace actually run — so this is a lower bound, and the loader checks only
// the over side against Session.Dist. Under-measure is sweep-review
// territory.
func stepsDistance(steps []resolvedStep) float64 {
	var m float64
	for _, s := range steps {
		if s.Repeat > 0 {
			var body float64
			for _, b := range s.Body {
				body += b.DistM
			}
			m += float64(s.Repeat) * body
			continue
		}
		m += s.DistM
	}
	return m
}

// stepsSeconds is the timed total, repeats multiplied — the bike counterpart
// of stepsDistance. A trainer workout IS its steps: the watch ends it when
// they do, so a bike day's sum is checked against Session.Mins the way a run
// day's metres are checked against Session.Dist.
func stepsSeconds(steps []resolvedStep) int {
	total := 0
	for _, s := range steps {
		if s.Repeat > 0 {
			body := 0
			for _, b := range s.Body {
				body += b.Secs
			}
			total += s.Repeat * body
			continue
		}
		total += s.Secs
	}
	return total
}
