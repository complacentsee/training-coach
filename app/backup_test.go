package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A store on dir with a few entries and one line the loader refuses: the
// record as it is on disk, not as the store replays it.
func snapshotFixture(t *testing.T, dir string) *Store {
	t.Helper()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i, k := range []string{"task-a", "task-b"} {
		if err := store.Append(Entry{Date: "2026-08-20", Kind: "task", Key: k, Val: "done",
			TS: time.Date(2026, 8, 20, 7, i, 0, 0, time.UTC)}); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.OpenFile(filepath.Join(dir, "entries.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{not json, but still the record\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return store
}

func readOrFail(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// The snapshot is the log's bytes: every line, including the one the
// loader skipped, and nothing re-marshalled.
func TestLogSnapshotIsTheLogsBytes(t *testing.T) {
	dir := t.TempDir()
	store := snapshotFixture(t, dir)
	bdir := backupDir(dir)

	path, created, err := snapshotLog(store, bdir, "2026-08-21")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !created {
		t.Fatal("first snapshot of the day reported as already present")
	}
	if want := filepath.Join(bdir, "entries-2026-08-21.jsonl"); path != want {
		t.Errorf("snapshot path %s, want %s", path, want)
	}
	log := readOrFail(t, filepath.Join(dir, "entries.jsonl"))
	got := readOrFail(t, path)
	if string(got) != string(log) {
		t.Errorf("snapshot differs from the log:\n--- log\n%s--- snapshot\n%s", log, got)
	}
	if !strings.Contains(string(got), "{not json") {
		t.Error("the malformed line is part of the record and must be in the copy")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("snapshot mode %o, want 0644 — the log's own mode, readable for a restore", info.Mode().Perm())
	}
	if stray, _ := filepath.Glob(filepath.Join(bdir, "*.tmp")); len(stray) != 0 {
		t.Errorf("temp file left behind: %v", stray)
	}
}

// One snapshot per date, and a later state of the log never replaces it:
// the second run of a day is a no-op, and a direct write to the name fails.
func TestLogSnapshotIsNeverOverwritten(t *testing.T) {
	dir := t.TempDir()
	store := snapshotFixture(t, dir)
	bdir := backupDir(dir)

	path, _, err := snapshotLog(store, bdir, "2026-08-21")
	if err != nil {
		t.Fatal(err)
	}
	first := readOrFail(t, path)

	if err := store.Append(Entry{Date: "2026-08-21", Kind: "note", Note: "after the snapshot"}); err != nil {
		t.Fatal(err)
	}
	_, created, err := snapshotLog(store, bdir, "2026-08-21")
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if created {
		t.Error("second run of the day took another snapshot")
	}
	if got := readOrFail(t, path); string(got) != string(first) {
		t.Error("the day's snapshot changed after the log grew — a snapshot once taken must stand")
	}
	if err := store.SnapshotTo(path); err == nil {
		t.Error("SnapshotTo onto an existing name succeeded; it must refuse")
	}
	if got := readOrFail(t, path); string(got) != string(first) {
		t.Error("the refused SnapshotTo still changed the file")
	}
	if stray, _ := filepath.Glob(filepath.Join(bdir, "*.tmp")); len(stray) != 0 {
		t.Errorf("temp file left behind by the refused write: %v", stray)
	}

	// The next day is a new name, and it carries the growth.
	next, created, err := snapshotLog(store, bdir, "2026-08-22")
	if err != nil || !created {
		t.Fatalf("next day: created=%v err=%v", created, err)
	}
	if got := readOrFail(t, next); !strings.Contains(string(got), "after the snapshot") {
		t.Error("the next day's snapshot does not carry the entry appended in between")
	}
}

// The newest backupKeep survive; everything the prune was not told about
// — a hand-made copy, a note, a directory — is left exactly where it was.
func TestLogSnapshotPrunesToTheNewest(t *testing.T) {
	dir := t.TempDir()
	store := snapshotFixture(t, dir)
	bdir := backupDir(dir)
	if err := os.MkdirAll(filepath.Join(bdir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	var dates []string
	for i := 0; i < 40; i++ {
		d := start.AddDate(0, 0, i).Format("2006-01-02")
		dates = append(dates, d)
		if err := os.WriteFile(backupPath(bdir, d), []byte(d+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	strangers := []string{"entries-2026-06-01.jsonl.bak", "entries-keep.jsonl", "README.txt", "entries-2026-06-02.jsonl.123.tmp"}
	for _, n := range strangers {
		if err := os.WriteFile(filepath.Join(bdir, n), []byte("mine\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	today := start.AddDate(0, 0, 40).Format("2006-01-02") // 2026-08-10
	if _, created, err := snapshotLog(store, bdir, today); err != nil || !created {
		t.Fatalf("snapshot: created=%v err=%v", created, err)
	}
	got, err := snapshotDates(bdir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != backupKeep {
		t.Fatalf("%d snapshots after prune, want %d: %v", len(got), backupKeep, got)
	}
	want := append(dates[len(dates)-(backupKeep-1):], today)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("kept %v\nwant %v", got, want)
		}
	}
	for _, d := range dates[:len(dates)-(backupKeep-1)] {
		if _, err := os.Stat(backupPath(bdir, d)); !os.IsNotExist(err) {
			t.Errorf("%s should have been pruned", d)
		}
	}
	for _, n := range append(strangers, "subdir") {
		if _, err := os.Stat(filepath.Join(bdir, n)); err != nil {
			t.Errorf("%s was not this code's to remove: %v", n, err)
		}
	}
	if latestSnapshot(bdir) != today {
		t.Errorf("latest %s, want %s", latestSnapshot(bdir), today)
	}
}

// A snapshot that cannot be taken leaves the older copies exactly as they
// were — the set never shrinks without first having grown.
func TestLogSnapshotFailureLeavesOlderCopies(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	dir := t.TempDir()
	store := snapshotFixture(t, dir)
	bdir := backupDir(dir)
	if err := os.MkdirAll(bdir, 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < backupKeep+5; i++ {
		d := start.AddDate(0, 0, i).Format("2006-01-02")
		if err := os.WriteFile(backupPath(bdir, d), []byte(d+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(bdir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(bdir, 0o755) })

	_, created, err := snapshotLog(store, bdir, "2026-08-21")
	if err == nil || created {
		t.Fatalf("snapshot into an unwritable directory: created=%v err=%v", created, err)
	}
	got, lerr := snapshotDates(bdir)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(got) != backupKeep+5 {
		t.Errorf("%d snapshots after a failed run, want all %d untouched", len(got), backupKeep+5)
	}
}

// A fresh install has no log: nothing to copy, nothing to report, and no
// directory made for nothing.
func TestLogSnapshotWithNoLog(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, created, err := snapshotLog(store, backupDir(dir), "2026-08-21")
	if err != errNoLog || created {
		t.Fatalf("no log: created=%v err=%v, want errNoLog", created, err)
	}
	if _, err := os.Stat(backupDir(dir)); !os.IsNotExist(err) {
		t.Error("backups/ was created with nothing to put in it")
	}
	if latestSnapshot(backupDir(dir)) != "" {
		t.Error("latest snapshot reported where there is no directory")
	}
}

// 00:05 on the next local wall-clock day, across both DST changes: the
// fall-back night is 25 hours long and the spring-forward night 23, and
// the snapshot lands at 00:05 on the clock either way.
func TestNextBackupAt(t *testing.T) {
	loc := chicago(t)
	cases := []struct {
		now  time.Time
		want time.Time
	}{
		{time.Date(2026, 8, 21, 13, 0, 0, 0, loc), time.Date(2026, 8, 22, 0, 5, 0, 0, loc)},
		{time.Date(2026, 8, 22, 0, 5, 0, 0, loc), time.Date(2026, 8, 23, 0, 5, 0, 0, loc)},
		{time.Date(2026, 8, 22, 0, 4, 59, 0, loc), time.Date(2026, 8, 23, 0, 5, 0, 0, loc)},
		// DST ends 1 Nov 2026: the night before is 25 hours long.
		{time.Date(2026, 10, 31, 23, 30, 0, 0, loc), time.Date(2026, 11, 1, 0, 5, 0, 0, loc)},
		{time.Date(2026, 11, 1, 12, 0, 0, 0, loc), time.Date(2026, 11, 2, 0, 5, 0, 0, loc)},
		// DST starts 14 Mar 2027: the night before is 23 hours long.
		{time.Date(2027, 3, 13, 12, 0, 0, 0, loc), time.Date(2027, 3, 14, 0, 5, 0, 0, loc)},
	}
	for _, c := range cases {
		got := nextBackupAt(c.now)
		if !got.Equal(c.want) {
			t.Errorf("nextBackupAt(%s) = %s, want %s", c.now, got, c.want)
		}
		if got.Hour() != 0 || got.Minute() != 5 {
			t.Errorf("nextBackupAt(%s) = %s, not 00:05 on the wall clock", c.now, got)
		}
	}

	// Zones whose DST gap sits at midnight: 00:05 on the transition date
	// does not exist, and the first cut of this code let Go resolve it to
	// 23:05 the evening before — the timer fired on a day already
	// snapshotted, and the transition day never got one. The snapshot must
	// land on the NEXT calendar date, whatever hour that date starts at.
	for _, c := range []struct{ zone, now, wantDate string }{
		{"America/Santiago", "2026-09-05 12:30", "2026-09-06"},
		{"America/Havana", "2026-03-07 12:30", "2026-03-08"},
		{"America/Santiago", "2026-09-06 01:02", "2026-09-07"},
	} {
		z, err := time.LoadLocation(c.zone)
		if err != nil {
			t.Fatal(err)
		}
		now, err := time.ParseInLocation("2006-01-02 15:04", c.now, z)
		if err != nil {
			t.Fatal(err)
		}
		got := nextBackupAt(now)
		if d := got.Format("2006-01-02"); d != c.wantDate {
			t.Errorf("%s from %s: next snapshot dated %s, want %s (%s)", c.zone, c.now, d, c.wantDate, got)
		}
		if !got.After(now) {
			t.Errorf("%s from %s: next %s is not after now", c.zone, c.now, got)
		}
		// And the day after is an ordinary one: exactly one snapshot per
		// date, no date skipped, no date doubled.
		again := nextBackupAt(got)
		if again.Format("2006-01-02") != got.AddDate(0, 0, 1).Format("2006-01-02") {
			t.Errorf("%s: after %s the next is %s, not the following date", c.zone, got, again)
		}
	}
	// The fold is at 02:00, so the long day is the one that STARTS on the
	// change date: from 00:05 on 1 Nov to 00:05 on 2 Nov is 25 hours, and
	// from 00:05 on 14 Mar to 00:05 on 15 Mar is 23. The timer is set from
	// wall-clock arithmetic, not from adding 24h.
	fall := nextBackupAt(time.Date(2026, 11, 1, 0, 5, 0, 0, loc)).Sub(time.Date(2026, 11, 1, 0, 5, 0, 0, loc))
	if fall != 25*time.Hour {
		t.Errorf("fall-back wait %s, want 25h", fall)
	}
	spring := nextBackupAt(time.Date(2027, 3, 14, 0, 5, 0, 0, loc)).Sub(time.Date(2027, 3, 14, 0, 5, 0, 0, loc))
	if spring != 23*time.Hour {
		t.Errorf("spring-forward wait %s, want 23h", spring)
	}
}

// Snapshots are the log's kin, not the plan's: the data Rev and the reload
// fingerprint must both be byte-identical before and after backups/ fills,
// and the load must still succeed. Runs dataless, against the embedded
// defaults, because the property is the loader's and not the plan's.
func TestLogSnapshotsAreInvisibleToTheDataRev(t *testing.T) {
	dir := t.TempDir()
	before, err := loadDataset(dir, time.UTC)
	if err != nil {
		t.Fatalf("load without snapshots: %v", err)
	}
	fpBefore := fingerprint(dir)

	store := snapshotFixture(t, dir)
	for _, d := range []string{"2026-08-20", "2026-08-21"} {
		if _, _, err := snapshotLog(store, backupDir(dir), d); err != nil {
			t.Fatal(err)
		}
	}
	// A write stranded mid-copy, too.
	if err := os.WriteFile(filepath.Join(backupDir(dir), "entries-2026-08-22.jsonl.99.tmp"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	after, err := loadDataset(dir, time.UTC)
	if err != nil {
		t.Fatalf("the backups directory broke the load: %v", err)
	}
	if after.Rev != before.Rev {
		t.Errorf("data Rev moved: %s -> %s — log snapshots must be invisible to it", before.Rev, after.Rev)
	}
	if fp := fingerprint(dir); fp != fpBefore {
		t.Errorf("fingerprint moved: %s -> %s — the reload poller would churn on every snapshot", fpBefore, fp)
	}
}

// /healthz reports the newest snapshot on disk — the date make verify
// checks — and "" when there is none.
func TestHealthzReportsTheNewestSnapshot(t *testing.T) {
	dir := t.TempDir()
	ts := fitTestMuxServer(t, dir)
	// The helper's store lives elsewhere; the binary's is in the data dir,
	// and the snapshot is of that one.
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ts.s.store = store

	health := func() string {
		rec := httptest.NewRecorder()
		ts.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("healthz %d: %s", rec.Code, rec.Body)
		}
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		v, ok := m["backup"]
		if !ok {
			t.Fatal("healthz carries no backup field")
		}
		s, _ := v.(string)
		return s
	}

	ts.s.backupOnce() // no log yet
	if got := health(); got != "" {
		t.Errorf("backup %q before any log exists, want \"\"", got)
	}
	if err := store.Append(Entry{Date: "2026-08-21", Kind: "task", Key: "t", Val: "done"}); err != nil {
		t.Fatal(err)
	}
	ts.s.backupOnce()
	today := ts.s.day(ts.s.ds()).Format("2006-01-02")
	if got := health(); got != today {
		t.Errorf("backup %q after the startup snapshot, want today %s", got, today)
	}
	if got := readOrFail(t, backupPath(backupDir(dir), today)); !strings.Contains(string(got), `"key":"t"`) {
		t.Error("the snapshot the server took is not the log")
	}
}

// The headline property, pinned directly: the copy waits for the store's
// lock. With the lock held, SnapshotTo does not return; released, it
// completes with the log's bytes. -race cannot see this — file I/O is not
// instrumented — so it is asserted by blocking rather than by detection.
func TestLogSnapshotWaitsForTheStoresLock(t *testing.T) {
	dir := t.TempDir()
	store := snapshotFixture(t, dir)
	bdir := backupDir(dir)
	if err := os.MkdirAll(bdir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := backupPath(bdir, "2026-08-21")

	store.mu.Lock()
	done := make(chan error, 1)
	go func() { done <- store.SnapshotTo(path) }()
	select {
	case err := <-done:
		t.Fatalf("SnapshotTo returned (%v) while the store's lock was held", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("the snapshot was published while the lock was held")
	}
	store.mu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SnapshotTo after release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SnapshotTo did not complete once the lock was released")
	}
	if got, want := readOrFail(t, path), readOrFail(t, filepath.Join(dir, "entries.jsonl")); string(got) != string(want) {
		t.Error("snapshot taken after the release is not the log")
	}
}

// Under a stream of appends, every snapshot is a whole number of whole
// lines: it ends in a newline and every line parses. A torn copy — the
// last line cut mid-JSON — is exactly what copying without the lock would
// produce, and what a cron beside the app could never rule out.
func TestLogSnapshotUnderAppendsIsWholeLines(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(Entry{Date: "2026-08-20", Kind: "task", Key: "seed", Val: "done"}); err != nil {
		t.Fatal(err)
	}
	bdir := backupDir(dir)
	if err := os.MkdirAll(bdir, 0o755); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			// A long note so a torn line would be obvious and likely.
			_ = store.Append(Entry{Date: "2026-08-21", Kind: "note", Key: fmt.Sprint(i),
				Note: strings.Repeat("the quick brown fox jumps over the lazy dog ", 40)})
		}
	}()

	const snaps = 25
	for i := 0; i < snaps; i++ {
		if err := store.SnapshotTo(backupPath(bdir, fmt.Sprintf("2026-01-%02d", i+1))); err != nil {
			t.Fatalf("snapshot %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()

	prev := -1
	for i := 0; i < snaps; i++ {
		b := readOrFail(t, backupPath(bdir, fmt.Sprintf("2026-01-%02d", i+1)))
		if len(b) == 0 || b[len(b)-1] != '\n' {
			t.Fatalf("snapshot %d does not end in a newline: torn copy", i)
		}
		lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
		for n, l := range lines {
			var e Entry
			if err := json.Unmarshal([]byte(l), &e); err != nil {
				t.Fatalf("snapshot %d line %d does not parse: %v", i, n+1, err)
			}
		}
		if len(lines) < prev {
			t.Fatalf("snapshot %d has %d lines, fewer than the previous %d: the log is append-only", i, len(lines), prev)
		}
		prev = len(lines)
	}
	if prev < 2 {
		t.Fatalf("the appender never got a line in (%d lines at the end) — the test proved nothing", prev)
	}
}

// The snapshot is dated by the ATHLETE's day, the one in athlete.json, not
// the host's and not the server's fallback flag. Two zones 26 hours apart:
// at any instant at most one of them shares the host's date, so a snapshot
// dated by anything but the athlete's own clock is caught whenever it runs.
func TestLogSnapshotIsDatedByTheAthletesDay(t *testing.T) {
	def, err := os.ReadFile("defaults/athlete.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, zone := range []string{"Etc/GMT-14", "Etc/GMT+12"} {
		t.Run(zone, func(t *testing.T) {
			dir := t.TempDir()
			var a map[string]any
			if err := json.Unmarshal(def, &a); err != nil {
				t.Fatal(err)
			}
			a["timezone"] = zone
			b, err := json.Marshal(a)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "athlete.json"), append(b, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
			ts := fitTestMuxServer(t, dir) // server.loc is Chicago: the wrong clock, deliberately
			store, err := OpenStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			ts.s.store = store
			if err := store.Append(Entry{Date: "2026-08-21", Kind: "task", Key: "t", Val: "done"}); err != nil {
				t.Fatal(err)
			}
			loc, err := time.LoadLocation(zone)
			if err != nil {
				t.Fatal(err)
			}
			if ts.s.ds().Loc.String() != loc.String() {
				t.Fatalf("dataset loc %s, want %s — the fixture did not take", ts.s.ds().Loc, loc)
			}
			before := time.Now().In(loc).Format("2006-01-02")
			ts.s.backupOnce()
			after := time.Now().In(loc).Format("2006-01-02")
			dates, err := snapshotDates(backupDir(dir))
			if err != nil {
				t.Fatal(err)
			}
			if len(dates) != 1 || (dates[0] != before && dates[0] != after) {
				t.Errorf("snapshot dated %v, want the athlete's day in %s (%s)", dates, zone, before)
			}
		})
	}
}
