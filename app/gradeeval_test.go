package main

// The grader's judgment, measured against the grades a human actually
// posted. For every date the log carries a grade, this replays the day
// through the real grading loop and prints both verdicts side by side —
// the comparison that decides whether dry mode has earned live.
//
// It runs nowhere by default: it needs a copy of the athlete's data (plan,
// log, and the archived .fit files) and a reachable model, neither of which
// belongs in a checkout. Point it at a scratch copy:
//
//	RC_EVAL_DIR=/tmp/eval GRADER_PROVIDER=openai GRADER_MODEL=qwen3:4b \
//	GRADER_BASE_URL=http://host:11434 RC_EVAL_OUT=/tmp/eval/report.md \
//	go test -run TestGradeEvalBatch -v -timeout 6h .
//
// The mode is forced to dry: this reads the log and never writes it.

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestGradeEvalBatch(t *testing.T) {
	dir := os.Getenv("RC_EVAL_DIR")
	if dir == "" {
		t.Skip("RC_EVAL_DIR not set — the judgment comparison runs where the data lives")
	}
	if _, err := os.Stat(dir + "/athlete.json"); err != nil {
		t.Fatalf("%s does not look like a data directory: %v", dir, err)
	}

	cfg, err := graderConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Mode = "dry" // never writes the log, whatever the environment says
	if cfg.Model == "" {
		t.Fatal("set GRADER_MODEL (and GRADER_BASE_URL) to the model under evaluation")
	}

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
	s.weather = newWeatherService(metrics)
	metrics.reconcile(dir+"/activities", s.ds().Loc, s.weather)

	g := newGrader(s, cfg)
	g.settle = 0 // the archive is on disk; nothing is still arriving

	// Every date a human graded, oldest first.
	human := store.Grades()
	dates := make([]string, 0, len(human))
	for d := range human {
		dates = append(dates, d)
	}
	sort.Strings(dates)
	if len(dates) == 0 {
		t.Fatal("no grades in the log to compare against")
	}
	t.Logf("comparing %d graded day(s) using %s at %s", len(dates), cfg.Model, cfg.BaseURL)

	d := s.ds()
	var report strings.Builder
	fmt.Fprintf(&report, "# Grading comparison — %s\n\nModel: `%s` at `%s`\n\n",
		time.Now().Format("2006-01-02"), cfg.Model, cfg.BaseURL)

	agreed, run := 0, 0
	for _, date := range dates {
		acts, err := metrics.byDate(date)
		if err != nil {
			t.Fatal(err)
		}
		if len(acts) == 0 {
			t.Logf("%s: no archived activity — skipped", date)
			fmt.Fprintf(&report, "## %s — no archived activity\n\n", date)
			continue
		}
		// Prefer the activity whose sport matches what the day prescribed;
		// a day can carry both a ride and a run.
		pick := &acts[0]
		var kind Kind
		if day, err := time.ParseInLocation("2006-01-02", date, d.Loc); err == nil {
			if blk := d.Current(s.day(d)); blk != nil {
				if wk, di, ok := blk.Locate(day); ok {
					kind = wk.Days[di].Kind
					for i := range acts {
						if (acts[i].Sport == "cycling") == kind.IsBike() {
							pick = &acts[i]
							break
						}
					}
				}
			}
		}

		// Blind: the grader must not read the verdict it is being measured
		// against. Everything else in the log stays visible, because the
		// surrounding week is real context a live grade would also have.
		g.blindDate = date
		start := time.Now()
		got, err := g.grade(pick)
		took := time.Since(start).Round(time.Second)
		run++

		h := human[date]
		fmt.Fprintf(&report, "## %s — %s (%s, %s)\n\n", date, kind, pick.Name, took)
		fmt.Fprintf(&report, "**Human: %s** — %s\n\n", h.Val, h.Note)
		switch {
		case err != nil:
			t.Logf("%s: FAILED after %s: %v", date, took, err)
			fmt.Fprintf(&report, "**Model: FAILED** — %v\n\n", err)
		default:
			same := ""
			if got.Val == h.Val {
				agreed++
				same = " (agrees)"
			}
			t.Logf("%s: human %s, model %s%s in %s", date, h.Val, got.Val, same, took)
			fmt.Fprintf(&report, "**Model: %s**%s — %s\n\n", got.Val, same, got.Note)
		}
	}

	fmt.Fprintf(&report, "\n---\n\nLetter agreement: %d of %d graded.\n", agreed, run)
	t.Logf("letter agreement: %d of %d", agreed, run)
	if out := os.Getenv("RC_EVAL_OUT"); out != "" {
		if err := os.WriteFile(out, []byte(report.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("report written to %s", out)
	}
}
