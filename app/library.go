package main

import (
	"fmt"
	"sort"
	"strings"
)

const (
	librarySchema = "library/1"
	indexSchema   = "index/1"
)

// Section is one labelled block inside a guide: "Set up", "Do", "Error".
type Section struct {
	Label string `json:"label"`
	Text  string `json:"text"`
}

// Step is one line of a guide's checklist. Guide links it to the movement entry
// that explains it, so a task popup can be drilled into.
type Step struct {
	Text  string `json:"text"`
	Guide string `json:"guide,omitempty"`
	// Key is the log identity for ticking this step off. It defaults to
	// "<guide id>#<n>", which is what the existing log already contains, so it
	// must not change for guides that do not set it. A guide whose steps vary
	// by week can set it explicitly to keep a movement's identity stable as
	// the list around it grows.
	Key string `json:"key,omitempty"`

	// Sets is how many times the movement repeats. Above one, the popup offers
	// a box per set, so a session cut short after two of three records what
	// actually happened rather than a bare "done" -- which is what the log
	// said on 4 Aug for a split squat that was explicitly not completed.
	Sets int `json:"sets,omitempty"`
}

// SetKeys are the per-set log keys, empty when a step is a single unit.
func (s Step) SetKeys() []string {
	if s.Sets < 2 || s.Key == "" {
		return nil
	}
	out := make([]string, s.Sets)
	for i := range out {
		out[i] = fmt.Sprintf("%s:%d", s.Key, i+1)
	}
	return out
}

// ProgressUnits are the tickable items a guide contributes to the Today badge:
// one per set where sets are declared, otherwise one per step. The exercise
// checkbox itself is excluded — it is independent of the sets, so counting both
// would double every completed movement.
func ProgressUnits(g Guide) []string {
	var out []string
	for _, st := range g.Steps {
		if ks := st.SetKeys(); ks != nil {
			out = append(out, ks...)
			continue
		}
		if st.Key != "" {
			out = append(out, st.Key)
		}
	}
	return out
}

// Guide is what a popup shows.
type Guide struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Summary  string    `json:"summary,omitempty"` // one line for the workouts index
	Sections []Section `json:"sections,omitempty"`
	Why      string    `json:"why,omitempty"`
	Video    string    `json:"video,omitempty"`
	Article  string    `json:"article,omitempty"`
	Source   string    `json:"source,omitempty"`
	Group    string    `json:"group,omitempty"` // movement library section

	// Order positions a movement within its group. The calf list runs
	// isometric → double-leg → single-leg → loaded → plyometric, which is a
	// progression rather than an alphabet, and sorting by title destroyed it.
	// Zero sorts last and falls back to the title, so an exercise added without
	// one still appears without anybody editing the index.
	Order int `json:"order,omitempty"`

	// The published strength document carries these three and the app does not
	// render any of them. They live here anyway: they are properties of the
	// guide, and the exporter cannot regenerate that document without them.
	VideoTitle   string `json:"videoTitle,omitempty"`
	ArticleTitle string `json:"articleTitle,omitempty"`
	// Match is the pattern the document uses to link a protocol line to this
	// entry. Alternatives are separated by "|".
	Match string `json:"match,omitempty"`

	Steps []Step `json:"steps,omitempty"`

	// Variants let one guide differ by week — the calf work that is bodyweight
	// in phase A and loaded in phase B is the same guide, not three. A variant
	// field replaces the base's; it never merges, because a merge rule nobody
	// can predict is worse than a little duplication in the data.
	Variants []GuideVariant `json:"variants,omitempty"`
}

type GuideVariant struct {
	Weeks    WeekRange `json:"weeks"`
	Title    string    `json:"title,omitempty"`
	Summary  string    `json:"summary,omitempty"`
	Why      string    `json:"why,omitempty"`
	Sections []Section `json:"sections,omitempty"`
	Steps    []Step    `json:"steps,omitempty"`
}

// GuideFile is one library file on disk.
type GuideFile struct {
	Schema string            `json:"schema"`
	Guides map[string]*Guide `json:"guides"`
}

// forWeek collapses a guide's variants down to the one that applies.
func (g *Guide) forWeek(week int) Guide {
	out := *g
	out.Variants = nil
	for _, v := range g.Variants {
		if !v.Weeks.Contains(week) {
			continue
		}
		if v.Title != "" {
			out.Title = v.Title
		}
		if v.Summary != "" {
			out.Summary = v.Summary
		}
		if v.Why != "" {
			out.Why = v.Why
		}
		if len(v.Sections) > 0 {
			out.Sections = v.Sections
		}
		if len(v.Steps) > 0 {
			out.Steps = v.Steps
		}
		break
	}
	return out
}

// resolve renders every string in the guide and assigns the step log keys.
func (g *Guide) resolve(c *Ctx) (Guide, error) {
	out := g.forWeek(c.week())
	var err error

	if out.Title, err = c.resolve(out.Title); err != nil {
		return out, fmt.Errorf("title: %w", err)
	}
	if out.Summary, err = c.resolve(out.Summary); err != nil {
		return out, fmt.Errorf("summary: %w", err)
	}
	if out.Why, err = c.resolve(out.Why); err != nil {
		return out, fmt.Errorf("why: %w", err)
	}

	if len(out.Sections) > 0 {
		secs := make([]Section, len(out.Sections))
		for i, s := range out.Sections {
			if s.Text, err = c.resolve(s.Text); err != nil {
				return out, fmt.Errorf("sections[%d] (%s): %w", i, s.Label, err)
			}
			if s.Label, err = c.resolve(s.Label); err != nil {
				return out, fmt.Errorf("sections[%d].label: %w", i, err)
			}
			secs[i] = s
		}
		out.Sections = secs
	}

	if len(out.Steps) > 0 {
		steps := make([]Step, len(out.Steps))
		for i, st := range out.Steps {
			if st.Text, err = c.resolve(st.Text); err != nil {
				return out, fmt.Errorf("steps[%d]: %w", i, err)
			}
			if st.Key == "" {
				st.Key = fmt.Sprintf("%s#%d", out.ID, i+1)
			}
			steps[i] = st
		}
		out.Steps = steps
	}
	return out, nil
}

// StepKeys returns the log keys for a resolved guide's steps.
func StepKeys(g Guide) []string {
	out := make([]string, 0, len(g.Steps))
	for _, st := range g.Steps {
		out = append(out, st.Key)
	}
	return out
}

// IsMovement distinguishes the exercise library from session and task guides.
func (g Guide) IsMovement() bool { return strings.HasPrefix(g.ID, "m-") }

// sortMovements puts a group's exercises in their taught order. Both the
// workouts page and the exported document use this, so the two cannot present
// the same library in two different orders.
func sortMovements(gs []Guide) {
	sort.SliceStable(gs, func(i, j int) bool {
		a, b := gs[i].Order, gs[j].Order
		if a != b {
			if a == 0 || b == 0 {
				return b == 0 // an unordered guide sorts after an ordered one
			}
			return a < b
		}
		return gs[i].Title < gs[j].Title
	})
}

/* ── the workouts index ────────────────────────────────────────────────── */

// Index is the layout of the workouts page: which guides appear, in what
// groups, and what the right-hand column says. `when` is a template, so
// "3×" and "6 Aug · 9 Sep" are both just counts over the block rather than
// numbers anyone has to keep in sync.
type Index struct {
	Schema string       `json:"schema"`
	Groups []IndexGroup `json:"groups"`
}

type IndexGroup struct {
	Title string     `json:"title"`
	Note  string     `json:"note,omitempty"`
	Rows  []IndexRow `json:"rows,omitempty"`
	// Movements, when set, fills the group with every movement guide carrying
	// that library group, sorted by title — so adding an exercise to the
	// library puts it on the page without touching the index.
	Movements string `json:"movements,omitempty"`
}

type IndexRow struct {
	Guide  string `json:"guide"`
	When   string `json:"when,omitempty"`
	Detail string `json:"detail,omitempty"`
}

func (ix *Index) validate() error {
	if ix.Schema != indexSchema {
		return fmt.Errorf("schema is %q, this build understands %q", ix.Schema, indexSchema)
	}
	for i, g := range ix.Groups {
		if g.Title == "" {
			return fmt.Errorf("groups[%d] has no title", i)
		}
		if len(g.Rows) == 0 && g.Movements == "" {
			return fmt.Errorf("groups[%d] (%s) has neither rows nor a movements group", i, g.Title)
		}
	}
	return nil
}
