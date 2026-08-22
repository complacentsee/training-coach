package main

// The log's backup. entries.jsonl is the athlete's health record and the one
// file in <data> nothing can rebuild: the plan ships from git, the metrics
// cache re-derives from the archive, the archive's own files are immutable
// once written. The log is appended to by hand all day, and until 21 Aug
// 2026 its only copy was the one being written.
//
// This keeps dated copies BESIDE it, in <data>/backups/entries-YYYY-MM-DD.jsonl:
// one per athlete-local calendar day, taken at startup and again just after
// local midnight, never overwritten once taken, and pruned to the newest
// backupKeep. It is the app that does this rather than a cron beside it
// because the app is the log's only writer — it can copy the bytes under the
// store's own lock so no append ever lands mid-copy — and because it owns the
// file's permissions: the container writes the log as root, and a copy
// attempted from outside by the ssh user is exactly how seed-dev's log copy
// failed silently for weeks.
//
// What this protects against is the file being damaged — a rewrite, a
// truncation, a bad seed, a migration that was not a rebuild. It is on the
// same disk as the original by the athlete's choice (21 Aug 2026: no remote),
// so it is not a defence against losing the disk.
//
// The copy is the file's BYTES, never a re-marshal of the entries the store
// holds in memory: a line the reader skips as malformed is still part of the
// record and is still in the copy.
//
// Invisible to the plan by construction — the data Rev hashes only the
// files loadDataset takes and fingerprint() counts only non-hidden .json
// files, so a snapshot can neither rotate workout identities nor trigger a
// reload; pinned by test beside the activity store's equivalent.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// backupKeep is how many daily snapshots survive a prune: the newest thirty
// by date. In steady state that is thirty days of history. It is a count of
// snapshots rather than a cut-off date on purpose: a server that was off for
// six weeks must not delete every copy it holds the moment it boots, because
// the one it takes at that boot is the only one that would remain.
const backupKeep = 30

// backupAfterMidnight is how long past local midnight the daily snapshot
// waits. Nothing about the log turns over at midnight — the date in the name
// is the date the copy was taken — but a few minutes keeps a snapshot that
// straddles the boundary from being named for the day that just ended.
const backupAfterMidnight = 5 * time.Minute

// backupName is the only shape of file the prune will ever delete. Anything
// else in the directory — a hand-made copy, a note — is not this code's to
// remove.
var backupName = regexp.MustCompile(`^entries-(\d{4}-\d{2}-\d{2})\.jsonl$`)

// errNoLog is the first run of a fresh install: there is no log yet, so
// there is nothing to copy and nothing to report as a failure.
var errNoLog = errors.New("no log to back up")

func backupDir(dataDir string) string {
	return filepath.Join(dataDir, "backups")
}

func backupPath(dir, date string) string {
	return filepath.Join(dir, "entries-"+date+".jsonl")
}

// snapshotLog takes the day's snapshot if it has not been taken yet, then
// prunes. It returns the snapshot's path and whether this call created it.
//
// Order matters: prune runs only once today's snapshot is known to exist —
// either made here or found from an earlier start today — so the set can
// never shrink without first having grown. A snapshot that fails leaves the
// older copies exactly as they were.
func snapshotLog(store *Store, dir, date string) (path string, created bool, err error) {
	path = backupPath(dir, date)
	if _, err := os.Stat(path); err == nil {
		return path, false, pruneSnapshots(dir, backupKeep)
	}
	if _, err := os.Stat(store.path); os.IsNotExist(err) {
		return path, false, errNoLog // a fresh install: no dir made for nothing
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return path, false, fmt.Errorf("create %s: %w", dir, err)
	}
	if err := store.SnapshotTo(path); err != nil {
		return path, false, err
	}
	return path, true, pruneSnapshots(dir, backupKeep)
}

// snapshotDates lists the dated snapshots in dir, oldest first. Only names
// the prune is allowed to delete are listed.
func snapshotDates(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var dates []string
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		if m := backupName.FindStringSubmatch(e.Name()); m != nil {
			dates = append(dates, m[1])
		}
	}
	sort.Strings(dates)
	return dates, nil
}

// pruneSnapshots deletes every dated snapshot but the newest keep. A file
// that will not delete is reported and the rest still go.
func pruneSnapshots(dir string, keep int) error {
	dates, err := snapshotDates(dir)
	if err != nil {
		return fmt.Errorf("list %s: %w", dir, err)
	}
	var errs []error
	for len(dates) > keep {
		if err := os.Remove(backupPath(dir, dates[0])); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
		dates = dates[1:]
	}
	return errors.Join(errs...)
}

// latestSnapshot is the date of the newest snapshot on disk, or "" — what
// /healthz reports, read from the directory rather than remembered, so it
// says what is there and not what was last attempted.
func latestSnapshot(dir string) string {
	dates, err := snapshotDates(dir)
	if err != nil || len(dates) == 0 {
		return ""
	}
	return dates[len(dates)-1]
}

// nextBackupAt is 00:05 on the next local calendar date, in now's location.
// Computed from the calendar rather than by adding 24h so a DST change still
// lands it at 00:05 on the wall clock. The next date is identified by its
// noon — an hour no DST gap has ever covered — because in a zone whose gap
// sits at midnight (America/Santiago, America/Havana) 00:05 on that date
// does not exist and Go resolves it to 23:05 on THIS date; the walk then
// steps forward to the first instant that is actually on the next date.
// Every instant on the next date is after every instant on this one, so
// the result is always after now.
func nextBackupAt(now time.Time) time.Time {
	loc := now.Location()
	y, m, d := now.Date()
	ty, tm, td := time.Date(y, m, d+1, 12, 0, 0, 0, loc).Date()
	next := time.Date(ty, tm, td, 0, 0, 0, 0, loc).Add(backupAfterMidnight)
	for ny, nm, nd := next.Date(); ny != ty || nm != tm || nd != td; ny, nm, nd = next.Date() {
		next = next.Add(time.Hour)
	}
	return next
}

// backupOnce runs one snapshot for the athlete's current local date and
// says what happened. A failure is loud in the log and visible on /healthz
// as a stale or missing date — which is what make verify checks — but it
// does not stop the site: a log that cannot be copied is still a log that
// can be read.
func (s *server) backupOnce() {
	dir := backupDir(s.dataDir)
	date := s.day(s.ds()).Format("2006-01-02")
	path, created, err := snapshotLog(s.store, dir, date)
	switch {
	case errors.Is(err, errNoLog):
		return
	case err != nil:
		log.Printf("backup FAILED for %s: %v", date, err)
	case created:
		log.Printf("backup    %s", path)
	}
}

// backupLoop takes one snapshot every day just after local midnight until
// ctx ends. The startup snapshot is main's, taken synchronously before the
// listener opens so the first /healthz already reports it. The local
// midnight is the athlete's, re-read each round so a reloaded athlete.json
// that changes timezone is honoured.
func (s *server) backupLoop(ctx context.Context) {
	for {
		now := time.Now().In(s.ds().Loc)
		t := time.NewTimer(time.Until(nextBackupAt(now)))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
			s.backupOnce()
			if s.coach != nil {
				s.coach.prune()
			}
		}
	}
}
