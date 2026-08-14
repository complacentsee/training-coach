package main

import (
	"fmt"
	"regexp"
	"time"
)

// athleteSchema is the version this binary understands. Every data file
// declares one so a future session can tell an old block from a new one and
// migrate it deliberately rather than by guessing.
const athleteSchema = "athlete/1"

// Athlete is who the plan is for: the anchors every prescription is expressed
// against. The typed fields are the ones the app itself reasons about; the maps
// are open so a block can template against anything an athlete tracks without
// this struct growing a field per sport.
type Athlete struct {
	Schema   string `json:"schema"`
	Name     string `json:"name"`
	Place    string `json:"place,omitempty"` // shown beside the name on the published masthead
	Units    Units  `json:"units"`
	Timezone string `json:"timezone"`

	Weight Weight `json:"weight,omitempty"`

	HR    map[string]int    `json:"hr,omitempty"`    // heart-rate anchors, bpm
	Power map[string]int    `json:"power,omitempty"` // watts
	Paces map[string]Pace   `json:"paces,omitempty"` // named reference paces
	Notes map[string]string `json:"notes,omitempty"` // free text for templates

	// Issues are the injuries under active management: what gets rated daily,
	// what the rating means for today, and what the block is rehabbing. They
	// belong to the athlete rather than the block because an injury does not
	// end when a block does.
	Issues []Issue `json:"issues,omitempty"`

	// App is how the install presents itself on a home screen. It lives with
	// the athlete because here the athlete *is* the install.
	App App `json:"app,omitempty"`
}

// App is the product identity: the name under the icon, the colour behind the
// status bar. Deliberately separate from the block — "Run Coach" is the app and
// "16-Week Build" is what he is doing in it, and the header used to say the
// second one as though it were the first, which stops being true the moment a
// four-week block exists.
type App struct {
	Name        string `json:"name,omitempty"`
	Short       string `json:"short,omitempty"`
	Description string `json:"description,omitempty"`
	Theme       Theme  `json:"theme,omitempty"`
}

// Theme is the status-bar colour in each mode. It reaches a <meta> tag and a
// manifest, so it is checked rather than trusted.
type Theme struct {
	Light string `json:"light,omitempty"`
	Dark  string `json:"dark,omitempty"`
}

func (a *App) applyDefaults() {
	if a.Name == "" {
		a.Name = "Run Coach"
	}
	if a.Short == "" {
		a.Short = a.Name
	}
	if a.Theme.Light == "" {
		a.Theme.Light = "#F2F2EF"
	}
	if a.Theme.Dark == "" {
		a.Theme.Dark = "#12171B"
	}
}

var hexColour = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func (a *App) validate() error {
	for what, c := range map[string]string{"light": a.Theme.Light, "dark": a.Theme.Dark} {
		if !hexColour.MatchString(c) {
			return fmt.Errorf("app.theme.%s is %q, want a six-digit hex colour like #12171B", what, c)
		}
	}
	return nil
}

// Issue looks up a declared issue by its log key.
func (a *Athlete) Issue(key string) *Issue {
	if a == nil {
		return nil
	}
	for i := range a.Issues {
		if a.Issues[i].Key == key {
			return &a.Issues[i]
		}
	}
	return nil
}

func (a *Athlete) validate() error {
	if a.Schema != athleteSchema {
		return fmt.Errorf("schema is %q, this build understands %q", a.Schema, athleteSchema)
	}
	if a.Units == "" {
		a.Units = Imperial
	}
	if !a.Units.valid() {
		return fmt.Errorf("units is %q, want %q or %q", a.Units, Imperial, Metric)
	}
	if a.Timezone != "" {
		if _, err := time.LoadLocation(a.Timezone); err != nil {
			return fmt.Errorf("timezone %q: %w", a.Timezone, err)
		}
	}
	a.App.applyDefaults()
	if err := a.App.validate(); err != nil {
		return err
	}
	seen := map[string]bool{}
	for i := range a.Issues {
		if err := a.Issues[i].validate(); err != nil {
			return err
		}
		if seen[a.Issues[i].Key] {
			return fmt.Errorf("two issues share the key %q — the log could not tell them apart", a.Issues[i].Key)
		}
		seen[a.Issues[i].Key] = true
	}
	return nil
}

// location is the athlete's timezone, falling back to the server's flag.
func (a *Athlete) location(fallback *time.Location) *time.Location {
	if a == nil || a.Timezone == "" {
		return fallback
	}
	loc, err := time.LoadLocation(a.Timezone)
	if err != nil {
		return fallback
	}
	return loc
}
