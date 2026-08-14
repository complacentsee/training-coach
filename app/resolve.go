package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"text/template"
)

/*
Every human-readable string in the data files is a template resolved against
the athlete, the block, and where in the block we are. That single mechanism is
what lets the binary stop carrying anyone's heart rates: "HR cap 155" becomes
"HR cap {{.Athlete.HR.EasyCap}}" and lives in JSON.

text/template, not html/template: the output is inserted into an html/template
page, which escapes it there. Escaping twice would render &amp;#39; to the user.
*/

// maxVarDepth stops a variable that refers to itself from taking the site down.
const maxVarDepth = 8

// Ctx is what a data template sees. Fields are exported because templates
// address them by name; the unexported ones are plumbing.
type Ctx struct {
	Athlete *Athlete
	Block   *Block
	Week    *Week
	Phase   *Phase
	Meso    *Mesocycle
	Session *Session
	InBlock bool

	depth int
}

// Kind, Tag and Resting are nil-safe views of the session, so a checklist
// predicate can ask about today's work on a day that has none — outside the
// block there is no session at all, and {{.Session.Kind}} would blow up.
func (c *Ctx) Kind() string {
	if c.Session == nil {
		return ""
	}
	return string(c.Session.Kind)
}

func (c *Ctx) Tag() string {
	if c.Session == nil {
		return ""
	}
	return c.Session.Tag
}

func (c *Ctx) Resting() bool {
	return c.Session == nil || c.Session.Kind == KindRest
}

func (c *Ctx) units() Units {
	if c.Athlete != nil && c.Athlete.Units.valid() {
		return c.Athlete.Units
	}
	return Imperial
}

func (c *Ctx) week() int {
	if c.Week == nil {
		return 1
	}
	return c.Week.N
}

// funcs are what a data template may call. dist/pace/wt take either a literal
// carrying its unit ({{pace "9:45/mi"}}) or a quantity reached through the
// context ({{pace .Athlete.Paces.decoupling}}), so an author never has to know
// which of the two they are holding.
func (c *Ctx) funcs() template.FuncMap {
	u := c.units()
	return template.FuncMap{
		"dist": func(v any) (string, error) { d, err := toDistance(v); return d.In(u), err },
		"pace": func(v any) (string, error) { p, err := toPace(v); return p.In(u), err },
		"wt":   func(v any) (string, error) { w, err := toWeight(v); return w.In(u), err },

		// paceRange shares one unit suffix across a band: "9:45–11:00/mi", not
		// "9:45/mi–11:00/mi". Rendering each end separately reads as two
		// unrelated paces rather than one range.
		"paceRange": func(lo, hi any) (string, error) {
			a, err := toPace(lo)
			if err != nil {
				return "", err
			}
			b, err := toPace(hi)
			if err != nil {
				return "", err
			}
			from := a.In(u)
			if i := strings.Index(from, "/"); i > 0 {
				from = from[:i]
			}
			return from + "–" + b.In(u), nil
		},

		// kg forces kilograms regardless of the athlete's units, for the
		// contexts where the metric unit *is* the concept — W/kg being the
		// obvious one, where "at 170 lb" would read as a mistake.
		"kg": func(v any) (string, error) {
			w, err := toWeight(v)
			return w.In(Metric), err
		},

		"pct": pctOf,
		"wkg": c.wattsPerKilo,

		// The inverses and companions of wkg. Every one of these replaces a
		// figure that was computed by hand and then outlived the bodyweight it
		// was computed against -- 2.95 W/kg, 31%, 185 lb.
		"perkg":  c.kilosPerWatt,
		"pctBW":  c.percentOfBodyweight,
		"bwPlus": c.bodyweightPlus,

		"var":          c.lookupVar,
		"count":        c.countKind,
		"dates":        c.tagDates,
		"sessionGuide": c.sessionGuide,
		"phase":        c.issuePhase,
		"phaseDetail":  c.issuePhaseDetail,

		"lower": strings.ToLower,
		"upper": strings.ToUpper,
		"trim":  strings.TrimSpace,
	}
}

func toDistance(v any) (Distance, error) {
	switch x := v.(type) {
	case Distance:
		return x, nil
	case string:
		return ParseDistance(x)
	default:
		return 0, fmt.Errorf(`dist: want a distance or a string like "7 mi", got %T`, v)
	}
}

func toPace(v any) (Pace, error) {
	switch x := v.(type) {
	case Pace:
		return x, nil
	case string:
		return ParsePace(x)
	default:
		return 0, fmt.Errorf(`pace: want a pace or a string like "9:45/mi", got %T`, v)
	}
}

func toWeight(v any) (Weight, error) {
	switch x := v.(type) {
	case Weight:
		return x, nil
	case string:
		return ParseWeight(x)
	default:
		return 0, fmt.Errorf(`wt: want a weight or a string like "25 lb", got %T`, v)
	}
}

func toFloat(v any) (float64, error) {
	switch x := v.(type) {
	case int:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case float64:
		return x, nil
	case Weight:
		return float64(x), nil
	case Distance:
		return float64(x), nil
	default:
		return 0, fmt.Errorf("want a number, got %T", v)
	}
}

// pctOf renders a percentage of a value: {{pct 62 .Athlete.Power.ftp}}.
func pctOf(percent, of any) (string, error) {
	p, err := toFloat(percent)
	if err != nil {
		return "", fmt.Errorf("pct: percentage: %w", err)
	}
	v, err := toFloat(of)
	if err != nil {
		return "", fmt.Errorf("pct: value: %w", err)
	}
	return strconv.Itoa(int(math.Round(v * p / 100))), nil
}

// wattsPerKilo turns a W/kg figure into watts for this athlete. Every such
// number in the prose was previously computed by hand against a bodyweight,
// which is why they all had to be corrected the last time the scale moved.
func (c *Ctx) wattsPerKilo(v any) (string, error) {
	x, err := toFloat(v)
	if err != nil {
		return "", fmt.Errorf("wkg: %w", err)
	}
	if c.Athlete == nil || c.Athlete.Weight <= 0 {
		return "", fmt.Errorf("wkg: athlete has no weight recorded")
	}
	return strconv.Itoa(int(math.Round(x * float64(c.Athlete.Weight)))), nil
}

// kilosPerWatt renders watts as W/kg: {{perkg .Athlete.Power.ftp}} → "2.78".
func (c *Ctx) kilosPerWatt(v any) (string, error) {
	w, err := toFloat(v)
	if err != nil {
		return "", fmt.Errorf("perkg: %w", err)
	}
	kg, err := c.bodyweight("perkg")
	if err != nil {
		return "", err
	}
	return strconv.FormatFloat(math.Round(w/kg*100)/100, 'f', 2, 64), nil
}

// percentOfBodyweight renders a load as a share of bodyweight:
// {{pctBW "50 lb"}} → "29%". The 31% this replaces was 50 lb against a weight
// that had already moved, and it was live in two guide popups.
func (c *Ctx) percentOfBodyweight(v any) (string, error) {
	load, err := toWeight(v)
	if err != nil {
		return "", fmt.Errorf("pctBW: %w", err)
	}
	kg, err := c.bodyweight("pctBW")
	if err != nil {
		return "", err
	}
	return strconv.Itoa(int(math.Round(float64(load)/kg*100))) + "%", nil
}

// bodyweightPlus is bodyweight carrying a load, for the figures that are "you,
// plus what you are holding" -- and which therefore move when either does.
func (c *Ctx) bodyweightPlus(v any) (Weight, error) {
	load, err := toWeight(v)
	if err != nil {
		return 0, fmt.Errorf("bwPlus: %w", err)
	}
	kg, err := c.bodyweight("bwPlus")
	if err != nil {
		return 0, err
	}
	return Weight(kg) + load, nil
}

func (c *Ctx) bodyweight(who string) (float64, error) {
	if c.Athlete == nil || c.Athlete.Weight <= 0 {
		return 0, fmt.Errorf("%s: athlete has no weight recorded", who)
	}
	return float64(c.Athlete.Weight), nil
}

// lookupVar resolves a week-varying value declared in the block's `vars`. The
// value is itself a template, so easyBand can be written once per phase in the
// author's own units and still render metric.
func (c *Ctx) lookupVar(name string) (string, error) {
	if c.Block == nil {
		return "", fmt.Errorf("var %q: no block in scope", name)
	}
	cases, ok := c.Block.Vars[name]
	if !ok {
		return "", fmt.Errorf("var %q: not declared in this block", name)
	}
	if c.depth >= maxVarDepth {
		return "", fmt.Errorf("var %q: nested more than %d deep (a cycle?)", name, maxVarDepth)
	}
	w := c.week()
	for _, cs := range cases {
		if cs.Weeks.Contains(w) {
			child := *c
			child.depth = c.depth + 1
			return child.resolve(cs.Value)
		}
	}
	return "", fmt.Errorf("var %q: no case covers week %d", name, w)
}

func (c *Ctx) countKind(kind string) (string, error) {
	if c.Block == nil {
		return "", fmt.Errorf("count %q: no block in scope", kind)
	}
	return strconv.Itoa(c.Block.CountKind(Kind(kind))), nil
}

func (c *Ctx) tagDates(tag string) (string, error) {
	if c.Block == nil {
		return "", fmt.Errorf("dates %q: no block in scope", tag)
	}
	return strings.Join(c.Block.TagDates(tag), " · "), nil
}

// issuePhase and issuePhaseDetail are where an issue's rehab has reached this
// week. A guide belonging to an issue asks by key rather than reading .Phase:
// .Phase is the block's own phase, and each issue advances on its own
// boundaries — the calf's tolerance/heavy/elastic stages do not line up with
// the mesocycles, which is the whole reason they are separate.
func (c *Ctx) issuePhase(issue string) (string, error) {
	p, err := c.phaseOf(issue)
	if err != nil {
		return "", err
	}
	return p.Name, nil
}

func (c *Ctx) issuePhaseDetail(issue string) (string, error) {
	p, err := c.phaseOf(issue)
	if err != nil {
		return "", err
	}
	return p.Detail, nil
}

func (c *Ctx) phaseOf(issue string) (*Phase, error) {
	if c.Block == nil {
		return nil, fmt.Errorf("phase %q: no block in scope", issue)
	}
	p := c.Block.PhaseForIssue(issue, c.week())
	if p == nil {
		return nil, fmt.Errorf("phase %q: no phase covers week %d — is the issue key right?", issue, c.week())
	}
	return p, nil
}

func (c *Ctx) sessionGuide() (string, error) {
	if c.Session == nil {
		return "", nil
	}
	return c.Session.GuideID(), nil
}

// resolve renders one template string. Strings with no action are returned
// untouched, which is most of them — the parse is skipped entirely.
func (c *Ctx) resolve(s string) (string, error) {
	if !strings.Contains(s, "{{") {
		return s, nil
	}
	t, err := template.New("d").Option("missingkey=error").Funcs(c.funcs()).Parse(s)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := t.Execute(&b, c); err != nil {
		return "", err
	}
	return b.String(), nil
}

// resolveAll renders a slice, reporting which element failed.
func (c *Ctx) resolveAll(ss []string) ([]string, error) {
	if len(ss) == 0 {
		return nil, nil
	}
	out := make([]string, len(ss))
	for i, s := range ss {
		v, err := c.resolve(s)
		if err != nil {
			return nil, fmt.Errorf("[%d]: %w", i, err)
		}
		out[i] = v
	}
	return out, nil
}

// isTrue reports whether a predicate template renders to something affirmative.
// Checklist rows use this instead of a bespoke condition language: `when` is
// just a template, and the row appears when it says so.
func (c *Ctx) isTrue(s string) (bool, error) {
	if strings.TrimSpace(s) == "" {
		return true, nil
	}
	v, err := c.resolve(s)
	if err != nil {
		return false, err
	}
	switch strings.TrimSpace(strings.ToLower(v)) {
	case "true", "yes", "1":
		return true, nil
	case "false", "no", "0", "":
		return false, nil
	default:
		return false, fmt.Errorf("when: rendered %q, want true or false", v)
	}
}
