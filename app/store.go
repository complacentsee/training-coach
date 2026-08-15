package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Entry is one appended event. The log is append-only: checking a task off and
// then un-checking it writes two entries, and the current state is a replay.
// That keeps history (did he skip it, or skip it and come back?) and avoids
// read-modify-write races entirely.
type Entry struct {
	TS   time.Time `json:"ts"`             // when it was recorded
	Date string    `json:"date"`           // the training day it refers to, YYYY-MM-DD
	Kind string    `json:"kind"`           // "task" | "issue" | "note" | "metric" | "grade"
	Key  string    `json:"key,omitempty"`  // task id, issue key, or metric name
	Val  string    `json:"val,omitempty"`  // "done"/"undone", a number, free text
	Note string    `json:"note,omitempty"` // optional feedback attached to the entry
}

// Store is an append-only JSONL file. Small enough to hold in memory —
// sixteen weeks of daily entries is a few thousand lines at most.
type Store struct {
	path string
	mu   sync.RWMutex
	all  []Entry
}

func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	s := &Store{path: filepath.Join(dir, "entries.jsonl")}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	f, err := os.Open(s.path)
	if os.IsNotExist(err) {
		return nil // first run
	}
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for ln := 1; sc.Scan(); ln++ {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			// A corrupt line should not take the site down. Skip it loudly.
			fmt.Fprintf(os.Stderr, "store: skipping malformed line %d: %v\n", ln, err)
			continue
		}
		s.all = append(s.all, e)
	}
	return sc.Err()
}

func (s *Store) Append(e Entry) error {
	if e.TS.IsZero() {
		e.TS = time.Now()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open log for append: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("append: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	s.all = append(s.all, e)
	return nil
}

// TasksFor replays the log and returns the current done-state per task key for
// a given date. Last write wins.
func (s *Store) TasksFor(date string) map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]bool{}
	for _, e := range s.all {
		if e.Kind == "task" && e.Date == date {
			out[e.Key] = e.Val == "done"
		}
	}
	return out
}

const (
	// kindIssue is what a daily issue rating is written as now: the issue's key
	// travels in Key, so one kind serves however many issues are declared.
	kindIssue = "issue"
	// legacyCalfKind is what the first hundred entries used, when "calf" was
	// hardcoded rather than declared. The log is append-only and is the athlete's health
	// record, so those are read as the issue they always were, never rewritten.
	legacyCalfKind = "calf"
)

// issueKeyOf reports which issue an entry rates, or "" if it rates none.
func issueKeyOf(e Entry) string {
	switch e.Kind {
	case kindIssue:
		return e.Key
	case legacyCalfKind:
		return legacyCalfKind
	}
	return ""
}

// Ratings returns every rating for an issue, oldest first, one per date
// (last write wins).
func (s *Store) Ratings(key string) []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	byDate := map[string]Entry{}
	for _, e := range s.all {
		if issueKeyOf(e) == key {
			byDate[e.Date] = e
		}
	}
	out := make([]Entry, 0, len(byDate))
	for _, e := range byDate {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	return out
}

// RatingOn returns the rating recorded for an issue on a date, if any. The log
// is append-only, so a re-rating appends and the last one wins.
func (s *Store) RatingOn(key, date string) *Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var found *Entry
	for i := range s.all {
		if issueKeyOf(s.all[i]) == key && s.all[i].Date == date {
			e := s.all[i]
			found = &e
		}
	}
	return found
}

// kindSkip is a session the athlete did not do and said so. It is a fact
// about the day, not a failure to record one: a blank day cannot tell
// "skipped, no time" from "forgot to log it", and the difference changes
// what the week means and how the next session reads.
const kindSkip = "skip"

// SkipOn reports whether the session on a date is currently marked skipped,
// with the entry that says so. Append-only, so unskipping writes another
// entry and the last one wins — the history of changing his mind survives.
func (s *Store) SkipOn(date string) (*Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var found *Entry
	for i := range s.all {
		if s.all[i].Kind == kindSkip && s.all[i].Date == date {
			e := s.all[i]
			found = &e
		}
	}
	if found == nil || found.Val != "skipped" {
		return found, false
	}
	return found, true
}

// Skips returns every date currently marked skipped, with its entry.
func (s *Store) Skips() map[string]Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]Entry{}
	for _, e := range s.all {
		if e.Kind != kindSkip {
			continue
		}
		if e.Val == "skipped" {
			out[e.Date] = e
		} else {
			delete(out, e.Date)
		}
	}
	return out
}

// Grades returns the graded result per training date, keyed YYYY-MM-DD.
// Grades are pushed in by Claude after the session data is analysed — the app
// records compliance, but the grade needs Strava, so it arrives from outside.
func (s *Store) Grades() map[string]Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]Entry{}
	for _, e := range s.all {
		if e.Kind == "grade" && e.Key != "week" {
			out[e.Date] = e
		}
	}
	return out
}

// WeekGrades is keyed by the Monday of the week it grades.
func (s *Store) WeekGrades() map[string]Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]Entry{}
	for _, e := range s.all {
		if e.Kind == "grade" && e.Key == "week" {
			out[e.Date] = e
		}
	}
	return out
}

// Notes returns free-text feedback newest first — this is the channel back to
// Claude, so it is meant to be read, not aggregated.
func (s *Store) Notes(limit int) []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Entry
	for i := len(s.all) - 1; i >= 0 && len(out) < limit; i-- {
		if s.all[i].Kind == "note" || s.all[i].Note != "" {
			out = append(out, s.all[i])
		}
	}
	return out
}

// All is the raw export, used by /api/entries so the whole log can be pulled
// down and read without shell access to the box.
func (s *Store) All() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Entry, len(s.all))
	copy(out, s.all)
	return out
}
