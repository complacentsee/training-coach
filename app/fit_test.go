package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/muktihari/fit/decoder"
	"github.com/muktihari/fit/profile/filedef"
)

/*
Placement rule: every test that imports muktihari/fit lives in this file, and
nothing else imports it. That keeps the dependency test-only by inspection as
well as by the Dockerfile's go-version -m proof.
*/

var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/workout.fit from the encoder")

// decodeWorkout round-trips encoder output through the independent decoder,
// CRC checking included (the decoder's default).
func decodeWorkout(t *testing.T, b []byte) *filedef.Workout {
	t.Helper()
	fit, err := decoder.New(bytes.NewReader(b)).Decode()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return filedef.NewWorkout(fit.Messages...)
}

// fixedIdentity is a valid, arbitrary identity for tests that are not about
// identity.
func fixedIdentity() (uint32, time.Time) {
	return 12345, time.Date(2026, 3, 1, 10, 30, 0, 0, time.UTC)
}

func TestFitRoundTripPreservesEveryStepField(t *testing.T) {
	serial, created := fixedIdentity()
	steps := []resolvedStep{
		{Role: "warmup", DistM: 3218.688, HRLo: 140, HRHi: 150, Note: "Hold back"},
		{Repeat: 2, Body: []resolvedStep{
			{Role: "active", Secs: 90, PaceFast: 0.27, PaceSlow: 0.28, Note: "Smooth"},
			{Role: "recovery", Secs: 60},
		}},
		{Role: "active", DistM: 1000, HRLo: 142, HRHi: 155},
		{Role: "cooldown", Secs: 600},
	}
	w := fitWorkoutFor("W01 Tu Round Trip", steps, fitSportRunning, serial, created)
	b, err := w.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got := decodeWorkout(t, b)

	if uint8(got.FileId.Type) != fitFileWorkout {
		t.Errorf("file_id.type = %d, want %d", got.FileId.Type, fitFileWorkout)
	}
	if uint16(got.FileId.Manufacturer) != fitManufacturerGarmin {
		t.Errorf("file_id.manufacturer = %d, want %d", got.FileId.Manufacturer, fitManufacturerGarmin)
	}
	if got.FileId.Product != fitProduct {
		t.Errorf("file_id.product = %d, want %d", got.FileId.Product, fitProduct)
	}
	if got.FileId.SerialNumber != serial {
		t.Errorf("file_id.serial_number = %d, want %d", got.FileId.SerialNumber, serial)
	}
	if !got.FileId.TimeCreated.Equal(created) {
		t.Errorf("file_id.time_created = %s, want %s", got.FileId.TimeCreated, created)
	}
	if got.Workout == nil {
		t.Fatal("no workout message decoded")
	}
	if got.Workout.WktName != "W01 Tu Round Trip" {
		t.Errorf("wkt_name = %q — the encoder must write the name verbatim", got.Workout.WktName)
	}
	if uint8(got.Workout.Sport) != fitSportRunning {
		t.Errorf("sport = %d, want running", got.Workout.Sport)
	}
	// 3 top-level leaves + 2 repeated leaves + the repeat message itself.
	if got.Workout.NumValidSteps != 6 {
		t.Errorf("num_valid_steps = %d, want 6 — the repeat message counts", got.Workout.NumValidSteps)
	}
	if len(got.WorkoutSteps) != 6 {
		t.Fatalf("decoded %d steps, want 6", len(got.WorkoutSteps))
	}

	want := []struct {
		durType uint8
		durVal  uint32
		tgtType uint8
		tgtVal  uint32
		lo, hi  uint32 // checked only when the target carries a custom range
		custom  bool
		intens  uint8
		notes   string
	}{
		// 2 mi → 321869 cm; HR 140–150 → 240–250.
		{fitDurationDistance, 321869, fitTargetHR, 0, 240, 250, true, 2, "Hold back"},
		// 90 s → 90000 ms; 4:30/km and 4:40/km → 3704 and 3571 mm/s, slower
		// speed in CustomLow.
		{fitDurationTime, 90000, fitTargetSpeed, 0, 3571, 3704, true, 0, "Smooth"},
		{fitDurationTime, 60000, fitTargetOpen, 0, 0, 0, false, 4, ""},
		// The repeat: duration_value is the first child's message_index.
		{fitDurationRepeat, 1, fitTargetOpen, 2, 0, 0, false, 0, ""},
		{fitDurationDistance, 100000, fitTargetHR, 0, 242, 255, true, 0, ""},
		{fitDurationTime, 600000, fitTargetOpen, 0, 0, 0, false, 3, ""},
	}
	for i, x := range want {
		g := got.WorkoutSteps[i]
		if uint16(g.MessageIndex) != uint16(i) {
			t.Errorf("step %d: message_index = %d", i, g.MessageIndex)
		}
		if uint8(g.DurationType) != x.durType || g.DurationValue != x.durVal {
			t.Errorf("step %d: duration %d/%d, want %d/%d", i, g.DurationType, g.DurationValue, x.durType, x.durVal)
		}
		if uint8(g.TargetType) != x.tgtType || g.TargetValue != x.tgtVal {
			t.Errorf("step %d: target %d/%d, want %d/%d", i, g.TargetType, g.TargetValue, x.tgtType, x.tgtVal)
		}
		if x.custom && (g.CustomTargetValueLow != x.lo || g.CustomTargetValueHigh != x.hi) {
			t.Errorf("step %d: custom range %d–%d, want %d–%d", i, g.CustomTargetValueLow, g.CustomTargetValueHigh, x.lo, x.hi)
		}
		if uint8(g.Intensity) != x.intens {
			t.Errorf("step %d: intensity %d, want %d", i, g.Intensity, x.intens)
		}
		if g.Notes != x.notes {
			t.Errorf("step %d: notes %q, want %q", i, g.Notes, x.notes)
		}
	}
}

// A trainer workout: sport=cycling, and power bands as watts offset by 1000
// (FIT reads values at or under 1000 as %FTP — the offset is what keeps 214 W
// meaning watts). ERG drives to these ranges, so the offset being wrong would
// coach in percent invisibly; the decode pins it.
func TestFitBikeRoundTripCarriesPowerAsWatts(t *testing.T) {
	serial, created := fixedIdentity()
	steps := []resolvedStep{
		{Role: "warmup", Secs: 600, PowerLo: 107, PowerHi: 133},
		{Repeat: 4, Body: []resolvedStep{
			{Role: "active", Secs: 180, PowerLo: 252, PowerHi: 252, Note: "VO2"},
			{Role: "recovery", Secs: 180, PowerLo: 107, PowerHi: 107},
		}},
		{Role: "cooldown", Secs: 600, PowerLo: 107, PowerHi: 133},
	}
	b, err := fitWorkoutFor("W02 We VO2 4x3:00", steps, fitSportCycling, serial, created).Encode()
	if err != nil {
		t.Fatal(err)
	}
	got := decodeWorkout(t, b)
	if uint8(got.Workout.Sport) != fitSportCycling {
		t.Errorf("sport = %d, want cycling", got.Workout.Sport)
	}
	work := got.WorkoutSteps[1]
	if uint8(work.TargetType) != fitTargetPower {
		t.Errorf("target type = %d, want power", work.TargetType)
	}
	if work.CustomTargetValueLow != 1252 || work.CustomTargetValueHigh != 1252 {
		t.Errorf("power custom %d–%d, want 1252–1252 (252 W offset by 1000, not %%FTP)",
			work.CustomTargetValueLow, work.CustomTargetValueHigh)
	}
	if got.Workout.NumValidSteps != 5 {
		t.Errorf("num_valid_steps = %d, want 5", got.Workout.NumValidSteps)
	}
}

// The data is fast-first (the paceRange convention); FIT wants speeds with
// the slower one in CustomLow. The bridge orders by the converted values, so
// it is right whichever way a pair arrives — conversion, not validation.
func TestFitPaceBandInversion(t *testing.T) {
	cases := []struct {
		name       string
		fast, slow Pace
		lo, hi     uint32
	}{
		{"imperial fast-first", 0.27, 0.29, 3448, 3704}, // 1000/0.29=3448.3, 1000/0.27=3703.7
		{"transposed pair", 0.29, 0.27, 3448, 3704},     // same band, wrong order in — same bytes out
		{"metric anchors", 0.27, 0.28, 3571, 3704},
		{"equal ends", 0.27, 0.27, 3704, 3704}, // a fixed-pace band is legitimate
	}
	for _, c := range cases {
		st := fitLeafStep(resolvedStep{Role: "active", Secs: 60, PaceFast: c.fast, PaceSlow: c.slow})
		if st.TargetType != fitTargetSpeed {
			t.Errorf("%s: target type %d, want speed", c.name, st.TargetType)
		}
		if st.CustomLow != c.lo || st.CustomHigh != c.hi {
			t.Errorf("%s: custom %d–%d, want %d–%d", c.name, st.CustomLow, st.CustomHigh, c.lo, c.hi)
		}
	}
}

func TestFitDeterminism(t *testing.T) {
	day := time.Date(2026, 1, 6, 0, 0, 0, 0, time.UTC)
	steps := []resolvedStep{{Role: "active", DistM: 5000, HRLo: 150, HRHi: 160}}

	serialA, createdA := fitIdentity("revA", "block", day)
	one, err := fitWorkoutFor("W01 Tu Same", steps, fitSportRunning, serialA, createdA).Encode()
	if err != nil {
		t.Fatal(err)
	}
	two, err := fitWorkoutFor("W01 Tu Same", steps, fitSportRunning, serialA, createdA).Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(one, two) {
		t.Error("identical inputs produced different bytes — something is reading a clock or a map")
	}

	serialB, createdB := fitIdentity("revB", "block", day)
	if serialA == serialB {
		t.Error("a new data revision kept the old serial — the watch would dedupe the re-import")
	}
	if createdA.Equal(createdB) {
		t.Error("a new data revision kept the old time_created")
	}
	other, err := fitWorkoutFor("W01 Tu Same", steps, fitSportRunning, serialB, createdB).Encode()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(one, other) {
		t.Error("a new identity produced identical bytes")
	}
}

// Every steps day of both fixtures must mint a distinct identity — the batch
// import is where dedupe would bite, so uniqueness IS the feature.
func TestFitIdentityUniqueAcrossFixtures(t *testing.T) {
	serials := map[uint32]string{}
	createds := map[int64]string{}
	for _, dir := range []string{"./data", ""} {
		if dir == "./data" {
			if _, err := os.Stat("./data/athlete.json"); err != nil {
				continue // no athlete data in this checkout — loading "./data" would just re-load the defaults and collide with itself
			}
		}
		d, err := loadDataset(dir, chicago(t))
		if err != nil {
			t.Fatalf("load %q: %v", dir, err)
		}
		for _, b := range d.Blocks {
			for wi, w := range b.Weeks {
				for di, sess := range w.Days {
					if len(sess.Steps) == 0 {
						continue
					}
					day := b.DayOf(wi, di)
					where := fmt.Sprintf("%s %s", b.ID, day.Format("2006-01-02"))
					serial, created := fitIdentity(d.Rev, b.ID, day)
					if serial == 0 || serial == fitInvalidUint32 {
						t.Errorf("%s: serial is the sentinel %#x", where, serial)
					}
					if prev, ok := serials[serial]; ok {
						t.Errorf("%s and %s share serial %d", prev, where, serial)
					}
					serials[serial] = where
					if prev, ok := createds[created.Unix()]; ok {
						t.Errorf("%s and %s share time_created %s", prev, where, created)
					}
					createds[created.Unix()] = where
				}
			}
		}
	}
}

// goldenFit is a fixed workout exercising every message shape: identity from
// fitIdentity, a targeted distance leaf, a repeat with notes, an untargeted
// cooldown.
func goldenFit(t *testing.T) []byte {
	t.Helper()
	serial, created := fitIdentity("goldenrev", "example-base-block", time.Date(2026, 1, 13, 0, 0, 0, 0, time.UTC))
	name, err := fitName(2, 1, "3 × 4:00 @ threshold")
	if err != nil {
		t.Fatal(err)
	}
	b, err := fitWorkoutFor(name, []resolvedStep{
		{Role: "warmup", DistM: 2000, HRLo: 135, HRHi: 150},
		{Repeat: 3, Body: []resolvedStep{
			{Role: "active", Secs: 240, PaceFast: 0.27, PaceSlow: 0.28, Note: "Comfortably hard"},
			{Role: "recovery", Secs: 120},
		}},
		{Role: "cooldown", DistM: 2000},
	}, fitSportRunning, serial, created).Encode()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestFitGoldenBytes pins the exact bytes. Any deliberate encoder change
// reruns with -update-golden (the -update-schema idiom); an accidental one
// fails here first. testdata/ is outside SRC_HASH, so this cannot force a
// redeploy.
func TestFitGoldenBytes(t *testing.T) {
	got := goldenFit(t)
	path := filepath.Join("testdata", "workout.fit")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes)", path, len(got))
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v — run 'go test -run TestFitGoldenBytes -update-golden .'", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("encoder output drifted from %s (%d bytes vs %d) — if deliberate, rerun with -update-golden", path, len(got), len(want))
	}
}

// A pristine file decodes (positive control) and a single corrupted byte
// fails the CRC (negative control) — proving the decoder's CRC check is
// actually on, without which the round-trip tests prove less than they seem
// to.
func TestFitCRCCatchesACorruptedByte(t *testing.T) {
	b := goldenFit(t)
	if _, err := decoder.New(bytes.NewReader(b)).Decode(); err != nil {
		t.Fatalf("pristine file failed to decode: %v", err)
	}
	bad := bytes.Clone(b)
	bad[len(bad)/2] ^= 0xFF
	if _, err := decoder.New(bytes.NewReader(bad)).Decode(); err == nil {
		t.Error("a corrupted byte decoded cleanly — CRC checking is not happening")
	}
}

// The 50-step budget is the watch's rule and the loader enforces it. The
// encoder does not: a 51-step workout encodes successfully, and this pins
// that boundary so nobody helpfully duplicates the watch's rules in the
// container format.
func TestFitEncoderAcceptsFiftyOneSteps(t *testing.T) {
	serial, created := fixedIdentity()
	steps := make([]resolvedStep, 51)
	for i := range steps {
		steps[i] = resolvedStep{Role: "active", Secs: 60}
	}
	b, err := fitWorkoutFor("W01 Mo Boundary", steps, fitSportRunning, serial, created).Encode()
	if err != nil {
		t.Fatalf("51 steps must encode — the budget is the loader's: %v", err)
	}
	if got := decodeWorkout(t, b); len(got.WorkoutSteps) != 51 {
		t.Errorf("decoded %d steps, want 51", len(got.WorkoutSteps))
	}
}

// An encoder-capability test, not a feature: the steps dialect has no open
// (press-lap) form, and the LOADER is the gatekeeper that keeps one out of
// the data. The container can carry it, and if the dialect ever grows an
// explicit "open": true this is the proof it already works.
func TestFitOpenDurationRoundTrip(t *testing.T) {
	serial, created := fixedIdentity()
	w := FitWorkout{Name: "W01 Mo Open", Sport: fitSportRunning, Serial: serial, TimeCreated: created,
		Steps: []FitStep{{
			Intensity: 0, DurationType: fitDurationOpen, DurationValue: 0,
			TargetType: fitTargetOpen, CustomLow: fitInvalidUint32, CustomHigh: fitInvalidUint32,
		}}}
	b, err := w.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got := decodeWorkout(t, b)
	if len(got.WorkoutSteps) != 1 || uint8(got.WorkoutSteps[0].DurationType) != fitDurationOpen {
		t.Error("open duration did not round-trip")
	}
}

func TestFitEncodeRejections(t *testing.T) {
	serial, created := fixedIdentity()
	ok := []FitStep{{DurationType: fitDurationTime, DurationValue: 60000,
		TargetType: fitTargetOpen, CustomLow: fitInvalidUint32, CustomHigh: fitInvalidUint32}}
	longNotes := ok[0]
	longNotes.Notes = string(bytes.Repeat([]byte("x"), 255))
	badNotes := ok[0]
	badNotes.Notes = "café"

	cases := []struct {
		name   string
		w      FitWorkout
		expect string
	}{
		{"no steps", FitWorkout{Name: "W01 Mo X", Sport: fitSportRunning, Serial: serial, TimeCreated: created}, "no steps"},
		{"empty name", FitWorkout{Sport: fitSportRunning, Serial: serial, TimeCreated: created, Steps: ok}, "empty"},
		{"non-ascii name", FitWorkout{Name: "W01 Mo Café", Sport: fitSportRunning, Serial: serial, TimeCreated: created, Steps: ok}, "printable ASCII"},
		{"non-ascii notes", FitWorkout{Name: "W01 Mo X", Sport: fitSportRunning, Serial: serial, TimeCreated: created, Steps: []FitStep{badNotes}}, "printable ASCII"},
		{"zero serial", FitWorkout{Name: "W01 Mo X", Sport: fitSportRunning, Serial: 0, TimeCreated: created, Steps: ok}, "sentinel"},
		{"max serial", FitWorkout{Name: "W01 Mo X", Sport: fitSportRunning, Serial: 0xFFFFFFFF, TimeCreated: created, Steps: ok}, "sentinel"},
		{"pre-epoch time", FitWorkout{Name: "W01 Mo X", Sport: fitSportRunning, Serial: serial, TimeCreated: time.Date(1985, 1, 1, 0, 0, 0, 0, time.UTC), Steps: ok}, "FIT epoch"},
		{"string over 254 bytes", FitWorkout{Name: "W01 Mo X", Sport: fitSportRunning, Serial: serial, TimeCreated: created, Steps: []FitStep{longNotes}}, "254"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := c.w.Encode()
			if err == nil {
				t.Fatalf("%s encoded without complaint", c.name)
			}
			if !bytes.Contains([]byte(err.Error()), []byte(c.expect)) {
				t.Errorf("error was %q, want it to mention %q", err, c.expect)
			}
		})
	}
}

// The whole pipeline over the embedded defaults: loadDataset → resolveSteps →
// fitName → fitIdentity → Encode → independent decode. The defaults are
// metric on purpose — the exact centimetre assertions below fail if any
// conversion quietly assumes imperial.
func TestDefaultsServeFitEndToEnd(t *testing.T) {
	d, err := loadDataset("", chicago(t))
	if err != nil {
		t.Fatal(err)
	}
	b := d.Blocks[0]

	type found struct {
		week, day int
		steps     []resolvedStep
		name      string
		bytes     []byte
	}
	var days []found
	for wi, w := range b.Weeks {
		for di, sess := range w.Days {
			if len(sess.Steps) == 0 {
				continue
			}
			c := b.ctxFor(d.Athlete, w.N)
			sc := *c
			sc.Session = &sess
			sc.InBlock = true
			rs, err := resolveSteps(&sc, sess)
			if err != nil {
				t.Fatalf("week %d day %d: %v", w.N, di+1, err)
			}
			name, err := fitName(w.N, di, sess.Label)
			if err != nil {
				t.Fatalf("week %d day %d: %v", w.N, di+1, err)
			}
			serial, created := fitIdentity(d.Rev, b.ID, b.DayOf(wi, di))
			bts, err := fitWorkoutFor(name, rs, fitSportRunning, serial, created).Encode()
			if err != nil {
				t.Fatalf("week %d day %d: %v", w.N, di+1, err)
			}
			days = append(days, found{w.N, di, rs, name, bts})
		}
	}
	if len(days) != 2 {
		t.Fatalf("defaults have %d steps days, want 2 (week 1 and week 2 Tuesday)", len(days))
	}

	// Week 1 Tuesday: 3 km warmup with templated HR band, 4× (0:20 stride +
	// 1:00 jog), 4 km cooldown with bare-number HR band.
	one := decodeWorkout(t, days[0].bytes)
	if one.Workout.NumValidSteps != 5 {
		t.Errorf("week 1: num_valid_steps = %d, want 5", one.Workout.NumValidSteps)
	}
	warm := one.WorkoutSteps[0]
	if warm.DurationValue != 300000 {
		t.Errorf("week 1 warmup = %d cm, want 300000 — is a conversion assuming imperial?", warm.DurationValue)
	}
	if warm.CustomTargetValueLow != 235 || warm.CustomTargetValueHigh != 245 {
		t.Errorf("week 1 warmup HR custom %d–%d, want 235–245 (easyLo/easyHi + 100)",
			warm.CustomTargetValueLow, warm.CustomTargetValueHigh)
	}
	rep := one.WorkoutSteps[3]
	if uint8(rep.DurationType) != fitDurationRepeat || rep.DurationValue != 1 || rep.TargetValue != 4 {
		t.Errorf("week 1 repeat = type %d back-to %d × %d, want type %d back-to 1 × 4",
			rep.DurationType, rep.DurationValue, rep.TargetValue, fitDurationRepeat)
	}

	// Week 2 Tuesday: the {{pace .Athlete.Paces.threshold}} band. 4:30/km →
	// 3704 mm/s, 4:40/km → 3571 mm/s, slower speed in CustomLow.
	two := decodeWorkout(t, days[1].bytes)
	if two.Workout.NumValidSteps != 5 {
		t.Errorf("week 2: num_valid_steps = %d, want 5", two.Workout.NumValidSteps)
	}
	active := two.WorkoutSteps[1]
	if uint8(active.TargetType) != fitTargetSpeed ||
		active.CustomTargetValueLow != 3571 || active.CustomTargetValueHigh != 3704 {
		t.Errorf("week 2 active custom %d–%d (type %d), want 3571–3704 (speed)",
			active.CustomTargetValueLow, active.CustomTargetValueHigh, active.TargetType)
	}
	if active.DurationValue != 240000 {
		t.Errorf("week 2 active = %d ms, want 240000", active.DurationValue)
	}
}

// Anchor-derived paces round to whole seconds in the athlete's DISPLAY units,
// so flipping units legitimately moves the FIT bytes by a few mm/s (and moves
// Rev and every ETag with them). This pins that as understood behaviour
// rather than a bug to be reported in November.
func TestUnitsFlipMovesAnchorDerivedPaces(t *testing.T) {
	d, err := loadDataset("", chicago(t))
	if err != nil {
		t.Fatal(err)
	}
	b := d.Blocks[0]
	sess := b.Weeks[1].Days[1] // week 2 Tuesday, the threshold session
	if len(sess.Steps) == 0 {
		t.Fatal("week 2 Tuesday lost its steps")
	}
	resolve := func() FitStep {
		c := b.ctxFor(d.Athlete, 2)
		sc := *c
		sc.Session = &sess
		sc.InBlock = true
		rs, err := resolveSteps(&sc, sess)
		if err != nil {
			t.Fatal(err)
		}
		return fitLeafStep(rs[1].Body[0])
	}
	metric := resolve()
	d.Athlete.Units = Imperial
	imperial := resolve()

	if metric.CustomHigh == imperial.CustomHigh {
		t.Error("units flip left the anchor-derived speed identical — rounding in display units should move it")
	}
	diff := int64(metric.CustomHigh) - int64(imperial.CustomHigh)
	if diff < -20 || diff > 20 {
		t.Errorf("units flip moved the speed by %d mm/s — a few is rounding, this is a conversion bug", diff)
	}
}
