package main

import (
	"fmt"
	"strings"
	"time"
)

const blockSchema = "block/1"

// Kind is the flavour of a session. It drives colour, which targets are shown,
// and whether distance or minutes is the meaningful number.
type Kind string

const (
	KindRest     Kind = "rest"
	KindEasy     Kind = "easy"
	KindLong     Kind = "long"
	KindRecovery Kind = "recovery"
	KindQuality  Kind = "quality"
	KindBikeEasy Kind = "bike_easy"
	KindBikeHard Kind = "bike_hard"
)

// IsRun reports whether the session's distance is the number that matters.
// Kinds are open — a block may invent its own — so anything carrying a distance
// counts, with the built-in run kinds always qualifying.
func (k Kind) IsRun() bool {
	switch k {
	case KindEasy, KindLong, KindRecovery, KindQuality:
		return true
	}
	return false
}

// IsBike reports whether the session is trainer work: the kinds whose steps
// carry power targets and whose FIT files say sport=cycling.
func (k Kind) IsBike() bool { return k == KindBikeEasy || k == KindBikeHard }

func (k Kind) slug() string { return strings.ReplaceAll(string(k), "_", "-") }

// Session is one day's prescribed work.
type Session struct {
	Kind  Kind     `json:"kind"`
	Label string   `json:"label"`
	Dist  Distance `json:"dist,omitempty"`
	Mins  int      `json:"mins,omitempty"`
	Tag   string   `json:"tag,omitempty"` // benchmark id: FTP, LT, DEC, TT, RACE …

	// Detail is the full prescription — warm-up, main set, recoveries, cool-down
	// — where Label is the short form a calendar cell has room for. It carries
	// *emphasis* rather than markup, so a data file still cannot inject HTML.
	// The published plan document held this and the app did not, which is how
	// the two came to disagree about what a Tuesday actually is.
	Detail string `json:"detail,omitempty"`

	// Targets replace the kind's guidance for this session. Eight of the long
	// runs finish at marathon or threshold pace, and on those the block's
	// governing rule inverts: pace is the input and heart rate is expected to
	// climb through it. Keyed off the kind alone, the app told him to walk when
	// the alarm fired in the middle of a prescribed tempo. Replaces rather than
	// merges, for the same reason guide variants do — the lines it displaces
	// are not merely incomplete, they are wrong for this session.
	Targets []string `json:"targets,omitempty"`

	// Steps is the session's structured form for a watch: FIT workout steps
	// elaborating Detail, served as a download from /fit/{date}. Run-only,
	// optional, and validated to agree with the session it elaborates —
	// steps.go owns the dialect.
	Steps []WorkoutStep `json:"steps,omitempty"`
}

// Amount is the headline number the calendar and cards show.
func (s Session) Amount(u Units) string {
	switch {
	case s.Kind == KindRest:
		return "—"
	case s.Dist > 0:
		return s.Dist.In(u)
	case s.Mins > 0:
		return fmt.Sprintf("%d′", s.Mins)
	default:
		return "—"
	}
}

// GuideID maps a session to the popup that explains it. A tagged benchmark
// always wins over the generic kind.
func (s Session) GuideID() string {
	if s.Tag != "" {
		return "t-" + s.Tag
	}
	if s.Kind == KindRest {
		return ""
	}
	return "s-" + s.Kind.slug()
}

// Week is one row of the block. Only the sessions are stored: the Monday, the
// date range and the weekly volume are all derivable, and a derived number
// cannot disagree with the sessions it came from.
type Week struct {
	N    int       `json:"n"`
	Days []Session `json:"days"`

	// Tags label the week's role — "down", "peak", "test", "taper", "race".
	// Authored rather than derived: peak and race fall out of the sessions, but
	// nothing in a week's data distinguishes a down week from a taper, and
	// guessing from falling volume tags the last three weeks of a build wrongly.
	Tags []string `json:"tags,omitempty"`

	block *Block
}

func (w *Week) Start() time.Time { return w.block.StartDate().AddDate(0, 0, (w.N-1)*7) }

func (w *Week) StartISO() string { return w.Start().Format("2006-01-02") }

// Dates reads "Aug 3–9", or "Aug 31–Sep 6" when the week straddles a month.
func (w *Week) Dates() string {
	a := w.Start()
	b := a.AddDate(0, 0, 6)
	if a.Month() == b.Month() {
		return fmt.Sprintf("%s %d–%d", a.Format("Jan"), a.Day(), b.Day())
	}
	return fmt.Sprintf("%s %d–%s %d", a.Format("Jan"), a.Day(), b.Format("Jan"), b.Day())
}

// Volume is the week's running distance, summed from the sessions.
func (w *Week) Volume() Distance {
	var total Distance
	for _, s := range w.Days {
		if s.Kind.IsRun() {
			total += s.Dist
		}
	}
	return total
}

func (w *Week) Phase() *Phase         { return w.block.PhaseFor(w.N) }
func (w *Week) Mesocycle() *Mesocycle { return w.block.MesocycleFor(w.N) }

// Phase is a named stretch of the block. Setting Issue makes it a stage of that
// issue's rehab rather than a phase of the block at large, which is what lets
// the tolerance → heavy → elastic progression show up inside the calf card
// instead of as an unattributed note under the checklist.
type Phase struct {
	Name   string    `json:"name"`
	Detail string    `json:"detail"`
	Weeks  WeekRange `json:"weeks"`
	Issue  string    `json:"issue,omitempty"`

	// Goal and Points are the published protocol's fuller account of the stage
	// — what it is for, and what characterises it. Detail is the one line the
	// app's card has room for.
	Goal   string   `json:"goal,omitempty"`
	Points []string `json:"points,omitempty"`
}

// Span reads "Weeks 1–3", or "Weeks 14+" for an open-ended stage.
func (p Phase) Span() string {
	if p.Weeks.To == 0 {
		return fmt.Sprintf("Weeks %d+", p.Weeks.From)
	}
	if p.Weeks.To == p.Weeks.From {
		return fmt.Sprintf("Week %d", p.Weeks.From)
	}
	return fmt.Sprintf("Weeks %d–%d", p.Weeks.From, p.Weeks.To)
}

// Mesocycle is the training-emphasis label a week sits under.
type Mesocycle struct {
	Name  string    `json:"name"`
	Weeks WeekRange `json:"weeks"`
}

// VarCase is one week-ranged value of a block variable. Phases and pace bands
// change on different week boundaries, so they cannot share a single series —
// each variable declares its own.
type VarCase struct {
	Weeks WeekRange `json:"weeks"`
	Value string    `json:"value"`
}

// Targets are the guidance lines shown under a session. Tags win over kinds.
type Targets struct {
	Kinds map[string][]string `json:"kinds,omitempty"`
	Tags  map[string][]string `json:"tags,omitempty"`
}

// ChecklistItem is one row of the day's checklist. When is a template that must
// render true for the row to appear — reusing the template mechanism rather
// than inventing a condition language for six rows.
type ChecklistItem struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Guide string `json:"guide,omitempty"`
	When  string `json:"when,omitempty"`
}

// Grading is the calendar's legend. Structured rather than a blob of HTML so
// the styling stays in the template where it belongs.
type Grading struct {
	Note   string      `json:"note,omitempty"`
	Bands  []GradeBand `json:"bands,omitempty"`
	Footer string      `json:"footer,omitempty"`
}

type GradeBand struct {
	Grade string `json:"grade"`
	Range string `json:"range"`
}

type Goal struct {
	Event  string `json:"event"`
	Date   string `json:"date"`
	Target string `json:"target"`
}

// Block is one training block: everything that changes when the goal changes.
type Block struct {
	Schema string `json:"schema"`
	ID     string `json:"id"`
	Name   string `json:"name"`
	Start  string `json:"start"`
	Goal   Goal   `json:"goal"`
	Intro  string `json:"intro,omitempty"`

	// Title and Note are the published document's masthead: the display form of
	// the name, which may carry a line break, and the one-line shape of the
	// block. A masthead is editorial, so it is authored rather than derived —
	// but it is authored *here*, where the dates beside it come from.
	Title string `json:"title,omitempty"`
	Note  string `json:"note,omitempty"`

	// Archived takes a block out of the running for "current" even if its dates
	// say otherwise — for the block that was abandoned halfway rather than
	// finished. Dates alone cannot express that.
	Archived bool `json:"archived,omitempty"`

	Mesocycles []Mesocycle          `json:"mesocycles,omitempty"`
	Phases     []Phase              `json:"phases,omitempty"`
	Vars       map[string][]VarCase `json:"vars,omitempty"`
	Targets    Targets              `json:"targets"`
	Checklist  []ChecklistItem      `json:"checklist,omitempty"`
	Grading    *Grading             `json:"grading,omitempty"`

	// Guides overrides library entries by id, for the block that wants this
	// movement cued differently without forking the whole library.
	Guides map[string]*Guide `json:"guides,omitempty"`
	// Index overrides the library's workouts-page layout.
	Index *Index `json:"index,omitempty"`

	Weeks []*Week `json:"weeks"`

	// loc is the timezone every block date is built in. In a scratch container
	// time.Local is UTC, so parsing dates there while the handlers work in
	// America/Chicago put them five hours apart — enough for a whole-day
	// difference to truncate to the wrong number.
	loc    *time.Location
	path   string            // where it was loaded from, for error messages
	guides map[string]*Guide // library merged with this block's overrides
}

func (b *Block) SetLocation(loc *time.Location) { b.loc = loc }

func (b *Block) location() *time.Location {
	if b.loc == nil {
		return time.UTC
	}
	return b.loc
}

func (b *Block) StartDate() time.Time {
	t, _ := time.ParseInLocation("2006-01-02", b.Start, b.location())
	return t
}

// EndDate is the last day of the last week.
func (b *Block) EndDate() time.Time {
	return b.StartDate().AddDate(0, 0, len(b.Weeks)*7-1)
}

func (b *Block) WeekCount() int { return len(b.Weeks) }

// DayOf returns the date of weekday index di (0=Mon) in week wi (0-based).
func (b *Block) DayOf(wi, di int) time.Time {
	return b.StartDate().AddDate(0, 0, wi*7+di)
}

// Locate returns the week and weekday index (0=Mon) for a date, plus whether
// that date falls inside the block at all.
func (b *Block) Locate(day time.Time) (*Week, int, bool) {
	days := DaysBetween(b.StartDate(), day)
	if days < 0 {
		return nil, 0, false
	}
	wi := days / 7
	if wi >= len(b.Weeks) {
		return nil, 0, false
	}
	return b.Weeks[wi], days % 7, true
}

func (b *Block) Contains(day time.Time) bool {
	_, _, ok := b.Locate(day)
	return ok
}

// DaysUntilStart is negative once the block is underway.
func (b *Block) DaysUntilStart(day time.Time) int {
	return DaysBetween(day, b.StartDate())
}

// Span reads "Aug 2026 – Nov 2026", or "Aug – Nov 2026" within one year.
func (b *Block) Span() string {
	from, to := b.StartDate(), b.EndDate()
	if from.Year() == to.Year() {
		return fmt.Sprintf("%s – %s %d", from.Format("Jan"), to.Format("Jan"), to.Year())
	}
	return fmt.Sprintf("%s – %s", from.Format("Jan 2006"), to.Format("Jan 2006"))
}

// Volume is the block's total running distance.
func (b *Block) Volume() Distance {
	var total Distance
	for _, w := range b.Weeks {
		total += w.Volume()
	}
	return total
}

func (b *Block) WeekByN(n int) *Week {
	if n < 1 || n > len(b.Weeks) {
		return nil
	}
	return b.Weeks[n-1]
}

// PhaseFor is the block's own phase for a week — the ones no issue owns.
func (b *Block) PhaseFor(week int) *Phase { return b.phaseOf("", week) }

// PhaseForIssue is where an issue's rehab has reached in a given week.
func (b *Block) PhaseForIssue(issue string, week int) *Phase { return b.phaseOf(issue, week) }

func (b *Block) phaseOf(issue string, week int) *Phase {
	for i := range b.Phases {
		if b.Phases[i].Issue == issue && b.Phases[i].Weeks.Contains(week) {
			return &b.Phases[i]
		}
	}
	return nil
}

func (b *Block) MesocycleFor(week int) *Mesocycle {
	for i := range b.Mesocycles {
		if b.Mesocycles[i].Weeks.Contains(week) {
			return &b.Mesocycles[i]
		}
	}
	return nil
}

// CountKind is how many times a session kind appears across the block.
func (b *Block) CountKind(k Kind) int {
	n := 0
	for _, w := range b.Weeks {
		for _, s := range w.Days {
			if s.Kind == k {
				n++
			}
		}
	}
	return n
}

// TagDates lists the dates a benchmark falls on, formatted for display.
func (b *Block) TagDates(tag string) []string {
	var out []string
	for wi, w := range b.Weeks {
		for di, s := range w.Days {
			if s.Tag == tag {
				out = append(out, b.DayOf(wi, di).Format("2 Jan"))
			}
		}
	}
	return out
}

// TargetsFor is the guidance for a session: what the session itself says, else
// its benchmark tag, else its kind. Most specific wins.
func (b *Block) TargetsFor(s Session) []string {
	if len(s.Targets) > 0 {
		return s.Targets
	}
	if s.Tag != "" {
		if t, ok := b.Targets.Tags[s.Tag]; ok {
			return t
		}
	}
	if t, ok := b.Targets.Kinds[string(s.Kind)]; ok {
		return t
	}
	return nil
}

// DaysBetween counts whole calendar days, immune to any clock offset.
func DaysBetween(from, to time.Time) int {
	a := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	b := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC)
	return int(b.Sub(a).Hours() / 24)
}

/* ── validation ────────────────────────────────────────────────────────── */

func (b *Block) validate() error {
	if b.Schema != blockSchema {
		return fmt.Errorf("schema is %q, this build understands %q", b.Schema, blockSchema)
	}
	if b.ID == "" {
		return fmt.Errorf("block has no id")
	}
	if _, err := time.Parse("2006-01-02", b.Start); err != nil {
		return fmt.Errorf("start %q: want YYYY-MM-DD", b.Start)
	}
	if b.StartDate().Weekday() != time.Monday {
		return fmt.Errorf("start %s is a %s; blocks begin on a Monday", b.Start, b.StartDate().Weekday())
	}
	if len(b.Weeks) == 0 {
		return fmt.Errorf("block has no weeks")
	}
	for i, w := range b.Weeks {
		if w.N != i+1 {
			return fmt.Errorf("weeks[%d] is numbered %d, want %d", i, w.N, i+1)
		}
		if len(w.Days) != 7 {
			return fmt.Errorf("week %d has %d days, want 7", w.N, len(w.Days))
		}
		for di, s := range w.Days {
			if s.Kind == "" {
				return fmt.Errorf("week %d day %d has no kind", w.N, di+1)
			}
			if s.Label == "" {
				return fmt.Errorf("week %d day %d has no label", w.N, di+1)
			}
			if err := s.validateSteps(); err != nil {
				return fmt.Errorf("week %d day %d: %w", w.N, di+1, err)
			}
		}
	}
	n := len(b.Weeks)
	// Coverage is checked per owner: the block's own phases must be total on
	// their own, and so must each issue's rehab. Checking them together would
	// call two independent progressions an overlap.
	byIssue := map[string][]Phase{}
	for _, p := range b.Phases {
		byIssue[p.Issue] = append(byIssue[p.Issue], p)
	}
	for issue, ps := range byIssue {
		what := "phases"
		if issue != "" {
			what = "phases for issue " + issue
		}
		if err := checkCoverage(what, weekRanges(ps, func(p Phase) WeekRange { return p.Weeks }), n); err != nil {
			return err
		}
	}
	if len(b.Mesocycles) > 0 {
		if err := checkCoverage("mesocycles", weekRanges(b.Mesocycles, func(m Mesocycle) WeekRange { return m.Weeks }), n); err != nil {
			return err
		}
	}
	for name, cases := range b.Vars {
		if err := checkCoverage("vars."+name, weekRanges(cases, func(c VarCase) WeekRange { return c.Weeks }), n); err != nil {
			return err
		}
	}
	for i, it := range b.Checklist {
		if it.Key == "" {
			return fmt.Errorf("checklist[%d] has no key — the log is keyed by it", i)
		}
	}
	return nil
}

func weekRanges[T any](items []T, get func(T) WeekRange) []WeekRange {
	out := make([]WeekRange, len(items))
	for i, it := range items {
		out[i] = get(it)
	}
	return out
}
