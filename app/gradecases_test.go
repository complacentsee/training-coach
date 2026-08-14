package main

// Can the grader say no? The judgment comparison measures agreement on days
// the athlete actually trained, and those days are mostly good ones — a
// grader that answered "A" every time would score well on them. This
// measures the other half: sessions that did NOT meet the day's
// requirement, graded against a prescription that says plainly what was
// asked for, with the expected letter fixed in advance by the block's own
// legend rather than by anyone's opinion.
//
// The cases live outside the repo, because they name real recordings:
// RC_CASES_DIR is a data directory (plan, log, and an activities/ holding
// the case files) containing cases.tsv, one case per line —
//
//	<activity.fit>	<block date>	<expected letter>	<why>
//
// The letter is what the legend's bands say about that activity's measured
// share, so a disagreement is the grader departing from the rubric, not
// from a preference. Mode is forced to dry: this never writes the log.
//
//	RC_CASES_DIR=/tmp/cases GRADER_PROVIDER=openai GRADER_MODEL=… \
//	GRADER_BASE_URL=… GRADER_API_KEY=… go test -run TestGradeCases -v .

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

type gradeCase struct {
	activity, date, want, why string
}

func TestGradeCases(t *testing.T) {
	dir := os.Getenv("RC_CASES_DIR")
	if dir == "" {
		t.Skip("RC_CASES_DIR not set — the discrimination cases run where the recordings live")
	}
	raw, err := os.ReadFile(dir + "/cases.tsv")
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
		if len(f) < 3 {
			t.Fatalf("malformed case line: %q", line)
		}
		c := gradeCase{activity: f[0], date: f[1], want: strings.TrimSpace(f[2])}
		if len(f) > 3 {
			c.why = f[3]
		}
		cases = append(cases, c)
	}

	cfg, err := graderConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Mode = "dry"
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := openMetricsDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer metrics.close()
	s := &server{store: store, loc: chicago(t), dataDir: dir}
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	s.metrics = metrics
	metrics.reconcile(dir+"/activities", s.ds().Loc)
	g := newGrader(s, cfg)

	var report strings.Builder
	fmt.Fprintf(&report, "# Discrimination cases — %s\n\nModel: `%s`\n\n"+
		"Each session is graded against the day's prescription; the expected letter is\n"+
		"what the block's legend says about the measured share.\n\n",
		time.Now().Format("2006-01-02"), cfg.Model)

	right := 0
	for _, c := range cases {
		row, err := metrics.rowByName(c.activity)
		if err != nil || row == nil {
			t.Fatalf("%s: no metrics row (err=%v)", c.activity, err)
		}
		share, err := metrics.underCapShareSQL(c.activity, s.ds().Athlete.HR["gradeCap"])
		if err != nil {
			t.Fatal(err)
		}
		m := &activityMetrics{Name: c.activity, Date: c.date, Sport: row.Sport}
		g.blindDate = c.date
		got, err := g.grade(m)

		measured := "n/a"
		if share != nil {
			measured = fmt.Sprintf("%.1f%% under cap", *share*100)
		}
		switch {
		case err != nil:
			t.Errorf("%s (%s): FAILED: %v", c.activity, c.why, err)
			fmt.Fprintf(&report, "## %s — want %s (%s, %s)\n\n**FAILED:** %v\n\n",
				c.date, c.want, c.why, measured, err)
		case got.Val == c.want:
			right++
			t.Logf("%s: want %s, got %s ✓  (%s, %s)", c.date, c.want, got.Val, c.why, measured)
			fmt.Fprintf(&report, "## %s — %s ✓ (%s, %s)\n\n%s\n\n",
				c.date, got.Val, c.why, measured, got.Note)
		default:
			t.Errorf("%s: want %s, got %s  (%s, %s)", c.date, c.want, got.Val, c.why, measured)
			fmt.Fprintf(&report, "## %s — got %s, expected %s (%s, %s)\n\n%s\n\n",
				c.date, got.Val, c.want, c.why, measured, got.Note)
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
