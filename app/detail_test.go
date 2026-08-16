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
	"testing"
	"time"

	"github.com/muktihari/fit/profile/mesgdef"
	"github.com/muktihari/fit/profile/typedef"
	"github.com/muktihari/fit/proto"
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
			SetPositionLat(semicircles(44.9484+float64(i)*0.00001)).
			SetPositionLong(semicircles(-93.3405)).ToMesg(nil))
	}
	msgs = append(msgs,
		lapMsg(0, 592_225, 592_225, 160_934, typedef.LapTriggerDistance).
			SetAvgHeartRate(137).SetMaxHeartRate(150).
			SetAvgCadence(85).SetAvgPower(294).SetMaxPower(397).
			SetTotalAscent(12).
			SetEnhancedAvgSpeed(2717).
			SetStartPositionLat(semicircles(44.9484)).
			SetStartPositionLong(semicircles(-93.3405)).
			SetEndPositionLat(semicircles(44.9435)).
			SetEndPositionLong(semicircles(-93.3414)).ToMesg(nil),
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
	out, code, msg := srv.s.detailPayload(name)
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
	pts := []trackPoint{{38.5, -120.2}, {40.7, -120.95}, {43.252, -126.453}}
	if got := encodePolyline(pts); got != wantEnc {
		t.Fatalf("encodePolyline = %q, want the published example %q", got, wantEnc)
	}

	// A real trace's worth of points, decoded back: no point may move more
	// than the encoding's own resolution.
	var route []trackPoint
	for i := 0; i < 500; i++ {
		route = append(route, trackPoint{44.9484 + float64(i)*0.000173, -93.3405 - float64(i)*0.000211})
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
		out = append(out, trackPoint{float64(lat) / 1e5, float64(lon) / 1e5})
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
		"first_20min": true,
	}
	for k := range got {
		if !want[k] {
			t.Errorf("the grader's payload gained %q — the page's builder is detailPayload", k)
		}
	}
	for _, forbidden := range []string{"laps", "track", "polyline", "splits", "paces", "moving_s", "ascent_m"} {
		if _, ok := got[forbidden]; ok {
			t.Errorf("%q reached the grader", forbidden)
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
