package main

// The metrics database: a DERIVED, DISPOSABLE cache of the register's
// output over the activity archive, at <data>/metrics.db. The archive's FIT
// bytes are canonical and immutable; every row here is rebuildable from
// them in seconds, which is the whole design: a schema change is a version
// bump and a rebuild, never a migration script, and deleting the file is
// always safe. The canonical bytes NEVER move into the database.
//
// The load-bearing trick is the time-at-value histograms: seconds spent at
// each exact bpm and watt per activity, so ANY threshold — a changed grade
// cap, retested FTP zones — is exact SQL later, verified diff-0 against
// direct stream computation. hr_hist stores every sample including <50 bpm;
// the dropout rule applies at query time.
//
// metrics.db and its WAL sidecars are non-.json, so they are invisible to
// the data Rev and the reload fingerprint by the same rule that hides the
// activity archive: an import can never rotate workout identities, trigger
// a reload, or fail `make verify`.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// metricsSchemaVersion gates the whole file: a mismatch on open drops every
// derived table and rebuilds from the archive via the startup reconcile.
// Bump it whenever the schema OR the register's arithmetic changes shape.
const metricsSchemaVersion = 1

const metricsSchema = `
CREATE TABLE IF NOT EXISTS activities(
  name TEXT PRIMARY KEY, date TEXT NOT NULL, sport TEXT NOT NULL,
  start_utc TEXT NOT NULL, elapsed_s INTEGER NOT NULL, records INTEGER NOT NULL,
  avg_hr REAL, max_hr INTEGER, dropout_share REAL, hr_drift REAL,
  decoupling_pct REAL, avg_cadence REAL, avg_power REAL, distance_m REAL,
  sha256 TEXT NOT NULL, schema_version INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS idx_act_sport_date ON activities(sport, date);
CREATE TABLE IF NOT EXISTS hr_hist(
  name TEXT NOT NULL, bpm INTEGER NOT NULL, seconds INTEGER NOT NULL,
  PRIMARY KEY(name, bpm)) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS power_hist(
  name TEXT NOT NULL, watts INTEGER NOT NULL, seconds INTEGER NOT NULL,
  PRIMARY KEY(name, watts)) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS failures(
  name TEXT PRIMARY KEY, error TEXT NOT NULL, at TEXT NOT NULL);
`

type metricsDB struct {
	w  *sql.DB // single writer connection
	r  *sql.DB // concurrent readers (WAL)
	mu sync.Mutex
}

// openMetricsDB opens (or creates) the cache. A file that cannot be opened
// or migrated is deleted and recreated — it is derived data, and a corrupt
// cache must never keep the server down when the canonical bytes are right
// there to rebuild from.
func openMetricsDB(dir string) (*metricsDB, error) {
	path := filepath.Join(dir, "metrics.db")
	m, err := openMetricsDBAt(path)
	if err == nil {
		return m, nil
	}
	log.Printf("metrics: %s unusable (%v) — deleting and rebuilding the derived cache", path, err)
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		_ = os.Remove(p)
	}
	return openMetricsDBAt(path)
}

func openMetricsDBAt(path string) (*metricsDB, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)"
	w, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	var version int
	if err := w.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		w.Close()
		return nil, err
	}
	if version != metricsSchemaVersion {
		if version != 0 {
			log.Printf("metrics: schema v%d on disk, this build is v%d — dropping derived tables for rebuild",
				version, metricsSchemaVersion)
		}
		for _, t := range []string{"activities", "hr_hist", "power_hist", "failures"} {
			if _, err := w.Exec(`DROP TABLE IF EXISTS ` + t); err != nil {
				w.Close()
				return nil, err
			}
		}
		if _, err := w.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, metricsSchemaVersion)); err != nil {
			w.Close()
			return nil, err
		}
	}
	if _, err := w.Exec(metricsSchema); err != nil {
		w.Close()
		return nil, err
	}
	r, err := sql.Open("sqlite", dsn)
	if err != nil {
		w.Close()
		return nil, err
	}
	return &metricsDB{w: w, r: r}, nil
}

func (m *metricsDB) close() {
	if m == nil {
		return
	}
	_ = m.r.Close()
	_ = m.w.Close()
}

var activityDatePrefix = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}`)

// activityDate is the training day an activity belongs to: the device name
// leads (names are the athlete's local time, verified 13 Aug 2026), the
// recording's start converted to the athlete's timezone is the fallback.
func activityDate(name string, startUTC time.Time, loc *time.Location) string {
	if p := activityDatePrefix.FindString(name); p != "" {
		if _, err := time.Parse("2006-01-02", p); err == nil {
			return p
		}
	}
	return startUTC.In(loc).Format("2006-01-02")
}

// importOne decodes one stored activity and lands its metrics in a single
// transaction. Idempotent: a re-import replaces the row and its histograms.
// Errors are recorded in the failures table AND returned — the caller
// decides whether they matter (an HTTP import logs and moves on; the
// canonical bytes are already safe on disk either way). A decoder panic on
// hostile bytes is contained here for the same reason.
func (m *metricsDB) importOne(name string, data []byte, loc *time.Location) (a *activityMetrics, err error) {
	defer func() {
		if r := recover(); r != nil {
			a, err = nil, fmt.Errorf("decode panic: %v", r)
		}
		if err != nil {
			m.recordFailure(name, err)
		}
	}()

	streams, err := decodeActivity(data)
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	sum := sha256.Sum256(data)
	a = computeMetrics(name, activityDate(name, streams.StartUTC, loc), streams)
	a.SHA256 = hex.EncodeToString(sum[:])

	m.mu.Lock()
	defer m.mu.Unlock()
	tx, err := m.w.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for _, del := range []string{`DELETE FROM hr_hist WHERE name=?`, `DELETE FROM power_hist WHERE name=?`} {
		if _, err := tx.Exec(del, name); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO activities VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.Name, a.Date, a.Sport, a.StartUTC, a.ElapsedS, a.Records,
		a.AvgHR, a.MaxHR, a.DropoutShare, a.HRDrift, a.DecouplingPct,
		a.AvgCadence, a.AvgPower, a.DistanceM, a.SHA256, metricsSchemaVersion); err != nil {
		return nil, err
	}
	insHR, err := tx.Prepare(`INSERT INTO hr_hist VALUES(?,?,?)`)
	if err != nil {
		return nil, err
	}
	for bpm, sec := range a.HRHist {
		if _, err := insHR.Exec(name, bpm, sec); err != nil {
			return nil, err
		}
	}
	insPW, err := tx.Prepare(`INSERT INTO power_hist VALUES(?,?,?)`)
	if err != nil {
		return nil, err
	}
	for w, sec := range a.PowerHist {
		if _, err := insPW.Exec(name, w, sec); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(`DELETE FROM failures WHERE name=?`, name); err != nil {
		return nil, err
	}
	return a, tx.Commit()
}

// recent lists stored activities on or after a date, oldest first — the
// grader's startup reconcile walks these looking for ungraded days.
func (m *metricsDB) recent(sinceDate string) ([]activityMetrics, error) {
	rows, err := m.r.Query(`SELECT name, date, sport FROM activities
		WHERE date >= ? ORDER BY name`, sinceDate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []activityMetrics
	for rows.Next() {
		var a activityMetrics
		if err := rows.Scan(&a.Name, &a.Date, &a.Sport); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (m *metricsDB) recordFailure(name string, cause error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.w.Exec(`INSERT OR REPLACE INTO failures VALUES(?,?,?)`,
		name, cause.Error(), time.Now().UTC().Format(time.RFC3339)); err != nil {
		log.Printf("metrics: could not record failure for %s: %v", name, err)
	}
}

func (m *metricsDB) has(name string) (bool, error) {
	var one int
	err := m.r.QueryRow(`SELECT 1 FROM activities WHERE name=?`, name).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// reconcile imports every archived file the DB does not hold — which on a
// fresh or version-bumped DB is the entire archive: the initial backfill IS
// the reconcile. Permanent decode failures retry each pass (12 ms a piece)
// and stay visible in the failures table rather than wedging anything.
func (m *metricsDB) reconcile(dir string, loc *time.Location) {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return // no archive yet — nothing to reconcile
	}
	if err != nil {
		log.Printf("metrics reconcile: %v", err)
		return
	}
	start := time.Now()
	imported, failed, present := 0, 0, 0
	for _, e := range ents {
		if e.IsDir() || !validActivityName(e.Name()) {
			continue
		}
		ok, err := m.has(e.Name())
		if err != nil {
			log.Printf("metrics reconcile: %s: %v", e.Name(), err)
			return
		}
		if ok {
			present++
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			log.Printf("metrics reconcile: %s: %v", e.Name(), err)
			failed++
			continue
		}
		if _, err := m.importOne(e.Name(), data, loc); err != nil {
			log.Printf("metrics reconcile: %s: %v", e.Name(), err)
			failed++
			continue
		}
		imported++
	}
	if imported > 0 || failed > 0 {
		log.Printf("metrics reconcile: %d imported, %d failed, %d already present in %v",
			imported, failed, present, time.Since(start).Round(time.Millisecond))
	}
}

/* ── queries ───────────────────────────────────────────────────────────── */

// rowByName returns the stored aggregate row, nil when the activity has no
// row yet (not stored, or its import failed — the failures table knows).
func (m *metricsDB) rowByName(name string) (*activityMetrics, error) {
	a := &activityMetrics{}
	err := m.r.QueryRow(`SELECT name, date, sport, start_utc, elapsed_s, records,
		avg_hr, max_hr, dropout_share, hr_drift, decoupling_pct,
		avg_cadence, avg_power, distance_m, sha256
		FROM activities WHERE name=?`, name).Scan(
		&a.Name, &a.Date, &a.Sport, &a.StartUTC, &a.ElapsedS, &a.Records,
		&a.AvgHR, &a.MaxHR, &a.DropoutShare, &a.HRDrift, &a.DecouplingPct,
		&a.AvgCadence, &a.AvgPower, &a.DistanceM, &a.SHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return a, nil
}

// underCapShareSQL is the run legend's number straight from the histogram:
// valid time (≥50 bpm) at or under the cap over all valid time. Verified
// diff-0 against the direct stream computation — the check the test suite
// pins at several caps.
func (m *metricsDB) underCapShareSQL(name string, cap int) (*float64, error) {
	var share sql.NullFloat64
	// ELSE 0 on the numerator: a cap under every valid sample is a share of
	// zero, not a NULL — the stream computation says 0.0 and diff-0 means 0.
	// The denominator keeps its NULL: no valid HR time at all is "no share".
	err := m.r.QueryRow(`
		SELECT CAST(SUM(CASE WHEN bpm BETWEEN 50 AND ? THEN seconds ELSE 0 END) AS REAL)
		     / SUM(CASE WHEN bpm >= 50 THEN seconds END)
		FROM hr_hist WHERE name=?`, cap, name).Scan(&share)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !share.Valid) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &share.Float64, nil
}

// failureFor reports the recorded import failure for a name, "" when none.
func (m *metricsDB) failureFor(name string) (string, error) {
	var msg string
	err := m.r.QueryRow(`SELECT error FROM failures WHERE name=?`, name).Scan(&msg)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return msg, err
}
