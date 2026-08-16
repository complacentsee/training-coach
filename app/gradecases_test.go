package main

// Can the grader say no? The judgment comparison measures agreement on days
// an athlete actually trained, and those days are mostly good ones — a
// grader answering "A" every time would score well on them. These cases
// measure the other half: sessions that did not meet the day's requirement,
// each graded against a prescription that says plainly what was asked, with
// the expected letter fixed in advance by the block's own legend rather
// than by anyone's opinion.
//
// The corpus is committed, in testdata/cases: sanitized real recordings for
// the failures and a generated one for the pass, with cases.tsv naming the
// day each is graded as and the letter its measured share earns. See that
// file and tools/sanitizefit for what the fixtures carry and what was
// stripped out of them.
//
// Two tests read it. The share pin needs nothing but the repo and runs
// everywhere; the grading run needs a model, and skips without one:
//
//	GRADER_PROVIDER=openai GRADER_MODEL=… GRADER_BASE_URL=… GRADER_API_KEY=… \
//	go test -run TestGradeCases -v .
//
// RC_CASES_DIR points both at a different corpus — a private one, say —
// laid out the same way, with its own plan and log beside the activities.

import (
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type gradeCase struct {
	activity, date, want string
	share                float64
	// basis is what the expected letter rests on: "legend" means the
	// rubric's bands applied to the measured share, so the expectation is
	// arithmetic. "judgment" means a session the share cannot grade — a
	// quality day, where the letter turns on whether the prescribed work
	// was delivered.
	basis string
	what  string
}

const committedCases = "testdata/cases"

func loadGradeCases(t *testing.T, dir string) []gradeCase {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "cases.tsv"))
	if err != nil {
		t.Fatalf("case table: %v", err)
	}
	var cases []gradeCase
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 5 {
			t.Fatalf("malformed case line: %q", line)
		}
		share, err := strconv.ParseFloat(f[3], 64)
		if err != nil {
			t.Fatalf("case %q: share: %v", f[0], err)
		}
		c := gradeCase{activity: f[0], date: f[1], want: strings.TrimSpace(f[2]),
			share: share, basis: strings.TrimSpace(f[4])}
		if c.basis != "legend" && c.basis != "judgment" {
			t.Fatalf("case %q: basis is %q, want legend or judgment", f[0], c.basis)
		}
		if len(f) > 5 {
			c.what = f[5]
		}
		cases = append(cases, c)
	}
	if len(cases) == 0 {
		t.Fatal("no cases in the table")
	}
	return cases
}

// caseServer stands the app up over a corpus: the committed one runs
// against the embedded example block, so it needs no data directory at all.
func caseServer(t *testing.T, dir string) (*server, []gradeCase) {
	t.Helper()
	cases := loadGradeCases(t, dir)
	dataDir := dir
	if dir == committedCases {
		dataDir = t.TempDir() // empty volume → the embedded defaults
		acts := filepath.Join(dataDir, "activities")
		if err := os.MkdirAll(acts, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, c := range cases {
			b, err := os.ReadFile(filepath.Join(dir, c.activity))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(acts, c.activity), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	store, err := OpenStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := openMetricsDB(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(metrics.close)
	s := &server{store: store, loc: chicago(t), dataDir: dataDir}
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	s.metrics = metrics
	s.weather = newWeatherService(metrics)
	metrics.reconcile(filepath.Join(dataDir, "activities"), s.ds().Loc, s.weather)
	return s, cases
}

func casesDir() string {
	if d := os.Getenv("RC_CASES_DIR"); d != "" {
		return d
	}
	return committedCases
}

// TestGradeCaseShares pins what the cases measure, with no model involved:
// every letter in the table rests on these numbers, so a register change
// that moves them changes the rubric's verdicts and must be seen. Real
// recordings, so this is also the suite's only end-to-end coverage of decode
// and measurement over sessions nobody synthesized.
func TestGradeCaseShares(t *testing.T) {
	s, cases := caseServer(t, casesDir())
	capKey := s.ds().Current(s.day(s.ds())).Grading.CapKey()
	cap := s.ds().Athlete.HR[capKey]
	if cap == 0 {
		t.Fatalf("the block grades under %q, which the athlete does not declare", capKey)
	}
	for _, c := range cases {
		got, err := s.metrics.underCapShareSQL(c.activity, cap)
		if err != nil || got == nil {
			t.Errorf("%s: no share (err=%v)", c.activity, err)
			continue
		}
		if pyRound(*got, 4) != c.share {
			t.Errorf("%s: share under %d = %.4f, table says %.4f",
				c.activity, cap, *got, c.share)
		}
		// Where the letter rests on the rubric, it must BE the rubric — the
		// expectation is arithmetic, not opinion. Where it rests on
		// judgment, the same check would be wrong: those sessions are meant
		// to run over the cap, and the share says nothing about them.
		if c.basis != "legend" {
			continue
		}
		if want := letterFor(t, s, *got); want != c.want {
			t.Errorf("%s: %.4f is a %s by the legend, table says %s", c.activity, *got, want, c.want)
		}
	}
}

// TestQualityDaysAreNotShareGraded pins the trap the quality fixtures were
// built to catch: applying the easy-run rubric to a quality day grades the
// session that was abandoned ABOVE the one that was delivered, because
// doing the prescribed work puts an athlete over the cap. Any change that
// lets the share decide a quality letter fails here.
func TestQualityDaysAreNotShareGraded(t *testing.T) {
	s, cases := caseServer(t, casesDir())
	var delivered, abandoned *gradeCase
	for i := range cases {
		if cases[i].basis != "judgment" {
			continue
		}
		switch cases[i].want {
		case "A":
			delivered = &cases[i]
		case "F", "DNF":
			// A session that was not essentially completed is DNF rather
			// than a letter (rule of 16 Aug 2026); either expectation marks
			// the abandoned half of the pair.
			abandoned = &cases[i]
		}
	}
	if delivered == nil || abandoned == nil {
		t.Skip("this corpus carries no judgment-graded pair")
	}
	if !(abandoned.share > delivered.share) {
		t.Fatalf("the fixtures no longer demonstrate the trap: abandoned %.4f, delivered %.4f",
			abandoned.share, delivered.share)
	}
	if letterFor(t, s, abandoned.share) == abandoned.want {
		t.Error("the abandoned session's letter now falls out of the share, so it proves nothing")
	}
}

// TestFastestSegmentsRecoverTheReps: the athlete laps nothing but whole
// miles, so a session's repetitions arrive buried in one smooth trace.
// Asking the stream for them must find the work that was done — and must
// not invent work that was not.
func TestFastestSegmentsRecoverTheReps(t *testing.T) {
	read := func(name string) *activityStreams {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(committedCases, name))
		if err != nil {
			t.Skipf("committed corpus not present: %v", err)
		}
		st, err := decodeActivity(b)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}

	// Three 4:00 reps at 4:30/km were run; three must come back, on target.
	segs := fastestSegments(read("2026-01-13-06-00-00.fit"), 0, 240, 3, Metric)
	if len(segs) != 3 {
		t.Fatalf("delivered session: %d segments, want 3", len(segs))
	}
	for i, g := range segs {
		if g.Pace != "4:30/km" {
			t.Errorf("rep %d: %s, want 4:30/km", i+1, g.Pace)
		}
		if i > 0 && segs[i-1].StartS >= g.StartS {
			t.Error("segments are not in the order they were run")
		}
	}

	// One slow rep was run. Asking for three still returns three stretches
	// — that is what "fastest" means — but only one is anywhere near the
	// work, and none is on target. A grader reading these sees a session
	// that was not delivered.
	segs = fastestSegments(read("2026-01-13-07-00-00.fit"), 0, 240, 3, Metric)
	if len(segs) != 3 {
		t.Fatalf("abandoned session: %d segments, want 3", len(segs))
	}
	onTarget := 0
	for _, g := range segs {
		if g.Pace <= "4:40/km" && strings.HasPrefix(g.Pace, "4:") {
			onTarget++
		}
	}
	if onTarget != 0 {
		t.Errorf("abandoned session: %d segments read as on target, want none: %+v", onTarget, segs)
	}
}

// TestIntegratedDistanceMatchesTheOdometer: the register integrates
// distance from the speed stream rather than reading the odometer, so the
// odometer is the independent check on it. These are real recordings and
// five of them carry a stop the recording did not describe; before those
// were excluded, four integrated 0.64 to 2.38% long and the stretch the
// grader was handed as a best effort could be most of the stop — on
// 2026-01-10-11 the third-fastest "four minutes" was 549 s covering
// 1,071 m, ranked above the real 240 s at 8:33/mi that replaced it.
func TestIntegratedDistanceMatchesTheOdometer(t *testing.T) {
	ents, err := os.ReadDir(committedCases)
	if err != nil {
		t.Skipf("committed corpus not present: %v", err)
	}
	checked, gapped := 0, 0
	for _, e := range ents {
		if filepath.Ext(e.Name()) != ".fit" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(committedCases, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		s, err := decodeActivity(b)
		if err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		if !s.HaveVel || s.DistM == nil || *s.DistM <= 0 {
			continue
		}
		checked++
		dist, gaps := describedDistance(s)
		n := len(s.Time) - 1
		if gaps[n] > 0 {
			gapped++
		}
		// 0.5%: the gap-free files in this corpus integrate within 0.16% of
		// their own odometers and the gapped ones within 0.21%, so this is
		// wide enough to be about gaps rather than about sampling.
		if off := math.Abs(dist[n]-*s.DistM) / *s.DistM; off > 0.005 {
			t.Errorf("%s: integrated %.1f m against a %.1f m odometer (%.2f%%, %d gaps)",
				e.Name(), dist[n], *s.DistM, off*100, gaps[n])
		}
	}
	if checked == 0 {
		t.Fatal("no fixture carried both a speed stream and an odometer")
	}
	if gapped == 0 {
		t.Error("no fixture carries a recording gap any more, so this proves nothing")
	}
}

func letterFor(t *testing.T, s *server, share float64) string {
	t.Helper()
	g := s.ds().Current(s.day(s.ds())).Grading
	floors := bandFloors(g.Bands)
	if floors == nil {
		t.Fatal("the legend's bands do not carry floors")
	}
	for i, f := range floors {
		if share*100 >= f {
			return g.Bands[i].Grade
		}
	}
	return "?"
}

func TestGradeCases(t *testing.T) {
	if os.Getenv("GRADER_MODEL") == "" {
		t.Skip("GRADER_MODEL not set — the grading cases need a model to grade with")
	}
	cfg, err := graderConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Mode = "dry" // reads the log, never writes it

	dir := casesDir()
	s, cases := caseServer(t, dir)
	g := newGrader(s, cfg)
	g.settle = 0 // the corpus is on disk; nothing is still arriving
	// One case is one RECORDING graded against a day's prescription. The
	// fixtures share dates on purpose — seven long runs under 2026-01-10,
	// both threshold sessions under 2026-01-13 — so the day around a case
	// is other cases, not the rest of its session.
	g.soloRecording = true

	var report strings.Builder
	fmt.Fprintf(&report, "# Grading cases — %s\n\nModel: `%s`\n\n"+
		"Each session graded against its day's prescription. The expected letter is\n"+
		"what the block's legend gives the measured share.\n\n",
		time.Now().Format("2006-01-02"), cfg.Model)

	right := 0
	for _, c := range cases {
		row, err := s.metrics.rowByName(c.activity)
		if err != nil || row == nil {
			t.Fatalf("%s: no metrics row (err=%v)", c.activity, err)
		}
		m := &activityMetrics{Name: c.activity, Date: c.date, Sport: row.Sport}
		g.blindDate = c.date
		got, err := g.grade(m, "")

		switch {
		case err != nil:
			t.Errorf("%s (%s): FAILED: %v", c.activity, c.what, err)
			fmt.Fprintf(&report, "## want %s — %s\n\n**FAILED:** %v\n\n", c.want, c.what, err)
		case got.Val == c.want:
			right++
			t.Logf("%.4f share: want %s, got %s ✓  (%s)", c.share, c.want, got.Val, c.what)
			fmt.Fprintf(&report, "## %s ✓ — %.1f%% under cap (%s)\n\n%s\n\n",
				got.Val, c.share*100, c.what, got.Note)
		default:
			t.Errorf("%.4f share (%s): want %s, got %s", c.share, c.what, c.want, got.Val)
			fmt.Fprintf(&report, "## got %s, expected %s — %.1f%% under cap (%s)\n\n%s\n\n",
				got.Val, c.want, c.share*100, c.what, got.Note)
		}
	}
	fmt.Fprintf(&report, "\n---\n\n%d of %d letters as the legend prescribes.\n", right, len(cases))
	t.Logf("%d of %d cases graded as the legend prescribes", right, len(cases))
	if out := os.Getenv("RC_CASES_OUT"); out != "" {
		if err := os.WriteFile(out, []byte(report.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestSameKindHistoryComparesLikeForLike: the grader had every prior grade in
// front of it and still compared a 10-mile long run against a 6-mile easy run
// with strides, because the log records the ENTRY kind and never the SESSION
// kind — so "which earlier session was comparable" could only be guessed from
// prose. The example block has the same shape as the real one: two Saturday
// long runs a week apart, with easy and quality days between them.
func TestSameKindHistoryComparesLikeForLike(t *testing.T) {
	s, _ := caseServer(t, committedCases)
	d := s.ds()
	blk := d.Current(mustDay(t, "2026-01-17", d.Loc))
	if blk == nil {
		t.Fatal("no block")
	}

	second := mustDay(t, "2026-01-17", d.Loc) // the later long run
	got := s.sameKindHistory(d, blk, second, KindLong, 6)
	if len(got) == 0 {
		t.Fatal("no earlier long run found; the comparison this exists for is impossible")
	}
	if got[0].Date != "2026-01-10" {
		t.Errorf("nearest earlier long run = %s, want 2026-01-10", got[0].Date)
	}
	for _, r := range got {
		if r.Date >= "2026-01-17" {
			t.Errorf("%s is not earlier than the day being graded — the grade under test must never leak", r.Date)
		}
		if wk, di, ok := blk.Locate(mustDay(t, r.Date, d.Loc)); ok {
			if k := wk.Days[di].Kind; k != KindLong {
				t.Errorf("%s is a %s, not a long run — the whole point is like for like", r.Date, k)
			}
		}
	}
	// Measured, not merely named: without the metrics join this is a list of
	// dates and compares nothing.
	if got[0].ElapsedS == 0 {
		t.Errorf("%s carries no measured duration: %+v", got[0].Date, got[0])
	}
	if got[0].UnderCap == nil {
		t.Errorf("%s carries no under-cap share, which is the rubric's own number", got[0].Date)
	}

	// The first session of its kind has nothing to compare with, and must say
	// so by being empty rather than reaching for an unlike session.
	if first := s.sameKindHistory(d, blk, mustDay(t, "2026-01-10", d.Loc), KindLong, 6); len(first) != 0 {
		t.Errorf("the first long run of the block got %d earlier ones: %+v", len(first), first)
	}

	// And it reaches the day payload, so the grader sees it without asking.
	out, code, _ := s.dayPayload("2026-01-17", "")
	if code != http.StatusOK {
		t.Fatalf("dayPayload: %d", code)
	}
	doc, ok := out.(dayDoc)
	if !ok {
		t.Fatalf("payload is %T", out)
	}
	if doc.Previous == nil || doc.Previous.Date != "2026-01-10" {
		t.Errorf("the prescription does not name the previous long run: %+v", doc.Previous)
	}
	t.Logf("previous long run: %s %q grade=%q %ds under_cap=%v",
		doc.Previous.Date, doc.Previous.Label, doc.Previous.Grade, doc.Previous.ElapsedS, doc.Previous.UnderCap)
}

func mustDay(t *testing.T, iso string, loc *time.Location) time.Time {
	t.Helper()
	d, err := time.ParseInLocation("2006-01-02", iso, loc)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
