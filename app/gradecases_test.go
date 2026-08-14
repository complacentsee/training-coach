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
	what                 string
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
		if len(f) < 4 {
			t.Fatalf("malformed case line: %q", line)
		}
		share, err := strconv.ParseFloat(f[3], 64)
		if err != nil {
			t.Fatalf("case %q: share: %v", f[0], err)
		}
		c := gradeCase{activity: f[0], date: f[1], want: strings.TrimSpace(f[2]), share: share}
		if len(f) > 4 {
			c.what = f[4]
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
	metrics.reconcile(filepath.Join(dataDir, "activities"), s.ds().Loc)
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
		// And the letter the table claims must be the one the legend gives
		// that share — the expectations are the rubric, not opinions.
		if want := letterFor(t, s, *got); want != c.want {
			t.Errorf("%s: %.4f is a %s by the legend, table says %s", c.activity, *got, want, c.want)
		}
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
		got, err := g.grade(m)

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
