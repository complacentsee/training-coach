package main

// Permanent, defaults-safe pins for the activity decode and the register:
// the two measured wire warts (Zwift's whole-file CRC convention, the
// explicit-speed-vs-expansion precedence), the histogram/direct diff-0 law,
// the metrics DB's invisibility to the data Rev, and the import API's
// contract. Fixtures are synthesized with muktihari's encoder rather than
// committed, so every test states exactly the bytes it relies on and the
// suite passes in a dataless clone. The whole-archive cross-check against
// the python mirrors lives in acceptgate_test.go and needs the archive.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muktihari/fit/encoder"
	"github.com/muktihari/fit/profile/factory"
	"github.com/muktihari/fit/profile/mesgdef"
	"github.com/muktihari/fit/profile/typedef"
	"github.com/muktihari/fit/profile/untyped/fieldnum"
	"github.com/muktihari/fit/profile/untyped/mesgnum"
	"github.com/muktihari/fit/proto"
)

var fixtureT0 = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

func encodeActivityFixture(t *testing.T, msgs ...proto.Message) []byte {
	t.Helper()
	all := []proto.Message{
		mesgdef.NewFileId(nil).
			SetType(typedef.FileActivity).
			SetManufacturer(typedef.ManufacturerDevelopment).
			SetTimeCreated(fixtureT0).ToMesg(nil),
	}
	all = append(all, msgs...)
	var buf bytes.Buffer
	if err := encoder.New(&buf).Encode(&proto.FIT{Messages: all}); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return buf.Bytes()
}

// runRecord is one second of running: hr in bpm, speed raw (scale 1000
// m/s), cadence per-leg rpm.
func runRecord(sec int, hr uint8, speedRaw uint16, cad uint8) proto.Message {
	return mesgdef.NewRecord(nil).
		SetTimestamp(fixtureT0.Add(time.Duration(sec) * time.Second)).
		SetHeartRate(hr).
		SetSpeed(speedRaw).
		SetCadence(cad).ToMesg(nil)
}

func sessionMsg(sport typedef.Sport, distCM uint32) proto.Message {
	return mesgdef.NewSession(nil).
		SetSport(sport).
		SetStartTime(fixtureT0).
		SetTimestamp(fixtureT0.Add(time.Minute)).
		SetTotalDistance(distCM).ToMesg(nil)
}

// withCompressedSpeed appends a raw compressed_speed_distance field: 12-bit
// speed at scale 100 m/s, 12-bit accumulated distance at scale 16 m — the
// 2018-era encoding whose component expansion must never beat an explicit
// speed field.
func withCompressedSpeed(msg proto.Message, speedCM, dist12 uint16) proto.Message {
	f := factory.CreateField(mesgnum.Record, fieldnum.RecordCompressedSpeedDistance)
	f.Value = proto.SliceUint8([]uint8{
		uint8(speedCM & 0xFF),
		uint8((speedCM>>8)&0x0F) | uint8((dist12&0x0F)<<4),
		uint8(dist12 >> 4),
	})
	msg.Fields = append(msg.Fields, f)
	return msg
}

// tenSecondRun is the shared fixture: eleven records, HR climbing 120→130,
// 3.0 m/s, cadence 80, a running session of 100 m.
func tenSecondRun(t *testing.T) []byte {
	t.Helper()
	msgs := make([]proto.Message, 0, 12)
	for i := 0; i <= 10; i++ {
		msgs = append(msgs, runRecord(i, uint8(120+i), 3000, 80))
	}
	msgs = append(msgs, sessionMsg(typedef.SportRunning, 100_00))
	return encodeActivityFixture(t, msgs...)
}

func TestDecodeActivityFixture(t *testing.T) {
	s, err := decodeActivity(tenSecondRun(t))
	if err != nil {
		t.Fatal(err)
	}
	if s.Sport != "running" {
		t.Errorf("sport = %q", s.Sport)
	}
	if !s.StartUTC.Equal(fixtureT0) {
		t.Errorf("start = %v", s.StartUTC)
	}
	if len(s.Time) != 11 || s.Time[10] != 10 {
		t.Fatalf("time = %v", s.Time)
	}
	if !s.HaveHR || s.HR[0] != 120 || s.HR[10] != 130 {
		t.Errorf("hr = %v", s.HR)
	}
	if !s.HaveVel || s.Vel[5] != 3.0 {
		t.Errorf("vel = %v", s.Vel)
	}
	if !s.HaveCad || s.Cad[3] != 80 {
		t.Errorf("cad = %v", s.Cad)
	}
	if s.HaveWatts {
		t.Error("watts stream from a powerless file")
	}
	if s.DistM == nil || *s.DistM != 100 {
		t.Errorf("dist = %v", s.DistM)
	}

	m := computeMetrics("2026-08-01-12-00-00.fit", "2026-08-01", s)
	if m.ElapsedS != 10 || m.Records != 11 {
		t.Errorf("elapsed=%d records=%d", m.ElapsedS, m.Records)
	}
	// Time-weighted: sample i covers t[i]−t[i−1], so the mean is over
	// samples 1..10 = 121..130.
	if m.AvgHR == nil || *m.AvgHR != 125.5 {
		t.Errorf("avg hr = %v", m.AvgHR)
	}
	if m.MaxHR == nil || *m.MaxHR != 130 {
		t.Errorf("max hr = %v", m.MaxHR)
	}
	if m.DropoutShare == nil || *m.DropoutShare != 0 {
		t.Errorf("dropout = %v", m.DropoutShare)
	}
	// Halves split at t=5: first-half samples 121..125, second 127..130
	// (126 opens the second list, so it weights nothing) → 128.5 − 123.
	if m.HRDrift == nil || *m.HRDrift != 5.5 {
		t.Errorf("drift = %v", m.HRDrift)
	}
	total := 0
	for _, sec := range m.HRHist {
		total += sec
	}
	if total != 10 || m.HRHist[125] != 1 {
		t.Errorf("hist = %v", m.HRHist)
	}
}

// TestZwiftCRCConventionAccepted rewrites the fixture the way Zwift writes
// files — header CRC slot zero, trailing CRC spanning header+data — and
// demands it decode identically to the untouched bytes.
func TestZwiftCRCConventionAccepted(t *testing.T) {
	clean := tenSecondRun(t)
	want, err := decodeActivity(clean)
	if err != nil {
		t.Fatal(err)
	}

	z := bytes.Clone(clean)
	z[12], z[13] = 0, 0
	crc := fitCRC16(0, z[:len(z)-2])
	z[len(z)-2], z[len(z)-1] = byte(crc), byte(crc>>8)

	got, err := decodeActivity(z)
	if err != nil {
		t.Fatalf("zwift-convention file refused: %v", err)
	}
	if len(got.Time) != len(want.Time) || got.HR[10] != want.HR[10] {
		t.Errorf("zwift-convention decode differs from the clean one")
	}
}

// TestCorruptByteRefused pins the refusal side: a flipped data byte fails
// muktihari's CRC AND the whole-file convention, so it must surface as a
// checksum error, never as streams.
func TestCorruptByteRefused(t *testing.T) {
	b := tenSecondRun(t)
	b[len(b)/2] ^= 0xFF
	if _, err := decodeActivity(b); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("corrupted file decoded, err = %v", err)
	}
}

// TestExplicitSpeedBeatsExpansion pins the 2018-era precedence: a record
// carrying both an explicit speed and compressed_speed_distance keeps the
// explicit value; a record with only the compressed form falls back to the
// expansion.
func TestExplicitSpeedBeatsExpansion(t *testing.T) {
	both := withCompressedSpeed(runRecord(0, 120, 3000, 80), 290, 0) // explicit 3.0, expansion 2.9
	only := mesgdef.NewRecord(nil).
		SetTimestamp(fixtureT0.Add(time.Second)).
		SetHeartRate(121).ToMesg(nil)
	only = withCompressedSpeed(only, 250, 16) // expansion 2.5, no explicit field

	s, err := decodeActivity(encodeActivityFixture(t, both, only, sessionMsg(typedef.SportRunning, 10_00)))
	if err != nil {
		t.Fatal(err)
	}
	if !s.HaveVel {
		t.Fatal("no velocity stream")
	}
	if s.Vel[0] != 3.0 {
		t.Errorf("explicit speed lost to expansion: vel[0] = %v, want 3.0", s.Vel[0])
	}
	if s.Vel[1] != 2.5 {
		t.Errorf("expansion fallback: vel[1] = %v, want 2.5", s.Vel[1])
	}
}

// TestExplicitSpeedBeatsSynthesizedEnhanced pins the other measured
// expansion wart: a record carrying ONLY an explicit speed field gets an
// enhanced_speed synthesized by component expansion, truncated an ulp low
// (raw 2006 → 2005). The explicit wire value must win: 2.006, never 2.005.
func TestExplicitSpeedBeatsSynthesizedEnhanced(t *testing.T) {
	msgs := []proto.Message{}
	for i := 0; i <= 3; i++ {
		msgs = append(msgs, runRecord(i, uint8(120+i), 2006, 80))
	}
	msgs = append(msgs, sessionMsg(typedef.SportRunning, 10_00))
	s, err := decodeActivity(encodeActivityFixture(t, msgs...))
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range s.Vel {
		if v != 2.006 {
			t.Fatalf("vel[%d] = %v, want 2.006 (synthesized enhanced_speed beat the explicit field)", i, v)
		}
	}
}

// TestNoHRFile: a strapless activity produces no HR stream, no HR metrics,
// and an empty histogram — never a wall of zeros.
func TestNoHRFile(t *testing.T) {
	msgs := []proto.Message{}
	for i := 0; i <= 5; i++ {
		msgs = append(msgs, mesgdef.NewRecord(nil).
			SetTimestamp(fixtureT0.Add(time.Duration(i)*time.Second)).
			SetSpeed(2500).ToMesg(nil))
	}
	msgs = append(msgs, sessionMsg(typedef.SportRunning, 15_00))
	s, err := decodeActivity(encodeActivityFixture(t, msgs...))
	if err != nil {
		t.Fatal(err)
	}
	if s.HaveHR {
		t.Fatal("HR stream from a strapless file")
	}
	m := computeMetrics("x.fit", "2026-08-01", s)
	if m.AvgHR != nil || m.DropoutShare != nil || len(m.HRHist) != 0 {
		t.Errorf("HR metrics from a strapless file: %+v", m)
	}
}

// TestHistogramMatchesDirectShares is the diff-0 law: the under-cap share
// read back from hr_hist by SQL equals the direct stream computation
// exactly, at several caps, dropouts included.
func TestHistogramMatchesDirectShares(t *testing.T) {
	hrs := []uint8{40, 45, 60, 120, 150, 155, 157, 158, 170, 185, 60, 45}
	msgs := make([]proto.Message, 0, len(hrs)+1)
	for i, h := range hrs {
		msgs = append(msgs, runRecord(i, h, 3000, 80))
	}
	msgs = append(msgs, sessionMsg(typedef.SportRunning, 30_00))
	data := encodeActivityFixture(t, msgs...)

	s, err := decodeActivity(data)
	if err != nil {
		t.Fatal(err)
	}
	db, err := openMetricsDB(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.close()
	const name = "2026-08-01-12-00-00.fit"
	if _, err := db.importOne(name, data, time.UTC); err != nil {
		t.Fatal(err)
	}
	for _, cap := range []int{50, 140, 157, 200} {
		sql, err := db.underCapShareSQL(name, cap)
		if err != nil {
			t.Fatal(err)
		}
		direct := runGradeShare(s, cap)
		switch {
		case (sql == nil) != (direct == nil):
			t.Errorf("cap %d: sql %v, direct %v", cap, sql, direct)
		case sql != nil && *sql != *direct:
			t.Errorf("cap %d: sql %v ≠ direct %v (diff must be exactly 0)", cap, *sql, *direct)
		}
	}
}

// TestMetricsDBInvisibleToRevAndFingerprint: the derived cache and its WAL
// sidecars can never perturb a reload or rotate workout identities, by the
// same rule that hides the activity archive.
func TestMetricsDBInvisibleToRevAndFingerprint(t *testing.T) {
	dir := t.TempDir()
	d1, err := loadDataset(dir, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	fp1 := fingerprint(dir)

	db, err := openMetricsDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.importOne("2026-08-01-12-00-00.fit", tenSecondRun(t), time.UTC); err != nil {
		t.Fatal(err)
	}
	db.close()
	for _, side := range []string{"metrics.db-wal", "metrics.db-shm"} {
		// close() may checkpoint the sidecars away; the fingerprint must
		// ignore them present or not, so pin the present case explicitly.
		if err := os.WriteFile(filepath.Join(dir, side), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	d2, err := loadDataset(dir, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if d1.Rev != d2.Rev {
		t.Errorf("metrics.db rotated the data rev: %s → %s", d1.Rev, d2.Rev)
	}
	if fp1 != fingerprint(dir) {
		t.Error("metrics.db perturbed the reload fingerprint")
	}
}

// TestActivityMetricsAPI drives the whole pipeline over the embedded
// defaults: import lands metrics, the endpoint serves them against the
// anchors the defaults DO declare, and omits what they do not — the
// defaults athlete deliberately has no gradeCap, so a hardcoded anchor
// fails here.
func TestActivityMetricsAPI(t *testing.T) {
	mux := fitTestMux(t, t.TempDir())
	const name = "2026-08-01-12-00-00.fit"
	if rec := post(mux, "/api/activity?name="+name, tenSecondRun(t)); rec.Code != http.StatusNoContent {
		t.Fatalf("POST = %d: %s", rec.Code, rec.Body)
	}
	rec := get(mux, "/api/activity-metrics?name="+name, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d: %s", rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["sport"] != "running" || out["date"] != "2026-08-01" {
		t.Errorf("sport/date: %v/%v", out["sport"], out["date"])
	}
	hr, _ := out["hr"].(map[string]any)
	if hr == nil || hr["avg"] != 125.5 {
		t.Errorf("hr = %v", out["hr"])
	}
	if out["cadence"] != 160.0 { // per-leg 80, doubled at presentation for a run
		t.Errorf("cadence = %v", out["cadence"])
	}
	if _, ok := out["grade_input"]; ok {
		t.Error("grade_input against an athlete who declares no gradeCap")
	}
	f20, _ := out["first_20min"].(map[string]any)
	if f20 == nil || f20["cap"] != 140.0 || f20["avg"] != 125.5 {
		t.Errorf("first_20min = %v", out["first_20min"])
	}

	if rec := get(mux, "/api/activity-metrics?name=../x.fit", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("traversal name = %d", rec.Code)
	}
	if rec := get(mux, "/api/activity-metrics?name=2026-01-01-00-00-00.fit", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown name = %d", rec.Code)
	}

	// A stored file that will not decode: the import still 204s (bytes on
	// disk is the contract), and the metrics 404 says why.
	junk := "2026-08-02-12-00-00.fit"
	if rec := post(mux, "/api/activity?name="+junk, fitBytes("not really fit")); rec.Code != http.StatusNoContent {
		t.Fatalf("junk POST = %d", rec.Code)
	}
	rec = get(mux, "/api/activity-metrics?name="+junk, nil)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "import failed") {
		t.Errorf("junk GET = %d %q", rec.Code, rec.Body.String())
	}
}

// TestReconcileImportsTheArchive: files already on disk when the server
// starts get their metrics on the reconcile pass, and a broken file lands
// in failures without wedging the rest.
func TestReconcileImportsTheArchive(t *testing.T) {
	dir := t.TempDir()
	actDir := filepath.Join(dir, "activities")
	if err := os.MkdirAll(actDir, 0o755); err != nil {
		t.Fatal(err)
	}
	good := tenSecondRun(t)
	for _, n := range []string{"2026-08-01-12-00-00.fit", "2026-08-02-12-00-00.fit"} {
		if err := os.WriteFile(filepath.Join(actDir, n), good, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(actDir, "2026-08-03-12-00-00.fit"), fitBytes("junk"), 0o644); err != nil {
		t.Fatal(err)
	}

	db, err := openMetricsDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.close()
	db.reconcile(actDir, time.UTC)

	for _, n := range []string{"2026-08-01-12-00-00.fit", "2026-08-02-12-00-00.fit"} {
		if ok, err := db.has(n); err != nil || !ok {
			t.Errorf("%s not reconciled (ok=%v err=%v)", n, ok, err)
		}
	}
	if ok, _ := db.has("2026-08-03-12-00-00.fit"); ok {
		t.Error("junk file grew a metrics row")
	}
	if msg, err := db.failureFor("2026-08-03-12-00-00.fit"); err != nil || msg == "" {
		t.Errorf("junk file's failure not recorded (msg=%q err=%v)", msg, err)
	}
}

// TestSchemaVersionBumpRebuilds: a version mismatch drops the derived
// tables on open — the migration story is a rebuild, never a script.
func TestSchemaVersionBumpRebuilds(t *testing.T) {
	dir := t.TempDir()
	db, err := openMetricsDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	const name = "2026-08-01-12-00-00.fit"
	if _, err := db.importOne(name, tenSecondRun(t), time.UTC); err != nil {
		t.Fatal(err)
	}
	if _, err := db.w.Exec(`PRAGMA user_version = 9999`); err != nil {
		t.Fatal(err)
	}
	db.close()

	db2, err := openMetricsDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.close()
	if ok, _ := db2.has(name); ok {
		t.Error("stale-versioned rows survived the reopen")
	}
}

// TestDayAPI drives /api/day over the embedded example block: the resolved,
// emphasis-stripped prescription, the legend with its template resolved
// against the defaults athlete, and 400/404 on every miss — never a
// fallback prescription.
func TestDayAPI(t *testing.T) {
	mux := fitTestMux(t, t.TempDir())

	// 2026-01-06 is Tuesday of week 1: the strides day.
	rec := get(mux, "/api/day?date=2026-01-06", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		Date    string   `json:"date"`
		Block   string   `json:"block"`
		Week    int      `json:"week"`
		Tags    []string `json:"week_tags"`
		Session struct {
			Kind    string   `json:"kind"`
			Label   string   `json:"label"`
			Dist    string   `json:"dist"`
			DistM   float64  `json:"dist_m"`
			Detail  string   `json:"detail"`
			Targets []string `json:"targets"`
		} `json:"session"`
		Grading *struct {
			Note  string `json:"note"`
			Bands []struct{ Grade, Range string }
		} `json:"grading"`
		Units string         `json:"units"`
		HR    map[string]int `json:"hr"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Date != "2026-01-06" || out.Block != "example-base-block" || out.Week != 1 {
		t.Errorf("date/block/week: %s/%s/%d", out.Date, out.Block, out.Week)
	}
	if out.Session.Kind != "quality" || out.Session.DistM != 8000 {
		t.Errorf("session: %+v", out.Session)
	}
	if strings.ContainsAny(out.Session.Detail+out.Session.Label, "*_") {
		t.Errorf("emphasis markers leaked: %q", out.Session.Detail)
	}
	if !strings.Contains(out.Session.Detail, "strides") {
		t.Errorf("detail = %q", out.Session.Detail)
	}
	if out.Grading == nil || len(out.Grading.Bands) == 0 {
		t.Fatalf("grading = %+v", out.Grading)
	}
	if strings.Contains(out.Grading.Note, "{{") {
		t.Errorf("legend note unresolved: %q", out.Grading.Note)
	}
	if out.HR["easyCap"] == 0 {
		t.Errorf("hr anchors missing: %v", out.HR)
	}

	if rec := get(mux, "/api/day?date=not-a-date", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad date = %d", rec.Code)
	}
	if rec := get(mux, "/api/day?date=2026-01-06&block=nope", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown block = %d", rec.Code)
	}
	if rec := get(mux, "/api/day?date=2020-01-01", nil); rec.Code != http.StatusNotFound {
		t.Errorf("outside block = %d", rec.Code)
	}
}

// TestActivityDate: the device name is the training day; a name without a
// date defers to the recording's start in the athlete's timezone.
func TestActivityDate(t *testing.T) {
	chi := chicago(t)
	if d := activityDate("2026-08-13-08-47-38.fit", fixtureT0, chi); d != "2026-08-13" {
		t.Errorf("named date = %s", d)
	}
	// 2026-08-01 12:00 UTC is 07:00 in Chicago — same day.
	if d := activityDate("workout.fit", fixtureT0, chi); d != "2026-08-01" {
		t.Errorf("fallback date = %s", d)
	}
	// 2026-08-01 03:00 UTC is the previous evening in Chicago.
	if d := activityDate("workout.fit", time.Date(2026, 8, 1, 3, 0, 0, 0, time.UTC), chi); d != "2026-07-31" {
		t.Errorf("tz fallback date = %s", d)
	}
}
