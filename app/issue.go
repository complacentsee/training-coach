package main

import (
	"fmt"
	"strconv"
	"strings"
)

/*
An issue is something the athlete is carrying and working back from -- a calf
that aches, a hip that grumbles -- rated once a day so the plan can respond to
it rather than find out in November.

This used to be one hardcoded concept called "calf": a kind in the log, a pair
of methods on the store, a card in the template, a 0-10 scale in the CSS and a
green/amber/red threshold in an if-else. All of it was this athlete's particular
niggle, which meant the app could only ever be run by someone with the same one.

An issue owns three things: the daily rating, the bands that say what a rating
means for today's training, and the rehab phases that carry it back. The phases
live on the block because they change when the block does; the issue lives on
the athlete because the injury does not go away when a block ends.
*/

// Issue is a declared injury or niggle under active management.
type Issue struct {
	// Key is the log identity. The log is append-only, so this must never
	// change once anything has been recorded against it.
	Key   string `json:"key"`
	Name  string `json:"name"`            // "Left calf"
	Short string `json:"short,omitempty"` // card heading; defaults to Name
	Ask   string `json:"ask,omitempty"`   // the question the scale answers
	Hint  string `json:"hint,omitempty"`
	// Guide is the protocol that treats it -- the popup behind the card.
	Guide string `json:"guide,omitempty"`
	// Protocol is what the published document treating this issue is called.
	// "Calf & Glute Protocol" is a title, not a derivation of "Left calf".
	Protocol string `json:"protocol,omitempty"`
	// Retest names the benchmark tag whose weeks this issue is re-measured in,
	// so "retest at weeks 1, 5, 9, 13" is derived from the sessions carrying
	// that tag rather than written out and left behind by the next block.
	Retest string `json:"retest,omitempty"`
	Scale  Scale  `json:"scale"`
	Bands  []Band `json:"bands"`
}

// Scale is the range of the daily rating, with a word at each end so the
// numbers mean the same thing on day 90 as on day 1.
type Scale struct {
	Min  int    `json:"min"`
	Max  int    `json:"max"`
	Low  string `json:"low,omitempty"`
	High string `json:"high,omitempty"`
}

func (s Scale) Values() []int {
	out := make([]int, 0, s.Max-s.Min+1)
	for v := s.Min; v <= s.Max; v++ {
		out = append(out, v)
	}
	return out
}

func (s Scale) contains(v int) bool { return v >= s.Min && v <= s.Max }

// Band is a stretch of the scale and what it means for today. UpTo is
// inclusive; the last band leaves it out and catches everything above.
type Band struct {
	UpTo  *int   `json:"upto,omitempty"`
	Tone  string `json:"tone"` // go | caution | stop
	Label string `json:"label"`
	// When describes what the rating feels like and Action says what to do
	// about it. The card shows the action, because by then the rating has been
	// given; the published protocol shows both, because there it is the
	// definition of the scale.
	When   string `json:"when,omitempty"`
	Action string `json:"action,omitempty"`
}

// tones are the three the stylesheet knows how to colour. They were validated
// for CVD separation once already, which is why this is a closed set rather
// than a colour in the data file.
var tones = map[string]bool{"go": true, "caution": true, "stop": true}

// BandFor is what a rating means. Bands are validated ascending and total, so
// the first match is the right one and the last always matches.
func (i *Issue) BandFor(v int) *Band {
	for bi := range i.Bands {
		b := &i.Bands[bi]
		if b.UpTo == nil || v <= *b.UpTo {
			return b
		}
	}
	return nil
}

// Heading is what the card is titled.
func (i *Issue) Heading() string {
	if i.Short != "" {
		return i.Short
	}
	return i.Name
}

// Range renders "0 nothing · 10 worst imaginable" for the card's footnote.
func (i *Issue) Range() string {
	lo := strconv.Itoa(i.Scale.Min)
	hi := strconv.Itoa(i.Scale.Max)
	if i.Scale.Low != "" {
		lo += " " + i.Scale.Low
	}
	if i.Scale.High != "" {
		hi += " " + i.Scale.High
	}
	return lo + " · " + hi
}

func (i *Issue) validate() error {
	if i.Key == "" {
		return fmt.Errorf("issue has no key — the log is keyed by it")
	}
	if strings.ContainsAny(i.Key, " \t:") {
		return fmt.Errorf("issue key %q: no spaces or colons, it is a log key", i.Key)
	}
	if i.Name == "" {
		return fmt.Errorf("issue %q has no name", i.Key)
	}
	if i.Scale.Max <= i.Scale.Min {
		return fmt.Errorf("issue %q: scale is %d–%d, want max above min", i.Key, i.Scale.Min, i.Scale.Max)
	}
	// A rating is a row of buttons on a phone. Beyond this it stops being one.
	if n := i.Scale.Max - i.Scale.Min + 1; n > 21 {
		return fmt.Errorf("issue %q: a %d-point scale will not fit a phone; keep it under 21", i.Key, n)
	}
	if len(i.Bands) == 0 {
		return fmt.Errorf("issue %q has no bands — a rating with no meaning is not actionable", i.Key)
	}

	prev := i.Scale.Min - 1
	for bi, b := range i.Bands {
		if !tones[b.Tone] {
			return fmt.Errorf("issue %q band %d: tone is %q, want go, caution or stop", i.Key, bi+1, b.Tone)
		}
		if b.Label == "" {
			return fmt.Errorf("issue %q band %d has no label", i.Key, bi+1)
		}
		last := bi == len(i.Bands)-1
		if last {
			if b.UpTo != nil {
				return fmt.Errorf("issue %q: the last band must omit upto so it catches the top of the scale", i.Key)
			}
			continue
		}
		if b.UpTo == nil {
			return fmt.Errorf("issue %q band %d needs an upto — only the last band may omit it", i.Key, bi+1)
		}
		if *b.UpTo <= prev {
			return fmt.Errorf("issue %q band %d: upto %d does not advance past %d; bands ascend and may not overlap",
				i.Key, bi+1, *b.UpTo, prev)
		}
		if !i.Scale.contains(*b.UpTo) {
			return fmt.Errorf("issue %q band %d: upto %d is outside the scale %d–%d",
				i.Key, bi+1, *b.UpTo, i.Scale.Min, i.Scale.Max)
		}
		prev = *b.UpTo
	}
	return nil
}
