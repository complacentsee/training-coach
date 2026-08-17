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
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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
	// Halves split at t=5: first-half samples 121..125 → 123, second half
	// 126..130 → 128. Sample 126 covers (5,6] and belongs to the half it
	// sits in — under the old sublist idiom it opened the second list and
	// weighted nothing, which is why this pinned 5.5 until 17 Aug 2026.
	if m.HRDrift == nil || *m.HRDrift != 5.0 {
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

// TestRecordingGapCarriesNoWeight pins the gap rule end to end: eleven
// seconds at HR 130, a 790 s gap (the watch stopped), eleven at HR 150.
// The sample after the gap spans it and must weight NOTHING — under the
// old convention it carried all 790 s, which is how a 12-minute phone
// call once read as "rode too easy".
func TestRecordingGapCarriesNoWeight(t *testing.T) {
	msgs := make([]proto.Message, 0, 24)
	for i := 0; i <= 10; i++ {
		msgs = append(msgs, runRecord(i, 130, 3000, 80))
	}
	for i := 800; i <= 810; i++ {
		msgs = append(msgs, runRecord(i, 150, 3000, 80))
	}
	msgs = append(msgs, sessionMsg(typedef.SportRunning, 100_00))
	body := encodeActivityFixture(t, msgs...)

	st, err := decodeActivity(body)
	if err != nil {
		t.Fatal(err)
	}
	m := computeMetrics("2026-08-01-12-00-00.fit", "2026-08-01", st)
	if m.ElapsedS != 810 || m.MovingS != 20 {
		t.Fatalf("elapsed=%d moving=%d, want 810 and 20", m.ElapsedS, m.MovingS)
	}
	// Ten weighted seconds at 130, ten at 150; the gap sample's 790 s of
	// HR 150 deposit nothing. Elapsed-weighted, this would be ~149.75.
	if m.AvgHR == nil || *m.AvgHR != 140 {
		t.Errorf("avg hr = %v, want exactly 140", m.AvgHR)
	}
	total := 0
	for _, sec := range m.HRHist {
		total += sec
	}
	if total != m.MovingS || m.HRHist[130] != 10 || m.HRHist[150] != 10 {
		t.Errorf("hist = %v (total %d), want 10 s each of 130 and 150", m.HRHist, total)
	}
	// Drift: halves split at t=405 — all 130 before, all 150 after.
	if m.HRDrift == nil || *m.HRDrift != 20 {
		t.Errorf("drift = %v, want 20", m.HRDrift)
	}
	if sh := runGradeShare(st, 145); sh == nil || *sh != 0.5 {
		t.Errorf("share under 145 = %v, want exactly 0.5", sh)
	}

	// Through the route: the payload says how much of the clock was work —
	// and only on a file that stopped. The stop-free fixture's payload must
	// not gain the keys, so a gap-free grade context is unchanged by
	// construction.
	dir := t.TempDir()
	mux := fitTestMux(t, dir)
	if rec := post(mux, "/api/activity?name=2026-08-01-12-00-00.fit", body); rec.Code != http.StatusNoContent {
		t.Fatalf("POST = %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(get(mux, "/api/activity-metrics?name=2026-08-01-12-00-00.fit", nil).Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["moving_s"] != 20.0 || out["moving_hms"] != "0:20" {
		t.Errorf("payload moving = %v %v, want 20 and 0:20", out["moving_s"], out["moving_hms"])
	}
	if rec := post(mux, "/api/activity?name=2026-08-01-13-00-00.fit", tenSecondRun(t)); rec.Code != http.StatusNoContent {
		t.Fatal("gap-free fixture refused")
	}
	var free map[string]any
	if err := json.Unmarshal(get(mux, "/api/activity-metrics?name=2026-08-01-13-00-00.fit", nil).Body.Bytes(), &free); err != nil {
		t.Fatal(err)
	}
	if _, has := free["moving_s"]; has {
		t.Error("a stop-free file's payload gained moving_s — it must be byte-identical to what it always was")
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
	// 49 and 50 pin the dropout boundary from both sides: 50 is the first
	// valid sample, and a drift to >50 or >=51 in either the SQL or the
	// stream side breaks the equality below.
	hrs := []uint8{40, 45, 49, 50, 60, 120, 150, 155, 157, 158, 170, 185, 60, 45}
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
	if _, err := db.importOne(name, data, time.UTC, nil); err != nil {
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
	if _, err := db.importOne("2026-08-01-12-00-00.fit", tenSecondRun(t), time.UTC, nil); err != nil {
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
	// The rubric measures under the anchor the BLOCK names — the example
	// block's legend says easyCap (150), and the example athlete carries no
	// anchor called gradeCap at all, so anything hardcoding that name
	// serves nothing here.
	gi, _ := out["grade_input"].(map[string]any)
	if gi == nil || gi["grade_cap_bpm"] != 150.0 || gi["under_grade_cap_share"] != 1.0 {
		t.Errorf("grade_input = %v, want the declared easyCap of 150", out["grade_input"])
	}
	f20, _ := out["first_20min"].(map[string]any)
	if f20 == nil || f20["cap_bpm"] != 140.0 || f20["avg_bpm"] != 125.5 {
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
	db.reconcile(actDir, time.UTC, nil)

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

	// A recorded failure must RETRY on the next pass, not wedge: heal the
	// file (test-only surgery) and reconcile again.
	if err := os.WriteFile(filepath.Join(actDir, "2026-08-03-12-00-00.fit"), good, 0o644); err != nil {
		t.Fatal(err)
	}
	db.reconcile(actDir, time.UTC, nil)
	if ok, _ := db.has("2026-08-03-12-00-00.fit"); !ok {
		t.Error("failure did not retry on the next reconcile pass")
	}
	if msg, _ := db.failureFor("2026-08-03-12-00-00.fit"); msg != "" {
		t.Errorf("stale failure survived a successful retry: %q", msg)
	}
}

// TestReconcilePrunesWhatLeftTheArchive: the archive is the authority in
// both directions. A file that is gone loses its rows — otherwise the
// calendar keeps offering a session that opens to a 404 — but an archive
// directory that reads EMPTY prunes nothing, because an unmounted volume
// and an emptied archive are indistinguishable and want opposite answers.
func TestReconcilePrunesWhatLeftTheArchive(t *testing.T) {
	dir := t.TempDir()
	actDir := filepath.Join(dir, "activities")
	if err := os.MkdirAll(actDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const kept, gone = "2026-08-01-12-00-00.fit", "2026-08-02-12-00-00.fit"
	good := tenSecondRun(t)
	for _, n := range []string{kept, gone} {
		if err := os.WriteFile(filepath.Join(actDir, n), good, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A file that fails to import leaves a failures row keyed by the same
	// name; deleting it must take that row too.
	const bad = "2026-08-03-12-00-00.fit"
	if err := os.WriteFile(filepath.Join(actDir, bad), fitBytes("junk"), 0o644); err != nil {
		t.Fatal(err)
	}

	db, err := openMetricsDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.close()
	db.reconcile(actDir, time.UTC, nil)

	hist := func(name string) int {
		var n int
		if err := db.r.QueryRow(`SELECT count(*) FROM hr_hist WHERE name=?`, name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if hist(gone) == 0 {
		t.Fatal("fixture: no histogram rows to prune")
	}

	for _, n := range []string{gone, bad} {
		if err := os.Remove(filepath.Join(actDir, n)); err != nil {
			t.Fatal(err)
		}
	}
	db.reconcile(actDir, time.UTC, nil)

	if ok, _ := db.has(gone); ok {
		t.Error("a deleted file kept its activities row")
	}
	if n := hist(gone); n != 0 {
		t.Errorf("a deleted file kept %d histogram rows", n)
	}
	if msg, _ := db.failureFor(bad); msg != "" {
		t.Errorf("a deleted file kept its failure row: %q", msg)
	}
	if ok, err := db.has(kept); err != nil || !ok {
		t.Errorf("the surviving file lost its row (ok=%v err=%v)", ok, err)
	}

	// Now the dangerous case: every file gone. That reads exactly like a
	// volume that failed to mount, so nothing is pruned.
	if err := os.Remove(filepath.Join(actDir, kept)); err != nil {
		t.Fatal(err)
	}
	db.reconcile(actDir, time.UTC, nil)
	if ok, err := db.has(kept); err != nil || !ok {
		t.Errorf("an empty archive pruned the cache (ok=%v err=%v)", ok, err)
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
	if _, err := db.importOne(name, tenSecondRun(t), time.UTC, nil); err != nil {
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

// TestPyRoundMatchesPythonTies pins the mirror-parity rounding on the tie
// quotients where RoundToEven(x·10ⁿ)/10ⁿ measurably diverges from python's
// round() — weighted means are integer-sum quotients, so these are
// reachable from ordinary HR data.
func TestPyRoundMatchesPythonTies(t *testing.T) {
	cases := []struct {
		x    float64
		n    int
		want float64
	}{
		{2007.0 / 20, 1, 100.3}, // just under the tie; python 100.3
		{2049.0 / 20, 1, 102.5}, // just over the tie; python 102.5
		{-2007.0 / 20, 1, -100.3},
		{160.5, 0, 160}, // the FTP-214 z2 band top: exact tie, to even
		{0.125, 2, 0.12},
		{-2.5, 0, -2},
		{10.45, 2, 10.45},
	}
	for _, c := range cases {
		if got := pyRound(c.x, c.n); got != c.want {
			t.Errorf("pyRound(%v, %d) = %v, want %v", c.x, c.n, got, c.want)
		}
	}
}

// TestBikeDecouplingExcludesWarmup pins the register's 600 s rule with the
// same records under both sports: a ride whose first ten minutes would
// swamp the signal decouples 0 as a bike and 200 as a run.
func TestBikeDecouplingExcludesWarmup(t *testing.T) {
	build := func(sport typedef.Sport) *activityMetrics {
		msgs := make([]proto.Message, 0, 1202)
		for i := 0; i <= 1200; i++ {
			hr, w := uint8(100), uint16(300) // warm-up: high output, low HR
			if i > 600 {
				hr, w = 150, 150 // steady: efficiency exactly 1.0
			}
			msgs = append(msgs, mesgdef.NewRecord(nil).
				SetTimestamp(fixtureT0.Add(time.Duration(i)*time.Second)).
				SetHeartRate(hr).
				SetPower(w).ToMesg(nil))
		}
		msgs = append(msgs, sessionMsg(sport, 100_00))
		s, err := decodeActivity(encodeActivityFixture(t, msgs...))
		if err != nil {
			t.Fatal(err)
		}
		return computeMetrics("x.fit", "2026-08-01", s)
	}
	bike := build(typedef.SportCycling)
	if bike.DecouplingPct == nil || *bike.DecouplingPct != 0 {
		t.Errorf("bike decoupling = %v, want exactly 0 (warm-up excluded)", bike.DecouplingPct)
	}
	run := build(typedef.SportRunning)
	if run.DecouplingPct == nil || *run.DecouplingPct != 200 {
		t.Errorf("run decoupling = %v, want exactly 200 (whole trace)", run.DecouplingPct)
	}
}

// TestBandFloors: the legend's letter bands are authored as prose, and the
// floor derived from each must reproduce the rubric exactly — a share of
// 21% is the band opening at 20, not the one below it. All-or-nothing, so
// a rubric this cannot read falls back to prose rather than half-numbers.
func TestBandFloors(t *testing.T) {
	ok := []GradeBand{{"A", "≥80%"}, {"B", "60–79%"}, {"C", "40–59%"}, {"D", "20–39%"}, {"F", "under 20%"}}
	got := bandFloors(ok)
	want := []float64{80, 60, 40, 20, 0}
	if len(got) != len(want) {
		t.Fatalf("floors = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("floor[%d] = %v, want %v", i, got[i], want[i])
		}
	}
	// The letter is the first band whose floor the share reaches.
	letter := func(pct float64) string {
		for i, f := range got {
			if pct >= f {
				return ok[i].Grade
			}
		}
		return "?"
	}
	for _, c := range []struct {
		pct  float64
		want string
	}{{99.4, "A"}, {80, "A"}, {79.9, "B"}, {42.3, "C"}, {21.07, "D"}, {20, "D"}, {19.9, "F"}, {1, "F"}} {
		if g := letter(c.pct); g != c.want {
			t.Errorf("%.2f%% → %s, want %s", c.pct, g, c.want)
		}
	}

	for _, bad := range [][]GradeBand{
		{{"A", "most of it"}},            // unparsable
		{{"A", "≥80%"}, {"B", "≥90%"}},   // not descending
		{{"A", "≥80%"}, {"B", "60–79%"}}, // nothing catches the bottom
	} {
		if f := bandFloors(bad); f != nil {
			t.Errorf("%v parsed to %v, want nothing", bad, f)
		}
	}
}

// TestBestRolling pins the peak an average hides: a ramp whose last minute
// is the whole result, and the refusal when the trace is shorter than the
// window.
func TestBestRolling(t *testing.T) {
	// 180 s: 60 s at 100 W, 60 at 200, 60 at 300 — the best minute is the
	// last one, and the mean over the whole thing is 200.
	tt := make([]int, 0, 181)
	vals := make([]float64, 0, 181)
	for i := 0; i <= 180; i++ {
		tt = append(tt, i)
		switch {
		case i <= 60:
			vals = append(vals, 100)
		case i <= 120:
			vals = append(vals, 200)
		default:
			vals = append(vals, 300)
		}
	}
	got := bestRolling(tt, vals, 60)
	if got == nil || *got != 300 {
		t.Errorf("best 60 s = %v, want 300", got)
	}
	if b := bestRolling(tt[:30], vals[:30], 60); b != nil {
		t.Errorf("a trace shorter than the window returned %v, want nil", *b)
	}
	if b := bestRolling(nil, nil, 60); b != nil {
		t.Errorf("empty trace returned %v", *b)
	}
}

// gappedRun is a trace with a hole in it: `before` seconds at `slow` m/s,
// then `hole` seconds nothing describes, then `after` seconds at `fast`,
// sampled every `every` seconds. The session's odometer states only the
// ground the records cover, which is what an athlete standing still covers.
func gappedRun(t *testing.T, every, before, hole, after int, slow, fast float64) *activityStreams {
	t.Helper()
	var msgs []proto.Message
	raw := func(v float64) uint16 { return uint16(v * 1000) }
	for sec := 0; sec <= before; sec += every {
		msgs = append(msgs, runRecord(sec, 140, raw(slow), 80))
	}
	for sec := before + hole; sec <= before+hole+after; sec += every {
		msgs = append(msgs, runRecord(sec, 150, raw(fast), 85))
	}
	dist := float64(before)*slow + float64(after)*fast
	msgs = append(msgs, sessionMsg(typedef.SportRunning, uint32(dist*100)))
	s, err := decodeActivity(encodeActivityFixture(t, msgs...))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestSegmentsRefuseARecordingGap: a stretch that spans a stop is not a best
// effort. Nothing in the file says how far the athlete travelled while the
// recording was stopped, and the sample at the far end reports the moment it
// resumed — integrating that across the gap invents ground nobody covered,
// and the invention wins the ranking precisely because it is invented.
// Measured on the 12 Aug 2026 ride before this rule: 9,551 m integrated
// against a 7,490 m odometer, all of it inside one 296 s stop, and the
// "fastest mile" it handed the grader was 2,065 m of standing still.
func TestSegmentsRefuseARecordingGap(t *testing.T) {
	// Twenty minutes at 3 m/s, a 300 s stop, twenty minutes at 4 m/s.
	// Nothing on this trace covers ground faster than 4 m/s.
	s := gappedRun(t, 1, 1200, 300, 1200, 3, 4)
	// The same shape, five minutes a side: one four-minute stretch fits in
	// each half and no more.
	short := gappedRun(t, 1, 300, 300, 300, 3, 4)

	straddle := func(g segment, gapStart, gapEnd int) bool {
		return g.StartS < gapEnd && g.StartS+int(g.Secs) > gapStart
	}
	for _, tc := range []struct {
		what          string
		on            *activityStreams
		gapStart      int
		meters        float64
		secs, count   int
		wantN         int
		maxM, minSecs float64
	}{
		// The fastest four minutes: 240 s of 4 m/s is 960 m, and the stop
		// offers 1,200 m to anyone willing to count it.
		{"240 s x3", s, 1200, 0, 240, 3, 3, 960.1, 240},
		// The fastest 800s: real ones take 200 s at 4 m/s, and a sample's
		// worth of overshoot; the stop hands over 800 m for nothing.
		{"800 m x3", s, 1200, 800, 0, 3, 3, 804.1, 199.9},
		// Asked for more than the trace holds gap-free, it returns what it
		// has rather than filling the tail with the stop.
		{"240 s x12", short, 300, 0, 240, 12, 2, 960.1, 240},
	} {
		segs := fastestSegments(tc.on, tc.meters, tc.secs, tc.count, Metric)
		if len(segs) != tc.wantN {
			t.Errorf("%s: %d segments, want %d: %+v", tc.what, len(segs), tc.wantN, segs)
		}
		for _, g := range segs {
			if straddle(g, tc.gapStart, tc.gapStart+300) {
				t.Errorf("%s: segment %+v spans the 300 s stop", tc.what, g)
			}
			if g.DistM > tc.maxM {
				t.Errorf("%s: segment %+v covers %.1f m, more than the trace describes", tc.what, g, g.DistM)
			}
			if g.Secs < tc.minSecs {
				t.Errorf("%s: segment %+v is shorter than the ground allows", tc.what, g)
			}
		}
	}

	// The distance the file describes is the ground the records cover; the
	// stop adds none of its own.
	dist, gaps := describedDistance(s)
	if n := len(s.Time) - 1; gaps[n] != 1 {
		t.Errorf("%d gaps, want exactly the one stop", gaps[n])
	} else if want := *s.DistM; dist[n] < want-1 || dist[n] > want+1 {
		t.Errorf("integrated %.1f m against a %.1f m odometer", dist[n], want)
	}
}

// TestGapThresholdFollowsTheRecordingRate: what counts as a gap is the
// file's own recording rate, not a constant. Smart recording writes a sample
// when something changes, so twelve seconds between samples describes twelve
// steady seconds; on a 1 Hz file the same twelve are eleven samples that do
// not exist. A device that records every eight seconds must still be
// able to report a best effort — measured 15 Aug 2026, archived files whose
// median interval is 2-4 s reach 17 s between samples with nothing wrong,
// while every stop measured runs 57 s or longer against a median of 1.
func TestGapThresholdFollowsTheRecordingRate(t *testing.T) {
	oneHz := make([]int, 61)
	for i := range oneHz {
		oneHz[i] = i
	}
	if got := recordingGapS(oneHz); got != gapFloorS {
		t.Errorf("1 Hz: gap threshold %d s, want the floor %d s", got, gapFloorS)
	}
	slow := make([]int, 61)
	for i := range slow {
		slow[i] = i * 8
	}
	if got, want := recordingGapS(slow), 8*gapCadenceMult; got != want {
		t.Errorf("8 s cadence: gap threshold %d s, want %d s", got, want)
	}

	// The same trace sampled every 8 s: no gaps, and the best efforts still
	// come back. This is the regression a flat threshold would cause.
	s := gappedRun(t, 8, 800, 8, 800, 3, 4)
	if _, gaps := describedDistance(s); gaps[len(s.Time)-1] != 0 {
		t.Errorf("an 8 s recording rate read as %d gaps", gaps[len(s.Time)-1])
	}
	segs := fastestSegments(s, 0, 240, 3, Metric)
	if len(segs) != 3 {
		t.Fatalf("8 s cadence: %d segments, want 3: %+v", len(segs), segs)
	}
	for _, g := range segs {
		if g.DistM > 960.1 {
			t.Errorf("8 s cadence: segment %+v covers more than the trace describes", g)
		}
	}
	// Hole one recording rate is not the other: 120 s of nothing is a stop
	// on the same file.
	s = gappedRun(t, 8, 800, 120, 800, 3, 4)
	if _, gaps := describedDistance(s); gaps[len(s.Time)-1] != 1 {
		t.Errorf("a 120 s stop on an 8 s file read as %d gaps, want 1", gaps[len(s.Time)-1])
	}
	for _, g := range fastestSegments(s, 0, 240, 3, Metric) {
		if g.StartS < 920 && g.StartS+int(g.Secs) > 800 {
			t.Errorf("segment %+v spans the 120 s stop", g)
		}
	}
}

// TestNonMonotonicTimeKeepsDiffZero: a backwards timestamp (a chained-file
// seam) contributes nothing anywhere — the histogram and the stream
// computation must still agree exactly.
func TestNonMonotonicTimeKeepsDiffZero(t *testing.T) {
	secs := []int{0, 1, 2, 3, 4, 5, 3, 6, 7, 8, 9, 10}
	hrs := []uint8{45, 120, 150, 158, 45, 160, 140, 120, 155, 49, 50, 165}
	msgs := make([]proto.Message, 0, len(secs)+1)
	for i, sec := range secs {
		msgs = append(msgs, runRecord(sec, hrs[i], 3000, 80))
	}
	msgs = append(msgs, sessionMsg(typedef.SportRunning, 30_00))
	data := encodeActivityFixture(t, msgs...)

	s, err := decodeActivity(data)
	if err != nil {
		t.Fatal(err)
	}
	if s.Time[6] != 3 {
		t.Fatalf("backwards timestamp lost: %v", s.Time)
	}
	db, err := openMetricsDB(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.close()
	const name = "2026-08-01-12-00-00.fit"
	if _, err := db.importOne(name, data, time.UTC, nil); err != nil {
		t.Fatal(err)
	}
	for _, cap := range []int{50, 140, 157} {
		sqlShare, err := db.underCapShareSQL(name, cap)
		if err != nil {
			t.Fatal(err)
		}
		direct := runGradeShare(s, cap)
		if (sqlShare == nil) != (direct == nil) || (sqlShare != nil && *sqlShare != *direct) {
			t.Errorf("cap %d: sql %v ≠ direct %v on non-monotonic time", cap, sqlShare, direct)
		}
	}
}

// TestImportPanicContained: a panicking decoder costs one failures row,
// never the process. The seam exists because fuzzing today's decoder
// proves nothing about next year's upgrade.
func TestImportPanicContained(t *testing.T) {
	orig := decodeForImport
	decodeForImport = func([]byte) (*activityStreams, error) { panic("injected decoder panic") }
	defer func() { decodeForImport = orig }()

	db, err := openMetricsDB(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.close()
	const name = "2026-08-01-12-00-00.fit"
	if _, err := db.importOne(name, []byte("whatever"), time.UTC, nil); err == nil ||
		!strings.Contains(err.Error(), "decode panic") {
		t.Fatalf("panic not contained as an error: %v", err)
	}
	if msg, _ := db.failureFor(name); !strings.Contains(msg, "injected") {
		t.Errorf("failure not recorded: %q", msg)
	}
}

// TestStartupReconcile pins the wiring, not just the parts: the server
// method main launches must reach the archive.
func TestStartupReconcile(t *testing.T) {
	dir := t.TempDir()
	actDir := filepath.Join(dir, "activities")
	if err := os.MkdirAll(actDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const name = "2026-08-01-12-00-00.fit"
	if err := os.WriteFile(filepath.Join(actDir, name), tenSecondRun(t), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := fitTestMuxServer(t, dir)
	ts.s.startupReconcile()
	if ok, err := ts.s.metrics.has(name); err != nil || !ok {
		t.Fatalf("startup reconcile missed the archive (ok=%v err=%v)", ok, err)
	}
}

// TestFailedImportRecovers pins both retry paths for a stored file whose
// first decode failed: the startup reconcile, and a re-POST of the name
// (which 409s, and retries the import from the canonical bytes on disk).
func TestFailedImportRecovers(t *testing.T) {
	dir := t.TempDir()
	ts := fitTestMuxServer(t, dir)
	const name = "2026-08-05-12-00-00.fit"
	if rec := post(ts.mux, "/api/activity?name="+name, fitBytes("broken")); rec.Code != http.StatusNoContent {
		t.Fatalf("POST = %d", rec.Code)
	}
	if rec := get(ts.mux, "/api/activity-metrics?name="+name, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("failed import served metrics: %d", rec.Code)
	}
	// The archive file "heals" (test-only surgery — in production the bytes
	// are immutable and it is the environment that heals).
	if err := os.WriteFile(filepath.Join(dir, "activities", name), tenSecondRun(t), 0o644); err != nil {
		t.Fatal(err)
	}
	if rec := post(ts.mux, "/api/activity?name="+name, fitBytes("dup")); rec.Code != http.StatusConflict {
		t.Fatalf("re-POST = %d, want 409", rec.Code)
	}
	if rec := get(ts.mux, "/api/activity-metrics?name="+name, nil); rec.Code != http.StatusOK {
		t.Fatalf("retry did not recover the import: %d %s", rec.Code, rec.Body)
	}
}

// TestDayAPIExplicitBlock: with two blocks loaded, ?block= serves exactly
// the named block's plan — the current block must never quietly answer for
// an explicitly named one.
func TestDayAPIExplicitBlock(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "blocks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "library"), 0o755); err != nil {
		t.Fatal(err)
	}
	copyFile(t, "./defaults/athlete.json", filepath.Join(dir, "athlete.json"))
	for _, f := range []string{"guides.json", "index.json"} {
		copyFile(t, "./defaults/library/"+f, filepath.Join(dir, "library", f))
	}
	copyFile(t, "./defaults/blocks/example-base-block.json", filepath.Join(dir, "blocks", "example-base-block.json"))
	raw, err := os.ReadFile("./defaults/blocks/example-base-block.json")
	if err != nil {
		t.Fatal(err)
	}
	var blk map[string]any
	if err := json.Unmarshal(raw, &blk); err != nil {
		t.Fatal(err)
	}
	blk["id"] = "example-later-block"
	blk["start"] = "2026-06-01" // a Monday, so the block loads
	later, err := json.Marshal(blk)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blocks", "example-later-block.json"), later, 0o644); err != nil {
		t.Fatal(err)
	}

	mux := fitTestMux(t, dir)
	rec := get(mux, "/api/day?date=2026-01-06&block=example-base-block", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"block":"example-base-block"`) ||
		!strings.Contains(rec.Body.String(), `"quality"`) {
		t.Errorf("explicit base block: %d %.120s", rec.Code, rec.Body.String())
	}
	// Without ?block=, the current block (the later one) answers — and this
	// date is outside it, so a fallback to the other block would be a lie.
	if rec := get(mux, "/api/day?date=2026-01-06", nil); rec.Code != http.StatusNotFound {
		t.Errorf("date outside the current block = %d, want 404", rec.Code)
	}
	rec = get(mux, "/api/day?date=2026-06-02&block=example-later-block", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"block":"example-later-block"`) {
		t.Errorf("explicit later block: %d %.120s", rec.Code, rec.Body.String())
	}
}

// TestSkippingASession: a session not done is a decision worth recording,
// and a blank day cannot tell that from one nobody logged. It is written
// like every other state here — appended, reversible by a later entry,
// last one wins — and it reaches the grader through the day's prescription.
func TestSkippingASession(t *testing.T) {
	ts := fitTestMuxServer(t, t.TempDir())
	const date = "2026-01-06"

	if _, skipped := ts.s.store.SkipOn(date); skipped {
		t.Fatal("a day nobody touched reads as skipped")
	}

	body := []byte(`{"date":"` + date + `","kind":"skip","val":"skipped","note":"no time before work"}`)
	if rec := post(ts.mux, "/api/entry", body); rec.Code != http.StatusNoContent {
		t.Fatalf("POST skip = %d: %s", rec.Code, rec.Body)
	}
	e, skipped := ts.s.store.SkipOn(date)
	if !skipped || e.Note != "no time before work" {
		t.Fatalf("SkipOn = %+v, %v", e, skipped)
	}
	if _, ok := ts.s.store.Skips()[date]; !ok {
		t.Error("Skips() does not carry the day")
	}

	// It reaches the grader with the reason, on the day's prescription.
	rec := get(ts.mux, "/api/day?date="+date, nil)
	var day struct {
		Skipped  bool   `json:"skipped"`
		SkipNote string `json:"skip_note"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &day); err != nil {
		t.Fatal(err)
	}
	if !day.Skipped || day.SkipNote != "no time before work" {
		t.Errorf("/api/day = %+v", day)
	}

	// Changing his mind is another entry, not an edit: the last one wins
	// and the fact that he changed it survives.
	if rec := post(ts.mux, "/api/entry",
		[]byte(`{"date":"`+date+`","kind":"skip","val":"unskipped"}`)); rec.Code != http.StatusNoContent {
		t.Fatalf("POST unskip = %d", rec.Code)
	}
	if _, skipped := ts.s.store.SkipOn(date); skipped {
		t.Error("still skipped after unskipping")
	}
	if _, ok := ts.s.store.Skips()[date]; ok {
		t.Error("Skips() still carries an unskipped day")
	}
	if n := len(ts.s.store.All()); n != 2 {
		t.Errorf("%d entries; the log is append-only and must hold both", n)
	}

	// A state this does not have is refused rather than stored.
	if rec := post(ts.mux, "/api/entry",
		[]byte(`{"date":"`+date+`","kind":"skip","val":"maybe"}`)); rec.Code != http.StatusBadRequest {
		t.Errorf(`val "maybe" = %d, want 400`, rec.Code)
	}
}

// TestSkipShowsOnThePages: the control appears on a day with a session and
// not on a rest day, the state survives a reload rather than living only in
// the tap that made it, and the calendar marks the day with the reason.
func TestSkipShowsOnThePages(t *testing.T) {
	// The today page renders now, so the block has to contain now: the
	// example one is shifted to this week. Blocks start on a Monday, so
	// which session today is depends on the day the suite runs — and a rest
	// day must offer nothing to skip.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "blocks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "library"), 0o755); err != nil {
		t.Fatal(err)
	}
	copyFile(t, "./defaults/athlete.json", filepath.Join(dir, "athlete.json"))
	for _, f := range []string{"guides.json", "index.json"} {
		copyFile(t, "./defaults/library/"+f, filepath.Join(dir, "library", f))
	}
	raw, err := os.ReadFile("./defaults/blocks/example-base-block.json")
	if err != nil {
		t.Fatal(err)
	}
	var blk map[string]any
	if err := json.Unmarshal(raw, &blk); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	monday := now.AddDate(0, 0, -(int(now.Weekday())+6)%7)
	blk["start"] = monday.Format("2006-01-02")
	shifted, err := json.Marshal(blk)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blocks", "example-base-block.json"), shifted, 0o644); err != nil {
		t.Fatal(err)
	}

	ts := fitTestMuxServer(t, dir)
	today := ts.s.day(ts.s.ds())
	iso := today.Format("2006-01-02")
	wk, di, ok := ts.s.ds().Current(today).Locate(today)
	if !ok {
		t.Fatalf("%s is not in the shifted block", iso)
	}
	rest := wk.Days[di].Kind == KindRest

	body := get(ts.mux, "/", nil).Body.String()
	switch {
	case rest && strings.Contains(body, `class="skipbtn"`):
		t.Error("a rest day offers a session to skip")
	case !rest && !strings.Contains(body, `class="skipbtn"`):
		t.Error("no skip control on a session day")
	}

	if err := ts.s.store.Append(Entry{
		Date: iso, Kind: kindSkip, Val: "skipped", Note: "no time before work",
	}); err != nil {
		t.Fatal(err)
	}
	if !rest {
		body = get(ts.mux, "/", nil).Body.String()
		if !strings.Contains(body, "Not done — no time before work") {
			t.Error("the skipped state does not survive a reload")
		}
		if !strings.Contains(body, "card k-") || !strings.Contains(body, " skipped\">") {
			t.Error("the session card is not marked skipped")
		}
	}

	// The calendar marks the day and carries the reason in the cell's title.
	cal := get(ts.mux, "/calendar", nil).Body.String()
	if !strings.Contains(cal, " skipped") {
		t.Error("the calendar does not mark a skipped day")
	}
	if !strings.Contains(cal, "Skipped — no time before work") {
		t.Error("the calendar cell does not carry the reason")
	}
}

// TestDayCarriesTheIssueRatingAndItsAction: what the athlete reported that
// day reaches the grader with the instruction attached. The number alone
// is not enough — a session taken easy on an instruction to take it easy
// is compliance, and only the band's action says so.
func TestDayCarriesTheIssueRatingAndItsAction(t *testing.T) {
	ts := fitTestMuxServer(t, t.TempDir())
	d := ts.s.ds()
	if len(d.Athlete.Issues) == 0 {
		t.Skip("the example athlete declares no issue")
	}
	is := d.Athlete.Issues[0]
	// The top band: whatever "worst" means on this athlete's own scale.
	worst := is.Scale.Max
	band := is.BandFor(worst)
	if band == nil {
		t.Fatalf("no band covers %d", worst)
	}
	if err := ts.s.store.Append(Entry{
		Date: "2026-01-06", Kind: kindIssue, Key: is.Key,
		Val: strconv.Itoa(worst), Note: "sore on the warm-up",
	}); err != nil {
		t.Fatal(err)
	}

	rec := get(ts.mux, "/api/day?date=2026-01-06", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d", rec.Code)
	}
	var out struct {
		Issues []struct {
			Key, Name, Tone, Label, Action, Note string
			Rating                               int
		} `json:"issues"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Issues) != 1 {
		t.Fatalf("issues = %+v", out.Issues)
	}
	got := out.Issues[0]
	if got.Key != is.Key || got.Rating != worst {
		t.Errorf("rating: %+v", got)
	}
	if got.Action == "" || got.Action != stripEmph(band.Action) {
		t.Errorf("action = %q, want the band's own %q", got.Action, stripEmph(band.Action))
	}
	if got.Tone != band.Tone || got.Label != band.Label {
		t.Errorf("band: %+v, want tone %q label %q", got, band.Tone, band.Label)
	}
	if got.Note != "sore on the warm-up" {
		t.Errorf("the athlete's own words were dropped: %q", got.Note)
	}

	// A day with no rating carries none, rather than a zero that reads as
	// "no pain reported".
	rec = get(ts.mux, "/api/day?date=2026-01-07", nil)
	out.Issues = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Issues) != 0 {
		t.Errorf("unrated day carried %+v", out.Issues)
	}
}

// TestGradeInputServedWithAnchors drives the grade-input wiring the
// defaults athlete deliberately cannot: an athlete who declares the run cap
// and the bike band gets both kinds' grade inputs computed and served.
func TestGradeInputServedWithAnchors(t *testing.T) {
	dir := t.TempDir()
	raw, err := os.ReadFile("./defaults/athlete.json")
	if err != nil {
		t.Fatal(err)
	}
	var ath map[string]any
	if err := json.Unmarshal(raw, &ath); err != nil {
		t.Fatal(err)
	}
	hr := ath["hr"].(map[string]any)
	// gradeCap is deliberately present AND different: the example block's
	// legend declares easyCap, so 150 must win over the tempting name.
	hr["gradeCap"] = 157
	hr["bikeLo"], hr["bikeHi"], hr["bikeCap"] = 130, 140, 145
	mod, err := json.Marshal(ath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "athlete.json"), mod, 0o644); err != nil {
		t.Fatal(err)
	}

	ts := fitTestMuxServer(t, dir)
	const run = "2026-08-01-12-00-00.fit"
	if rec := post(ts.mux, "/api/activity?name="+run, tenSecondRun(t)); rec.Code != http.StatusNoContent {
		t.Fatalf("run POST = %d", rec.Code)
	}
	var out struct {
		GradeInput map[string]any `json:"grade_input"`
		First20    map[string]any `json:"first_20min"`
		Power      map[string]any `json:"power"`
	}
	rec := get(ts.mux, "/api/activity-metrics?name="+run, nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.GradeInput["under_grade_cap_share"] != 1.0 || out.GradeInput["grade_cap_bpm"] != 150.0 {
		t.Errorf("run grade_input = %v", out.GradeInput)
	}
	if out.First20 == nil {
		t.Error("first_20min absent with firstMin declared")
	}

	// An eleven-minute steady bike: the after-warm-up window exists, sits
	// entirely in the 130–140 band, and never crosses the 145 cap.
	msgs := make([]proto.Message, 0, 702)
	for i := 0; i <= 700; i++ {
		msgs = append(msgs, mesgdef.NewRecord(nil).
			SetTimestamp(fixtureT0.Add(time.Duration(i)*time.Second)).
			SetHeartRate(135).
			SetPower(150).ToMesg(nil))
	}
	msgs = append(msgs, sessionMsg(typedef.SportCycling, 50_00))
	const bike = "2026-08-02-12-00-00.fit"
	if rec := post(ts.mux, "/api/activity?name="+bike, encodeActivityFixture(t, msgs...)); rec.Code != http.StatusNoContent {
		t.Fatalf("bike POST = %d", rec.Code)
	}
	rec = get(ts.mux, "/api/activity-metrics?name="+bike, nil)
	out.GradeInput, out.Power = nil, nil
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.GradeInput["in_band_share_after_warmup"] != 1.0 || out.GradeInput["secs_over_hr_cap"] != 0.0 {
		t.Errorf("bike grade_input = %v", out.GradeInput)
	}
	if out.Power["avg"] != 150.0 || out.Power["pct_ftp"] != 0.75 {
		t.Errorf("bike power = %v", out.Power)
	}
}

// TestActivityDate: the device name is the training day; a name without a
// date defers to the recording's start in the athlete's timezone.
func TestActivityDate(t *testing.T) {
	// The named fixture is deliberately synthetic (a future date): a real
	// device filename in a tracked file would leak the archive's contents
	// into the public repo.
	chi := chicago(t)
	if d := activityDate("2030-01-02-03-04-05.fit", fixtureT0, chi); d != "2030-01-02" {
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

// TestStopsSayWhereNotJustWhen: a grade note that places a stop from the
// per-minute profile is guessing — measured against the real file, one such
// note put a 2:34 stop "around minutes 82-92" when it began at 90:49, 9.34
// miles in. The register states both, and states distance because that is
// how an athlete finds the place again.
func TestStopsSayWhereNotJustWhen(t *testing.T) {
	// Ten minutes at 3 m/s, a 200 s stop, ten more. The stop begins at
	// 600 s and 1,800 m — 1.118 miles.
	s := gappedRun(t, 1, 600, 200, 600, 3, 3)
	stops, total := stopsIn(s, Imperial)
	if len(stops) != 1 {
		t.Fatalf("%d stops, want the one: %+v", len(stops), stops)
	}
	st := stops[0]
	if st.AtS != 600 || st.AtHMS != "10:00" || st.Secs != 200 {
		t.Errorf("stop at %d s (%q) for %d s, want 600 (10:00) for 200", st.AtS, st.AtHMS, st.Secs)
	}
	if math.Abs(st.AtDistM-1800) > 1 {
		t.Errorf("stop at %.1f m in, want 1,800", st.AtDistM)
	}
	if st.AtDist != "1.12 mi" {
		t.Errorf("stop at %q in, want it spoken in the athlete's units", st.AtDist)
	}
	if total != 200 {
		t.Errorf("stopped total %d s, want 200", total)
	}

	// The device's own arithmetic is the cross-check: elapsed minus the
	// timer time is what the watch says it paused for. Where the two
	// disagree, one of them is being read wrong.
	d, err := decodeDetail(encodeActivityFixture(t, sessionOnly(t, 1400, 1200)...))
	if err != nil {
		t.Fatal(err)
	}
	if got := d.ElapsedS - d.TimerS; got != 200 {
		t.Errorf("elapsed minus moving = %v, want the 200 s the session states", got)
	}

	// A gap-free recording has nothing to report, and says nothing.
	clean := gappedRun(t, 1, 600, 1, 600, 3, 3)
	if stops, total := stopsIn(clean, Imperial); len(stops) != 0 || total != 0 {
		t.Errorf("a continuous recording reported %d stops (%d s)", len(stops), total)
	}

	// A stream with no speed still counts its stops; it just cannot place
	// them on the ground.
	noVel := &activityStreams{Time: []int{0, 1, 2, 300, 301}}
	if stops, total := stopsIn(noVel, Imperial); len(stops) != 1 || total != 298 || stops[0].AtDist != "" {
		t.Errorf("no-speed stream: %+v total %d", stops, total)
	}
}

// sessionOnly is a two-record activity whose SESSION states the clocks, for
// checking the device's own elapsed-minus-timer arithmetic.
func sessionOnly(t *testing.T, elapsedS, timerS int) []proto.Message {
	t.Helper()
	return []proto.Message{
		runRecord(0, 140, 3000, 80),
		runRecord(1, 140, 3000, 80),
		mesgdef.NewSession(nil).SetSport(typedef.SportRunning).
			SetStartTime(fixtureT0).SetTimestamp(fixtureT0.Add(time.Minute)).
			SetTotalElapsedTime(uint32(elapsedS * 1000)).
			SetTotalTimerTime(uint32(timerS * 1000)).
			SetTotalDistance(100_00).ToMesg(nil),
	}
}
