package main

// Pins for the display-only decode: what a lap carries, what a session
// carries, which of it reaches the page, and — the load-bearing one — what
// does NOT reach the grader. Fixtures are synthesized rather than committed,
// so every test states the bytes it relies on and the suite passes in a
// dataless clone.

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/muktihari/fit/decoder"
	"github.com/muktihari/fit/profile/mesgdef"
	"github.com/muktihari/fit/profile/typedef"
	"github.com/muktihari/fit/profile/untyped/mesgnum"
	"github.com/muktihari/fit/proto"
)

// fixtureLat/fixtureLon put every synthesized route in the Sierra Nevada,
// which is where the polyline algorithm's own published example sits and
// where nobody in this repo has ever run. A fixture must never carry a real
// position: tools/sanitizefit exists to strip them out of committed
// recordings, and a test that pasted one back in would defeat it.
const (
	fixtureLat = 38.5
	fixtureLon = -120.2
)

// semicircles is the inverse of the decode side's conversion, so a fixture
// can state a position in degrees.
func semicircles(deg float64) int32 {
	return int32(math.Round(deg * (2147483648.0 / 180.0)))
}

func lapMsg(startSec int, elapsedMS, timerMS, distCM uint32, trigger typedef.LapTrigger) *mesgdef.Lap {
	return mesgdef.NewLap(nil).
		SetStartTime(fixtureT0.Add(time.Duration(startSec) * time.Second)).
		SetTimestamp(fixtureT0.Add(time.Duration(startSec)*time.Second + time.Duration(elapsedMS)*time.Millisecond)).
		SetTotalElapsedTime(elapsedMS).
		SetTotalTimerTime(timerMS).
		SetTotalDistance(distCM).
		SetLapTrigger(trigger)
}

func splitMsg(kind typedef.SplitType, timerMS, distCM uint32, splits uint16) proto.Message {
	return mesgdef.NewSplitSummary(nil).
		SetSplitType(kind).
		SetTotalTimerTime(timerMS).
		SetTotalDistance(distCM).
		SetNumSplits(splits).ToMesg(nil)
}

// gpsRun is a fixture that carries everything a page wants: a route, two
// auto-lapped miles with a workout step on the second, the walk-break split
// summary, and a session whose timer time is shorter than its elapsed.
func gpsRun(t *testing.T, sub typedef.SubSport) []byte {
	t.Helper()
	msgs := make([]proto.Message, 0, 40)
	// Twenty records a metre apart in latitude, so the track has a shape and
	// a known length.
	for i := 0; i <= 20; i++ {
		msgs = append(msgs, mesgdef.NewRecord(nil).
			SetTimestamp(fixtureT0.Add(time.Duration(i)*time.Second)).
			SetHeartRate(uint8(140+i%5)).
			SetSpeed(3000).
			SetCadence(80).
			SetPositionLat(semicircles(fixtureLat+float64(i)*0.00001)).
			SetPositionLong(semicircles(fixtureLon)).ToMesg(nil))
	}
	msgs = append(msgs,
		lapMsg(0, 592_225, 592_225, 160_934, typedef.LapTriggerDistance).
			SetAvgHeartRate(137).SetMaxHeartRate(150).
			SetAvgCadence(85).SetAvgPower(294).SetMaxPower(397).
			SetTotalAscent(12).
			SetEnhancedAvgSpeed(2717).
			SetStartPositionLat(semicircles(fixtureLat)).
			SetStartPositionLong(semicircles(fixtureLon)).
			SetEndPositionLat(semicircles(fixtureLat-0.005)).
			SetEndPositionLong(semicircles(fixtureLon-0.001)).ToMesg(nil),
		lapMsg(593, 591_287, 591_287, 160_934, typedef.LapTriggerManual).
			SetAvgHeartRate(149).SetWktStepIndex(typedef.MessageIndex(3)).ToMesg(nil),
		// A button press: 0.2 m in 1.1 s. Arithmetically a pace of 179:05/mi.
		lapMsg(1185, 1_100, 1_100, 20, typedef.LapTriggerManual).ToMesg(nil),
		splitMsg(typedef.SplitTypeRwdRun, 3_724_002, 1_022_607, 14),
		splitMsg(typedef.SplitTypeRwdWalk, 768_863, 110_180, 15),
		mesgdef.NewSession(nil).
			SetSport(typedef.SportRunning).
			SetSubSport(sub).
			SetStartTime(fixtureT0).
			SetTimestamp(fixtureT0.Add(time.Minute)).
			SetTotalElapsedTime(4_510_464).
			SetTotalTimerTime(4_493_098).
			SetTotalDistance(1_132_787).
			SetTotalAscent(59).ToMesg(nil),
	)
	return encodeActivityFixture(t, msgs...)
}

func detailOf(t *testing.T, data []byte) *detailOut {
	t.Helper()
	dir := t.TempDir()
	srv := fitTestMuxServer(t, "")
	srv.s.dataDir = dir
	const name = "2026-08-01-12-00-00.fit"
	if rec := post(srv.mux, "/api/activity?name="+name, data); rec.Code != http.StatusNoContent {
		t.Fatalf("POST = %d: %s", rec.Code, rec.Body)
	}
	out, code, msg := srv.s.detailPayload(name, "")
	if code != http.StatusOK {
		t.Fatalf("detailPayload = %d %s", code, msg)
	}
	return out
}

// TestDetailCarriesWhatTheWatchRecorded: the session's two clocks, the
// device's own ascent and distance, the laps with their triggers and their
// workout steps, and the run/walk split summary — each read from the file,
// none of it derived.
func TestDetailCarriesWhatTheWatchRecorded(t *testing.T) {
	d := detailOf(t, gpsRun(t, typedef.SubSportGeneric))

	if d.Sport != "running" || d.Indoor {
		t.Errorf("sport %q indoor %v", d.Sport, d.Indoor)
	}
	if d.ElapsedS != 4510.464 || d.MovingS != 4493.098 {
		t.Errorf("clocks: elapsed %v moving %v", d.ElapsedS, d.MovingS)
	}
	if d.ElapsedHMS != "75:10" || d.MovingHMS != "74:53" {
		t.Errorf("hms: %q / %q", d.ElapsedHMS, d.MovingHMS)
	}
	if d.AscentM == nil || *d.AscentM != 59 || d.Ascent != "59 m" {
		t.Errorf("ascent %v %q", d.AscentM, d.Ascent)
	}
	if d.Dist != "11.33 km" {
		t.Errorf("dist %q", d.Dist)
	}

	// Three paces, each labelled and each from the file: the walk-break
	// dilution a grade note otherwise corrects in prose is a stored scalar.
	// The example athlete is metric — that is what app/defaults is for.
	want := map[string]string{"elapsed": "6:38/km", "moving": "6:37/km", "running": "6:04/km"}
	if len(d.Paces) != 3 {
		t.Fatalf("%d paces, want 3: %+v", len(d.Paces), d.Paces)
	}
	for _, p := range d.Paces {
		if want[p.Basis] != p.Pace {
			t.Errorf("%s pace %q, want %q", p.Basis, p.Pace, want[p.Basis])
		}
	}

	if len(d.Laps) != 3 {
		t.Fatalf("%d laps, want 3", len(d.Laps))
	}
	l := d.Laps[0]
	if l.Trigger != "distance" || l.Dist != "1.61 km" || l.Pace != "6:08/km" {
		t.Errorf("lap 1: %+v", l)
	}
	if l.AvgHR == nil || *l.AvgHR != 137 || l.MaxHR == nil || *l.MaxHR != 150 {
		t.Errorf("lap 1 hr: %+v", l)
	}
	if l.AvgCadence == nil || *l.AvgCadence != 170 { // per-leg 85, doubled for a run
		t.Errorf("lap 1 cadence %v, want the doubled 170", l.AvgCadence)
	}
	if l.AvgPower == nil || *l.AvgPower != 294 || l.AscentM == nil || *l.AscentM != 12 {
		t.Errorf("lap 1 power/ascent: %+v", l)
	}
	if len(l.Start) != 2 || len(l.End) != 2 {
		t.Errorf("lap 1 corners: %v %v", l.Start, l.End)
	}
	if l.Step != nil {
		t.Errorf("lap 1 step %v, want none — it was an auto-lap", *l.Step)
	}
	if s := d.Laps[1].Step; s == nil || *s != 3 {
		t.Errorf("lap 2 step %v, want the pushed workout's step 3", s)
	}
	// A lap that is a button press keeps its clock and loses its pace.
	if b := d.Laps[2]; b.Pace != "" || b.TimerS != 1.1 {
		t.Errorf("button-press lap: pace %q timer %v, want no pace", b.Pace, b.TimerS)
	}

	if len(d.Splits) != 2 || d.Splits[0].Type != "rwd_run" || d.Splits[0].Pace != "6:04/km" {
		t.Errorf("splits: %+v", d.Splits)
	}
	if d.Track == nil || d.Track.Points != 21 {
		t.Fatalf("track: %+v", d.Track)
	}
}

// TestDetailGatesTheRouteIndoors: a trainer session records a position and
// it is not a place. Zwift writes its own world's coordinates and Watopia is
// in the Solomon Islands, so an indoor session serves no track and no lap
// corners — the same gate that stopped 79 °F being frozen onto a basement
// FTP test.
func TestDetailGatesTheRouteIndoors(t *testing.T) {
	d := detailOf(t, gpsRun(t, typedef.SubSportVirtualActivity))
	if !d.Indoor {
		t.Fatal("a virtual_activity did not read as indoors")
	}
	if d.Track != nil {
		t.Errorf("indoor session served a %d-point track", d.Track.Points)
	}
	for _, l := range d.Laps {
		if l.Start != nil || l.End != nil {
			t.Errorf("lap %d served corners from a virtual world: %v %v", l.N, l.Start, l.End)
		}
	}
	// Everything that is not a place still travels.
	if d.MovingS != 4493.098 || len(d.Laps) != 3 {
		t.Errorf("indoor session lost its numbers: moving %v laps %d", d.MovingS, len(d.Laps))
	}
}

// TestTimerMayExceedElapsed: Zwift writes a timer time one second LARGER
// than elapsed. Both clocks are reported as recorded — nothing clamps one to
// the other, and nothing treats the file as corrupt for saying so.
func TestTimerMayExceedElapsed(t *testing.T) {
	msgs := []proto.Message{
		mesgdef.NewRecord(nil).SetTimestamp(fixtureT0).SetSpeed(7000).ToMesg(nil),
		mesgdef.NewRecord(nil).SetTimestamp(fixtureT0.Add(time.Second)).SetSpeed(7000).ToMesg(nil),
		lapMsg(0, 1_842_000, 1_843_000, 749_040, typedef.LapTriggerSessionEnd).ToMesg(nil),
		mesgdef.NewSession(nil).
			SetSport(typedef.SportCycling).
			SetSubSport(typedef.SubSportVirtualActivity).
			SetStartTime(fixtureT0).
			SetTimestamp(fixtureT0.Add(time.Minute)).
			SetTotalElapsedTime(1_842_000).
			SetTotalTimerTime(1_843_000).
			SetTotalDistance(749_040).ToMesg(nil),
	}
	d := detailOf(t, encodeActivityFixture(t, msgs...))
	if d.ElapsedS != 1842 || d.MovingS != 1843 {
		t.Errorf("clocks: elapsed %v moving %v, want them both as written", d.ElapsedS, d.MovingS)
	}
	if l := d.Laps[0]; l.ElapsedS != 1842 || l.TimerS != 1843 {
		t.Errorf("lap clocks: elapsed %v timer %v", l.ElapsedS, l.TimerS)
	}
	if len(d.Paces) != 0 {
		t.Errorf("a ride was given paces: %+v", d.Paces)
	}
}

// TestPolylineMatchesTheAlgorithm pins the encoding against the published
// worked example, so this is Google's polyline and not a lookalike, and then
// pins the precision the plan bought with it: five decimal places, no
// simplification, ~1 m worst error.
func TestPolylineMatchesTheAlgorithm(t *testing.T) {
	const wantEnc = `_p~iF~ps|U_ulLnnqC_mqNvxq` + "`" + `@`
	pts := []trackPoint{{Lat: 38.5, Lon: -120.2}, {Lat: 40.7, Lon: -120.95}, {Lat: 43.252, Lon: -126.453}}
	if got := encodePolyline(pts); got != wantEnc {
		t.Fatalf("encodePolyline = %q, want the published example %q", got, wantEnc)
	}

	// A real trace's worth of points, decoded back: no point may move more
	// than the encoding's own resolution.
	var route []trackPoint
	for i := 0; i < 500; i++ {
		route = append(route, trackPoint{Lat: fixtureLat + float64(i)*0.000173, Lon: fixtureLon - float64(i)*0.000211})
	}
	back := decodePolylineForTest(t, encodePolyline(route))
	if len(back) != len(route) {
		t.Fatalf("round trip returned %d points, want %d", len(back), len(route))
	}
	worst := 0.0
	for i := range route {
		// Degrees to metres is ~111 km per degree of latitude; a tenth of
		// that error budget is plenty at 1e-5 resolution.
		dlat := math.Abs(back[i].Lat-route[i].Lat) * 111_320
		dlon := math.Abs(back[i].Lon-route[i].Lon) * 111_320 * math.Cos(route[i].Lat*math.Pi/180)
		if d := math.Hypot(dlat, dlon); d > worst {
			worst = d
		}
	}
	if worst > 1.0 {
		t.Errorf("worst round-trip error %.3f m, want under a metre", worst)
	}
}

func decodePolylineForTest(t *testing.T, s string) []trackPoint {
	t.Helper()
	var out []trackPoint
	var lat, lon int
	i := 0
	read := func() int {
		shift, result := 0, 0
		for i < len(s) {
			b := int(s[i]) - 63
			i++
			result |= (b & 0x1f) << shift
			shift += 5
			if b < 0x20 {
				break
			}
		}
		if result&1 != 0 {
			return ^(result >> 1)
		}
		return result >> 1
	}
	for i < len(s) {
		lat += read()
		lon += read()
		out = append(out, trackPoint{Lat: float64(lat) / 1e5, Lon: float64(lon) / 1e5})
	}
	return out
}

// TestGraderPayloadIsNotThePagePayload is the split, pinned. The two were
// one builder, so a field added for a page arrived in the grader's context
// as a side effect — and a 9 KB polyline is ~4k tokens of high-entropy noise
// per turn for a small model on modest hardware. The grader's body carries
// the measured numbers and nothing a page invented.
func TestGraderPayloadIsNotThePagePayload(t *testing.T) {
	dir := t.TempDir()
	srv := fitTestMuxServer(t, "")
	srv.s.dataDir = dir
	const name = "2026-08-01-12-00-00.fit"
	if rec := post(srv.mux, "/api/activity?name="+name, gpsRun(t, typedef.SubSportGeneric)); rec.Code != http.StatusNoContent {
		t.Fatalf("POST = %d: %s", rec.Code, rec.Body)
	}

	out, code, msg := srv.s.activityMetricsPayload(name)
	if code != http.StatusOK {
		t.Fatalf("activityMetricsPayload = %d %s", code, msg)
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	// Exactly the keys the grader has always been given for this file. A new
	// key here is a change to what every future grade reads, and it must be
	// made deliberately rather than fall out of a page.
	want := map[string]bool{
		"name": true, "date": true, "sport": true, "start_utc": true,
		"elapsed_s": true, "elapsed_hms": true, "records": true,
		"distance_m": true, "dist": true, "pace": true, "sha256": true,
		"hr": true, "cadence": true, "profile": true, "grade_input": true,
		"decoupling_pct": true, "power": true, "weather": true,
		// Added deliberately 16 Aug 2026: where the clock stopped and for
		// how long, so a note stops guessing at it from the profile; and the
		// laps WITH their trigger, so a button press is not read as a rep.
		"stops": true, "stopped_s": true, "laps": true, "lap_count": true,
		"first_20min": true,
		// Added deliberately 17 Aug 2026: the moving clock beside the wall
		// clock, present only when a recording gap exists — a stop-free
		// file's payload is unchanged by construction. The statistics are
		// weighted over moving time (the gap rule), and against a prescribed
		// duration, moving is the number that answers "was the work done".
		"moving_s": true, "moving_hms": true,
	}
	for k := range got {
		if !want[k] {
			t.Errorf("the grader's payload gained %q — the page's builder is detailPayload", k)
		}
	}
	// The route NEVER goes: 9 KB of polyline is about 4k tokens of
	// high-entropy noise per turn for a small model on modest hardware, and
	// nothing about a grade depends on where the athlete was standing.
	// "laps" left this list on 16 Aug 2026, deliberately — a grade does
	// depend on them, and on the trigger that says what each one was.
	for _, forbidden := range []string{"track", "polyline", "splits", "paces", "moving_s", "ascent_m"} {
		if _, ok := got[forbidden]; ok {
			t.Errorf("%q reached the grader", forbidden)
		}
	}
	// And a lap the grader gets carries no position, for the same reason.
	if laps, ok := got["laps"].([]any); ok && len(laps) > 0 {
		first, _ := laps[0].(map[string]any)
		for _, k := range []string{"start", "end", "start_lat", "end_lat"} {
			if _, bad := first[k]; bad {
				t.Errorf("a lap carried %q into the grader's context", k)
			}
		}
		if _, ok := first["trigger"]; !ok {
			t.Error("a lap reached the grader without its trigger, which is the half that says what it was")
		}
	}
}

// TestActivityDetailAPI: the route, its validator and its compression. The
// bytes are immutable, so the response is a pure function of file, plan and
// build — and the origin compresses nothing, which is why this one does.
func TestActivityDetailAPI(t *testing.T) {
	mux := fitTestMux(t, t.TempDir())
	const name = "2026-08-01-12-00-00.fit"
	if rec := post(mux, "/api/activity?name="+name, gpsRun(t, typedef.SubSportGeneric)); rec.Code != http.StatusNoContent {
		t.Fatalf("POST = %d: %s", rec.Code, rec.Body)
	}

	rec := get(mux, "/api/activity-detail?name="+name, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d: %s", rec.Code, rec.Body)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" || rec.Header().Get("Vary") != "Accept-Encoding" {
		t.Errorf("validator headers: etag %q vary %q", etag, rec.Header().Get("Vary"))
	}
	var page map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page["date"] != "2026-08-01" || page["sport"] != "running" {
		t.Errorf("date/sport: %v/%v", page["date"], page["sport"])
	}

	if rec := get(mux, "/api/activity-detail?name="+name, map[string]string{"If-None-Match": etag}); rec.Code != http.StatusNotModified {
		t.Errorf("revalidation = %d, want 304", rec.Code)
	}

	// gzip is byte-identical after inflation — the encoding is a transport
	// detail and never a different answer.
	gzRec := get(mux, "/api/activity-detail?name="+name, map[string]string{"Accept-Encoding": "gzip"})
	if gzRec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("no gzip: %q", gzRec.Header().Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(bytes.NewReader(gzRec.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	inflated, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(inflated, rec.Body.Bytes()) {
		t.Error("the gzipped body inflates to something else")
	}
	if len(gzRec.Body.Bytes()) >= len(inflated) {
		t.Errorf("gzip made it bigger: %d vs %d", len(gzRec.Body.Bytes()), len(inflated))
	}

	if rec := get(mux, "/api/activity-detail?name=../x.fit", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("traversal name = %d", rec.Code)
	}
	if rec := get(mux, "/api/activity-detail?name=2026-01-01-00-00-00.fit", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown name = %d", rec.Code)
	}

	// A stored file that will not decode still 204s on import — bytes on
	// disk is the contract — and this route says so rather than 500ing.
	junk := "2026-08-02-12-00-00.fit"
	if rec := post(mux, "/api/activity?name="+junk, fitBytes("not really fit")); rec.Code != http.StatusNoContent {
		t.Fatalf("junk POST = %d", rec.Code)
	}
	if rec := get(mux, "/api/activity-detail?name="+junk, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("undecodable file = %d, want 422", rec.Code)
	}
}

// TestElevationRenders: metres in, whole units out, both systems. A tenth of
// a foot of climbing is noise on a figure a barometer guessed at.
func TestElevationRenders(t *testing.T) {
	for _, tc := range []struct {
		m                float64
		metric, imperial string
	}{
		{59, "59 m", "194 ft"},
		{124, "124 m", "407 ft"},
		{0, "0 m", "0 ft"},
		{1, "1 m", "3 ft"},
	} {
		if got := Elevation(tc.m).In(Metric); got != tc.metric {
			t.Errorf("%v m metric = %q, want %q", tc.m, got, tc.metric)
		}
		if got := Elevation(tc.m).In(Imperial); got != tc.imperial {
			t.Errorf("%v m imperial = %q, want %q", tc.m, got, tc.imperial)
		}
	}
}

// TestActivitiesByDate: the page needs the recordings for one day and their
// sports, so it can open the one matching the day's session — 121 archive
// dates carry more than one file. The unfiltered listing is what it always
// was, because the watch page reads it.
func TestActivitiesByDate(t *testing.T) {
	mux := fitTestMux(t, t.TempDir())
	post(mux, "/api/activity?name=2026-01-06-07-00-00.fit", gpsRun(t, typedef.SubSportGeneric))
	post(mux, "/api/activity?name=2026-01-06-17-00-00.fit", bikeFixture(t))
	// A DIFFERENT run on the second day. It used to be a second gpsRun, which
	// is byte-identical to the first — two days holding one recording, which
	// the cross-name dedupe now correctly refuses.
	post(mux, "/api/activity?name=2026-01-07-07-00-00.fit", tenSecondRun(t))

	var all []map[string]any
	if err := json.Unmarshal(get(mux, "/api/activities", nil).Body.Bytes(), &all); err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("unfiltered listing has %d, want 3", len(all))
	}
	if _, ok := all[0]["sport"]; ok {
		t.Error("the unfiltered listing gained a sport key the watch page never asked for")
	}

	var day []map[string]any
	if err := json.Unmarshal(get(mux, "/api/activities?date=2026-01-06", nil).Body.Bytes(), &day); err != nil {
		t.Fatal(err)
	}
	if len(day) != 2 {
		t.Fatalf("2026-01-06 listed %d recordings, want 2: %v", len(day), day)
	}
	sports := map[string]bool{}
	for _, a := range day {
		sports[a["sport"].(string)] = true
	}
	if !sports["running"] || !sports["cycling"] {
		t.Errorf("sports on the day: %v, want both", sports)
	}
	if rec := get(mux, "/api/activities?date=6-1-2026", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed date = %d, want 400", rec.Code)
	}
	if rec := get(mux, "/api/activities?date=2026-02-02", nil); rec.Code != http.StatusOK ||
		strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("a day with nothing recorded = %d %q, want an empty list", rec.Code, rec.Body.String())
	}
}

// bikeFixture is a minimal indoor ride: enough for a sport and a session.
func bikeFixture(t *testing.T) []byte {
	t.Helper()
	msgs := []proto.Message{
		mesgdef.NewRecord(nil).SetTimestamp(fixtureT0).SetSpeed(7000).SetPower(200).ToMesg(nil),
		mesgdef.NewRecord(nil).SetTimestamp(fixtureT0.Add(time.Second)).SetSpeed(7000).SetPower(210).ToMesg(nil),
		mesgdef.NewSession(nil).SetSport(typedef.SportCycling).
			SetSubSport(typedef.SubSportVirtualActivity).
			SetStartTime(fixtureT0).SetTimestamp(fixtureT0.Add(time.Minute)).
			SetTotalElapsedTime(2_000).SetTotalTimerTime(2_000).SetTotalDistance(1_400).ToMesg(nil),
	}
	return encodeActivityFixture(t, msgs...)
}

// TestCalendarOffersTheRecording: a day with a recording gets the affordance
// that opens it, and the script that draws it. A block with nothing recorded
// renders neither — the page pays for what it uses.
func TestCalendarOffersTheRecording(t *testing.T) {
	dir := t.TempDir()
	mux := fitTestMux(t, dir)

	body := get(mux, "/calendar", nil).Body.String()
	if strings.Contains(body, "data-detail=") {
		t.Error("an empty archive still offered a recording to open")
	}
	if strings.Contains(body, "detail.") && strings.Contains(body, "<script") {
		t.Error("an empty archive loaded detail.js")
	}

	// The example block runs from 2026-01-05; its second day is a quality run.
	if rec := post(mux, "/api/activity?name=2026-01-06-07-00-00.fit", gpsRun(t, typedef.SubSportGeneric)); rec.Code != http.StatusNoContent {
		t.Fatalf("POST = %d: %s", rec.Code, rec.Body)
	}
	body = get(mux, "/calendar", nil).Body.String()
	if !strings.Contains(body, `data-detail="2026-01-06"`) {
		t.Error("the recorded day carries no trigger")
	}
	if !strings.Contains(body, `data-sport="running"`) {
		t.Error("the trigger does not say which sport the session was")
	}
	if !regexp.MustCompile(`<script src="/static/detail\.[0-9a-f]+\.js"`).MatchString(body) {
		t.Error("detail.js is not loaded on a calendar that has a trigger")
	}
	// One recording is one trigger: every other day of a two-week block is
	// still an ordinary cell.
	if n := strings.Count(body, "data-detail="); n != 1 {
		t.Errorf("%d triggers, want exactly the recorded day's", n)
	}
}

// TestStepIndicesAddressTheEmittedFile: the join reads a lap's
// wkt_step_index as an index into flattenSteps, so that numbering has to be
// the one the encoder writes into the file — body-first with the repeat step
// trailing. Encode a workout with a repeat, decode it back, and hold the two
// against each other index by index. If they ever disagree the popover shows
// the wrong step's target beside every rep, silently.
func TestStepIndicesAddressTheEmittedFile(t *testing.T) {
	steps := []resolvedStep{
		{Role: "warmup", Secs: 600},
		{Repeat: 3, Body: []resolvedStep{
			{Role: "active", Secs: 240, PaceFast: 0.24, PaceSlow: 0.26},
			{Role: "recovery", Secs: 120},
		}},
		{Role: "cooldown", DistM: 1000},
	}
	em := flattenSteps(steps)
	want := []string{"warmup", "active", "recovery", "REPEAT", "cooldown"}
	if len(em) != len(want) {
		t.Fatalf("%d emitted steps, want %d", len(em), len(want))
	}
	for i, w := range want {
		got := em[i].Leaf.Role
		if em[i].IsRepeat {
			got = "REPEAT"
		}
		if got != w {
			t.Errorf("emitted %d is %q, want %q", i, got, w)
		}
	}
	if em[1].Group != 1 || em[1].Reps != 3 || em[3].First != 1 || em[3].Times != 3 {
		t.Errorf("repeat bookkeeping: %+v / %+v", em[1], em[3])
	}

	w := fitWorkoutFor("W02 Tu Reps", steps, fitSportRunning, 0x1234, fixtureT0)
	data, err := w.Encode()
	if err != nil {
		t.Fatal(err)
	}
	dec, err := decodeWorkoutSteps(t, data)
	if err != nil {
		t.Fatal(err)
	}
	if len(dec) != len(em) {
		t.Fatalf("the file carries %d steps, flattenSteps says %d", len(dec), len(em))
	}
	for i, d := range dec {
		if d.index != i {
			t.Errorf("step %d carries message_index %d — the join addresses by position", i, d.index)
		}
		if em[i].IsRepeat {
			if d.durationType != fitDurationRepeat {
				t.Errorf("emitted %d should be the repeat marker, file says duration type %d", i, d.durationType)
			}
			continue
		}
		if d.intensity != fitIntensities[em[i].Leaf.Role] {
			t.Errorf("emitted %d is %q, file says intensity %d", i, em[i].Leaf.Role, d.intensity)
		}
	}
}

type decodedStep struct {
	index        int
	intensity    uint8
	durationType uint8
}

func decodeWorkoutSteps(t *testing.T, data []byte) ([]decodedStep, error) {
	t.Helper()
	fit, err := decoder.New(bytes.NewReader(data)).Decode()
	if err != nil {
		return nil, err
	}
	var out []decodedStep
	for i := range fit.Messages {
		m := &fit.Messages[i]
		if m.Num != mesgnum.WorkoutStep {
			continue
		}
		d := decodedStep{index: -1}
		for _, f := range m.Fields {
			switch f.Num {
			case 254: // message_index
				d.index = int(f.Value.Uint16())
			case 7: // intensity
				d.intensity = f.Value.Uint8()
			case 1: // duration_type
				d.durationType = f.Value.Uint8()
			}
		}
		out = append(out, d)
	}
	return out, nil
}

// steppedRun builds a recording of the example block's quality Tuesday as if
// the watch had driven the pushed workout: a warm-up lap, then reps and
// recoveries carrying the step index the file would stamp on each. paces are
// given per rep in seconds per metre so a test can place a lap inside or
// outside the step's own band without restating the band.
func steppedRun(t *testing.T, repPaces []float64) []byte {
	t.Helper()
	var msgs []proto.Message
	sec := 0
	rec := func(n int) {
		for i := 0; i < n; i++ {
			msgs = append(msgs, runRecord(sec, 150, 3000, 85))
			sec++
		}
	}
	rec(3)
	lap := func(start, secs int, distM float64, step uint16) proto.Message {
		return lapMsg(start, uint32(secs*1000), uint32(secs*1000), uint32(distM*100),
			typedef.LapTriggerTime).
			SetWktStepIndex(typedef.MessageIndex(step)).
			SetAvgHeartRate(150).ToMesg(nil)
	}
	at := 0
	msgs = append(msgs, lap(at, 600, 2000, 0)) // the warm-up, step 0
	at += 600
	for i, p := range repPaces {
		secs := 240
		if i == len(repPaces)-1 && p < 0 { // a negative pace marks a rep cut short
			secs = 60
			p = -p
		}
		msgs = append(msgs, lap(at, secs, float64(secs)/p, 1)) // step 1: the rep
		at += secs
		msgs = append(msgs, lap(at, 120, 300, 2)) // step 2: the recovery
		at += 120
	}
	msgs = append(msgs, mesgdef.NewSession(nil).
		SetSport(typedef.SportRunning).
		SetStartTime(fixtureT0).
		SetTimestamp(fixtureT0.Add(time.Minute)).
		SetTotalElapsedTime(uint32(at*1000)).
		SetTotalTimerTime(uint32(at*1000)).
		SetTotalDistance(800_000).ToMesg(nil))
	return encodeActivityFixture(t, msgs...)
}

// TestChartPowerBand pins the watts panel's target selection: the longest
// active step's band, repeats and powerless steps ignored, the %FTP labels
// derived from the anchor and absent when no FTP is declared. The example
// block carries no bike steps day, so this tests the selection directly —
// the resolution path it feeds from is TestPrescribedJoinsLapsToSteps's.
func TestChartPowerBand(t *testing.T) {
	step := func(role string, secs, lo, hi int) emittedStep {
		return emittedStep{Leaf: resolvedStep{Role: role, Secs: secs, PowerLo: lo, PowerHi: hi}}
	}
	em := []emittedStep{
		step("warmup", 300, 107, 133),   // a warm-up band is not the target
		step("active", 240, 180, 200),   // a shorter interval…
		step("active", 2700, 133, 146),  // …loses to the main set
		step("recovery", 120, 100, 120), // recoveries say nothing
		{IsRepeat: true, Leaf: resolvedStep{Role: "active", Secs: 9999, PowerLo: 1, PowerHi: 2}},
		step("active", 600, 0, 0), // no power band, nothing to draw
	}
	band, pct := chartPowerBand(em, 214)
	if band == nil || band[0] != 133 || band[1] != 146 {
		t.Fatalf("band = %v, want [133 146]", band)
	}
	// 133/214 = 62.1%, 146/214 = 68.2% — the numbers the grade note quotes.
	if pct == nil || pct[0] != 62 || pct[1] != 68 {
		t.Errorf("pct = %v, want [62 68]", pct)
	}
	if _, pctFree := chartPowerBand(em, 0); pctFree != nil {
		t.Error("an athlete with no FTP got a pct-of-FTP label")
	}
	if bandFree, _ := chartPowerBand(em[3:], 200); bandFree != nil {
		t.Errorf("no active step with power, yet band = %v", bandFree)
	}
}

// TestPrescribedJoinsLapsToSteps is the phase's whole point: a lap the watch
// drove says which prescribed step it was, and the block says what that step
// asked for. Prescribed against delivered, rep by rep — the reading a general
// activity site cannot make, because it does not have the plan.
func TestPrescribedJoinsLapsToSteps(t *testing.T) {
	dir := t.TempDir()
	srv := fitTestMuxServer(t, "")
	srv.s.dataDir = dir

	// The example block's quality Tuesday: warm-up, then 3 × (4:00 at a pace
	// band, recovery). Read the band out of the plan rather than restating it.
	d := srv.s.ds()
	blk := d.Current(srv.s.day(d))
	day := time.Date(2026, 1, 13, 0, 0, 0, 0, d.Loc)
	wk, di, ok := blk.Locate(day)
	if !ok {
		t.Skip("the example block does not cover 2026-01-13")
	}
	sess := wk.Days[di]
	rs, err := resolveSteps(blk.ctxFor(d.Athlete, wk.N).forSession(&sess), sess)
	if err != nil {
		t.Fatal(err)
	}
	em := flattenSteps(rs)
	if len(em) < 3 || em[1].Leaf.Role != "active" || em[1].Leaf.PaceFast == 0 {
		t.Skipf("the example quality day is no longer a paced repeat: %+v", em)
	}
	inBand := (float64(em[1].Leaf.PaceFast) + float64(em[1].Leaf.PaceSlow)) / 2
	tooSlow := float64(em[1].Leaf.PaceSlow) * 1.15

	const name = "2026-01-13-06-30-00.fit"
	if rec := post(srv.mux, "/api/activity?name="+name,
		steppedRun(t, []float64{inBand, tooSlow, -inBand})); rec.Code != http.StatusNoContent {
		t.Fatalf("POST = %d: %s", rec.Code, rec.Body)
	}
	out, code, msg := srv.s.detailPayload(name, "")
	if code != http.StatusOK {
		t.Fatalf("detailPayload = %d %s", code, msg)
	}

	if out.Session == nil || out.Session.RepsAsked != em[1].Reps {
		t.Fatalf("session: %+v, want the day's %d reps", out.Session, em[1].Reps)
	}
	// Two ran the full four minutes; the third was cut off after one.
	if out.Session.RepsDone != 2 {
		t.Errorf("reps done = %d, want 2 — the third lap covered a quarter of its step", out.Session.RepsDone)
	}
	want := []struct{ label, verdict string }{
		{"W-up", ""},
		{"Rep 1", "in"},
		{"Rec 1", ""},
		{"Rep 2", "under"},
		{"Rec 2", ""},
		{"Rep 3", "short"},
		{"Rec 3", ""},
	}
	if len(out.Laps) != len(want) {
		t.Fatalf("%d laps, want %d", len(out.Laps), len(want))
	}
	for i, w := range want {
		p := out.Laps[i].Prescribed
		if p == nil {
			t.Errorf("lap %d joined to no step", i+1)
			continue
		}
		if p.Label != w.label {
			t.Errorf("lap %d is %q, want %q", i+1, p.Label, w.label)
		}
		if w.verdict != "" && p.Verdict != w.verdict {
			t.Errorf("lap %d (%s) verdict %q, want %q", i+1, p.Label, p.Verdict, w.verdict)
		}
		if p.Label == "Rep 1" && p.Target == "" {
			t.Error("a rep carries no target, so there is nothing to read it against")
		}
	}
}

// TestJoinDegradesWithoutAPushedWorkout: the join is a bonus, never a
// requirement. A recording with no step indices — a Zwift ride is one lap
// with none — still gets its day's prescription named, and every lap stands
// on its own numbers.
func TestJoinDegradesWithoutAPushedWorkout(t *testing.T) {
	dir := t.TempDir()
	srv := fitTestMuxServer(t, "")
	srv.s.dataDir = dir
	const name = "2026-01-13-06-30-00.fit"
	if rec := post(srv.mux, "/api/activity?name="+name, gpsRun(t, typedef.SubSportGeneric)); rec.Code != http.StatusNoContent {
		t.Fatalf("POST = %d: %s", rec.Code, rec.Body)
	}
	out, code, msg := srv.s.detailPayload(name, "")
	if code != http.StatusOK {
		t.Fatalf("detailPayload = %d %s", code, msg)
	}
	if out.Session == nil || out.Session.Label == "" {
		t.Fatal("the day's prescription is not named")
	}
	for _, l := range out.Laps {
		// gpsRun's second lap carries a step index of its own invention; the
		// join may name it, but nothing here may fail for want of one.
		_ = l
	}
	if out.Session.RepsDone > out.Session.RepsAsked {
		t.Errorf("more reps delivered than asked: %+v", out.Session)
	}

	// A ride recorded on a run day is not that day's session: naming its laps
	// after the run's steps would invent a reading.
	const bike = "2026-01-13-17-00-00.fit"
	if rec := post(srv.mux, "/api/activity?name="+bike, bikeFixture(t)); rec.Code != http.StatusNoContent {
		t.Fatalf("bike POST = %d: %s", rec.Code, rec.Body)
	}
	out, code, _ = srv.s.detailPayload(bike, "")
	if code != http.StatusOK {
		t.Fatalf("bike detailPayload = %d", code)
	}
	for i, l := range out.Laps {
		if l.Prescribed != nil {
			t.Errorf("lap %d of a ride was joined to a run day's steps: %+v", i+1, l.Prescribed)
		}
	}
}

// TestNotesTravelOnlyWhenSomethingWasSaid: the athlete's own account of a
// day is the half no recording carries — on 12 Aug 2026 the file shows two
// of four reps and a stop inside the second, and only the note says the
// chain came off. It travels when it exists, and the key is absent when
// nothing was said, so a page cannot render a heading over nothing.
func TestNotesTravelOnlyWhenSomethingWasSaid(t *testing.T) {
	dir := t.TempDir()
	srv := fitTestMuxServer(t, "")
	srv.s.dataDir = dir
	const name = "2026-01-13-06-30-00.fit"
	if rec := post(srv.mux, "/api/activity?name="+name, gpsRun(t, typedef.SubSportGeneric)); rec.Code != http.StatusNoContent {
		t.Fatalf("POST = %d: %s", rec.Code, rec.Body)
	}

	out, _, _ := srv.s.detailPayload(name, "")
	if len(out.Notes) != 0 {
		t.Fatalf("a day nobody wrote about carries %d notes", len(out.Notes))
	}
	body := get(srv.mux, "/api/activity-detail?name="+name, nil).Body.String()
	if strings.Contains(body, `"notes"`) {
		t.Error("the payload carries an empty notes key, which reads as a note that exists")
	}

	for _, e := range []Entry{
		{Kind: "note", Date: "2026-01-13", Note: "Chain came off in rep 2; ERG would not re-engage."},
		{Kind: "task", Date: "2026-01-13", Key: "session", Val: "done", Note: "legs felt flat"},
		{Kind: "grade", Date: "2026-01-13", Val: "C", Note: "the verdict, which is not the athlete talking"},
		{Kind: "note", Date: "2026-01-14", Note: "another day entirely"},
	} {
		if err := srv.s.store.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	out, _, _ = srv.s.detailPayload(name, "")
	if len(out.Notes) != 2 {
		t.Fatalf("%d notes, want the day's two: %+v", len(out.Notes), out.Notes)
	}
	if !strings.Contains(out.Notes[0].Note, "Chain came off") || out.Notes[1].Kind != "task" {
		t.Errorf("notes are not the day's own, oldest first: %+v", out.Notes)
	}
	for _, n := range out.Notes {
		if n.Kind == "grade" {
			t.Error("the grade's note came through as the athlete's account of the day")
		}
	}
}

// TestElapsedFallsBackToTheRecords: a session message that carries no
// total_elapsed_time is not a zero-second activity. The span of the records
// is the fallback, and it must not depend on position — an indoor ride has
// no fixes, and one reported "0:00 elapsed" beside 7.77 miles.
func TestElapsedFallsBackToTheRecords(t *testing.T) {
	var msgs []proto.Message
	for i := 0; i <= 600; i++ {
		msgs = append(msgs, mesgdef.NewRecord(nil).
			SetTimestamp(fixtureT0.Add(time.Duration(i)*time.Second)).
			SetSpeed(7000).SetPower(200).ToMesg(nil))
	}
	// A session with a distance and a sport, and no clock of its own.
	msgs = append(msgs, mesgdef.NewSession(nil).
		SetSport(typedef.SportCycling).
		SetSubSport(typedef.SubSportIndoorCycling).
		SetStartTime(fixtureT0).
		SetTimestamp(fixtureT0.Add(time.Minute)).
		SetTotalDistance(420_000).ToMesg(nil))

	d := detailOf(t, encodeActivityFixture(t, msgs...))
	if d.ElapsedS != 600 || d.ElapsedHMS != "10:00" {
		t.Errorf("elapsed %v (%q), want the 600 s the records cover", d.ElapsedS, d.ElapsedHMS)
	}
	if d.MovingS != 0 {
		t.Errorf("moving %v — the file states no timer time and none may be invented", d.MovingS)
	}
}

// TestChartBucketsMedianAndBreaks: the chart's series is the recording
// bucketed to about one point per pixel, each point the MEDIAN of its
// bucket. Median because one dropped heart-rate sample or one GPS spike
// moves a mean and does not move a median. A bucket with nothing of its own
// is null so the line breaks where the recording did — a smoothing window is
// not a licence to draw across a stop — and watts are read from their own
// bucket alone, because ERG holds a square wave and rounding its corners off
// would hide the very steps the session was prescribed as.
func TestChartBucketsMedianAndBreaks(t *testing.T) {
	var msgs []proto.Message
	add := func(sec int, hr uint8, speedRaw uint16, watts uint16) {
		msgs = append(msgs, mesgdef.NewRecord(nil).
			SetTimestamp(fixtureT0.Add(time.Duration(sec)*time.Second)).
			SetHeartRate(hr).SetSpeed(speedRaw).SetPower(watts).ToMesg(nil))
	}
	// Five minutes at 3 m/s and 140 bpm, 100 W.
	for sec := 0; sec <= 300; sec++ {
		add(sec, 140, 3000, 100)
	}
	// Five minutes the recording does not describe at all.
	// Then five more at 5 m/s, a dropout heart rate, and a step to 250 W.
	for sec := 600; sec <= 900; sec++ {
		add(sec, 40, 5000, 250)
	}
	msgs = append(msgs, mesgdef.NewSession(nil).
		SetSport(typedef.SportCycling).SetSubSport(typedef.SubSportIndoorCycling).
		SetStartTime(fixtureT0).SetTimestamp(fixtureT0.Add(time.Minute)).
		SetTotalElapsedTime(900_000).SetTotalTimerTime(900_000).
		SetTotalDistance(300_000).ToMesg(nil))

	d := detailOf(t, encodeActivityFixture(t, msgs...))
	c := d.Chart
	if c == nil {
		t.Fatal("no chart series")
	}
	if c.Unit != "/km" { // the example athlete is metric
		t.Errorf("pace unit %q, want the athlete's own", c.Unit)
	}
	if len(c.Secs) < 100 {
		t.Fatalf("%d points for a 900 s recording", len(c.Secs))
	}

	at := func(sec int) int {
		best := 0
		for i, s := range c.Secs {
			if s <= sec {
				best = i
			}
		}
		return best
	}
	// The gap: nothing was recorded, so nothing is drawn.
	for _, sec := range []int{420, 480, 540} {
		i := at(sec)
		if c.Pace[i] != nil {
			t.Errorf("%d s sits inside the gap and carries a pace of %v", sec, *c.Pace[i])
		}
		if c.HR[i] != nil {
			t.Errorf("%d s sits inside the gap and carries a heart rate", sec)
		}
	}
	// The first half: 3 m/s is 333.3 s per kilometre.
	if p := c.Pace[at(150)]; p == nil || math.Abs(*p-333.3) > 1 {
		t.Errorf("pace at 150 s = %v, want ~333.3 s/km", p)
	}
	// The second half: 5 m/s is 200 s per kilometre, and 40 bpm is a dropout
	// the register excludes everywhere.
	if p := c.Pace[at(800)]; p == nil || math.Abs(*p-200) > 1 {
		t.Errorf("pace at 800 s = %v, want ~200 s/km", p)
	}
	if hr := c.HR[at(800)]; hr != nil {
		t.Errorf("a 40 bpm dropout was drawn as a heart rate of %d", *hr)
	}
	if hr := c.HR[at(150)]; hr == nil || *hr != 140 {
		t.Errorf("heart rate at 150 s = %v, want 140", hr)
	}
	// The watt step stays square: 100 before the gap, 250 after, with no
	// bucket carrying a blend of the two.
	if w := c.Watts[at(150)]; w == nil || *w != 100 {
		t.Errorf("watts at 150 s = %v, want 100", w)
	}
	if w := c.Watts[at(800)]; w == nil || *w != 250 {
		t.Errorf("watts at 800 s = %v, want 250", w)
	}
	for i, w := range c.Watts {
		if w != nil && *w != 100 && *w != 250 {
			t.Errorf("bucket %d carries %d W, which is neither step — the square wave was smoothed", i, *w)
		}
	}
}

// TestRouteIsCutAtLapBoundaries: his outdoor runs are out-and-backs — 57 to
// 87% of their points retrace themselves within 15 m — so a single stroke
// says nothing about which stretch was which. The route is served cut at the
// laps the watch recorded, each segment repeating its predecessor's last
// point so the drawn line has no hole at the seam.
func TestRouteIsCutAtLapBoundaries(t *testing.T) {
	// Half an hour of positions walking north, lapped every ten minutes.
	var msgs []proto.Message
	for sec := 0; sec <= 1800; sec += 10 {
		msgs = append(msgs, mesgdef.NewRecord(nil).
			SetTimestamp(fixtureT0.Add(time.Duration(sec)*time.Second)).
			SetHeartRate(150).SetSpeed(3000).
			SetPositionLat(semicircles(fixtureLat+float64(sec)*0.00002)).
			SetPositionLong(semicircles(fixtureLon)).ToMesg(nil))
	}
	for i, start := range []int{0, 600, 1200} {
		msgs = append(msgs, lapMsg(start, 600_000, 600_000, 160_934, typedef.LapTriggerDistance).
			SetMessageIndex(typedef.MessageIndex(i)).ToMesg(nil))
	}
	msgs = append(msgs, mesgdef.NewSession(nil).
		SetSport(typedef.SportRunning).SetStartTime(fixtureT0).
		SetTimestamp(fixtureT0.Add(time.Minute)).
		SetTotalElapsedTime(1_800_000).SetTotalTimerTime(1_800_000).
		SetTotalDistance(482_802).ToMesg(nil))

	d := detailOf(t, encodeActivityFixture(t, msgs...))
	if len(d.Track.Segments) != 3 {
		t.Fatalf("%d segments for three laps: %+v", len(d.Track.Segments), d.Track.Segments)
	}
	if d.Track == nil || len(d.Track.Segments) < 2 {
		t.Fatalf("track: %+v", d.Track)
	}
	var prevEnd []float64
	for i, seg := range d.Track.Segments {
		pts := decodePolylineForTest(t, seg.Polyline)
		if len(pts) < 2 {
			t.Errorf("segment %d carries %d points", i, len(pts))
			continue
		}
		if i > 0 {
			first := []float64{pts[0].Lat, pts[0].Lon}
			if math.Abs(first[0]-prevEnd[0]) > 1e-5 || math.Abs(first[1]-prevEnd[1]) > 1e-5 {
				t.Errorf("segment %d starts at %v, not where segment %d ended (%v) — the line has a hole",
					i, first, i-1, prevEnd)
			}
		}
		last := pts[len(pts)-1]
		prevEnd = []float64{last.Lat, last.Lon}
	}
	// Lap numbers travel so a segment can say which mile it was.
	if d.Track.Segments[len(d.Track.Segments)-1].Lap == 0 {
		t.Error("no segment carries a lap number")
	}
}

// TestGraderLapsCarryTheirProvenance: a lap means nothing without the
// trigger that made it. The watch's own auto-lap is a mile split, a manual
// lap is the athlete pressing the button — twice in two seconds, on
// 12 Aug 2026 — and a lap driven by a pushed workout carries the index of
// the step it was. Without that, a 2.4-second press reads as a
// catastrophically failed rep.
func TestGraderLapsCarryTheirProvenance(t *testing.T) {
	dir := t.TempDir()
	srv := fitTestMuxServer(t, "")
	srv.s.dataDir = dir
	const name = "2026-01-13-06-30-00.fit"
	if rec := post(srv.mux, "/api/activity?name="+name, gpsRun(t, typedef.SubSportGeneric)); rec.Code != http.StatusNoContent {
		t.Fatalf("POST = %d: %s", rec.Code, rec.Body)
	}
	out, code, msg := srv.s.activityMetricsPayload(name)
	if code != http.StatusOK {
		t.Fatalf("payload = %d %s", code, msg)
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Laps []struct {
			N        int     `json:"n"`
			Trigger  string  `json:"trigger"`
			Step     *int    `json:"step"`
			Dist     string  `json:"dist"`
			TimerS   float64 `json:"timer_s"`
			ElapsedS float64 `json:"elapsed_s"`
			Pace     string  `json:"pace"`
			AvgHR    *int    `json:"avg_hr"`
		} `json:"laps"`
		LapCount int `json:"lap_count"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.LapCount != 3 || len(got.Laps) != 3 {
		t.Fatalf("%d laps (count %d), want 3", len(got.Laps), got.LapCount)
	}
	if got.Laps[0].Trigger != "distance" || got.Laps[0].Pace == "" {
		t.Errorf("the auto-lap lost its trigger or its pace: %+v", got.Laps[0])
	}
	if s := got.Laps[1].Step; s == nil || *s != 3 {
		t.Errorf("the pushed workout's lap lost its step index: %v", s)
	}
	// The button press: it keeps its clock and it is visibly not a rep.
	if b := got.Laps[2]; b.Trigger != "manual" || b.TimerS != 1.1 || b.Pace != "" {
		t.Errorf("the button press reads as something else: %+v", b)
	}
}

// TestTheDetailValidatorCoversTheGrade: the ETag has to change when
// anything in the response does. It used to hash (file, plan, build) —
// correct while the payload was a pure function of those, and silently
// wrong the moment it carried the day's grade, which moves when none of
// them do. The failure was total and invisible: the popover polls this
// endpoint to show a re-grade as it lands, every poll revalidated to 304,
// and the browser handed back a body from before the grade existed.
func TestTheDetailValidatorCoversTheGrade(t *testing.T) {
	ts := fitTestMuxServer(t, t.TempDir())
	const name, date = "2026-01-13-12-00-00.fit", "2026-01-13"
	body := week2Run(t, 13)
	if _, err := ts.s.metrics.importOne(name, body, time.UTC, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ts.s.activitiesDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ts.s.activitiesDir(), name), body, 0o644); err != nil {
		t.Fatal(err)
	}
	url := "/api/activity-detail?name=" + name

	first := get(ts.mux, url, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first GET = %d", first.Code)
	}
	tag := first.Header().Get("ETag")
	if tag == "" {
		t.Fatal("no validator at all")
	}
	// Unchanged: the conditional request must still be cheap.
	if rec := get(ts.mux, url, map[string]string{"If-None-Match": tag}); rec.Code != http.StatusNotModified {
		t.Errorf("an unchanged response did not revalidate: %d", rec.Code)
	}

	// Now the day gets a grade. Nothing about the FILE, the plan or the
	// build has changed.
	if err := ts.s.store.Append(Entry{Date: date, Kind: "grade", Val: "B", Note: "first"}); err != nil {
		t.Fatal(err)
	}
	rec := get(ts.mux, url, map[string]string{"If-None-Match": tag})
	if rec.Code != http.StatusOK {
		t.Fatalf("a graded day still revalidated to %d — the page can never see a new grade", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"grade":"B"`) {
		t.Errorf("the fresh body carries no grade: %.200s", rec.Body.String())
	}
	second := rec.Header().Get("ETag")
	if second == tag {
		t.Error("the validator did not move when the response did")
	}

	// And a re-grade to the SAME letter must move it too, or a supersede is
	// invisible.
	time.Sleep(1100 * time.Millisecond) // the log stamps whole seconds
	if err := ts.s.store.Append(Entry{Date: date, Kind: "grade", Val: "B", Note: "second"}); err != nil {
		t.Fatal(err)
	}
	rec = get(ts.mux, url, map[string]string{"If-None-Match": second})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "second") {
		t.Errorf("a same-letter re-grade was invisible: %d", rec.Code)
	}
}
