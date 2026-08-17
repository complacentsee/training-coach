package main

// The acceptance gate for the register port: decode and measure EVERY
// archived activity with both registers — this binary's (decode.go +
// metrics.go) and the python mirrors (tools/fit_streams.py +
// tools/grade_metrics.py) — and demand zero unexplained differences in
// streams AND computed metrics. This is the whole-archive cross-check the
// 14 Aug 2026 probes ran, kept as a runnable test.
//
// It needs the archive corpus, which is not in the repo (personal data), so
// it activates only when RC_CORPUS names a directory holding pulled/,
// backfill/ and backfill-plan.json — the probe corpus layout. Without it,
// the test skips; the permanent, fixture-based subset checks live in
// decode_test.go and run everywhere.
//
//	RC_CORPUS=/path/to/corpus go test -run TestAcceptanceGate -v -timeout 30m .

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
)

// gateHelperPy drives the python mirrors verbatim — fit_streams.streams()
// for the streams, grade_metrics.main() for the metrics — one interpreter
// for the whole shard. It is written to a temp dir at run time so the gate
// stays a single self-contained file.
const gateHelperPy = `
import io, json, sys, tempfile, os
sys.path.insert(0, sys.argv[1])   # tools/
import fit_streams, grade_metrics

athlete = sys.argv[2]
for line in sys.stdin:
    line = line.rstrip("\n")
    if not line:
        continue
    name, kind, path = line.split("\t")
    out = {"name": name}
    try:
        records, sport = fit_streams.read_activity(path)
        s = fit_streams.streams(records)
        out["streams"] = s
        if kind in ("run", "bike") and s.get("heart_rate"):
            with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
                json.dump(s, f)
                tmp = f.name
            try:
                sys.argv = ["grade_metrics", "--streams", tmp, "--athlete", athlete, "--kind", kind]
                buf = io.StringIO()
                real = sys.stdout
                sys.stdout = buf
                try:
                    grade_metrics.main()
                finally:
                    sys.stdout = real
                out["metrics"] = json.loads(buf.getvalue())
            finally:
                os.unlink(tmp)
    except SystemExit as e:
        out["error"] = str(e)
    except Exception as e:
        out["error"] = repr(e)
    print(json.dumps(out), flush=True)
`

type gateJob struct{ name, kind, path string }

type gateResult struct {
	Name    string          `json:"name"`
	Streams json.RawMessage `json:"streams"`
	Metrics json.RawMessage `json:"metrics"`
	Error   string          `json:"error"`
}

func TestAcceptanceGate(t *testing.T) {
	corpus := os.Getenv("RC_CORPUS")
	if corpus == "" {
		t.Skip("RC_CORPUS not set — the whole-archive gate runs only where the archive lives")
	}
	requireAthleteData(t)
	d := liveData(t)

	// The corpus file list: pulled/ plus the backfill plan's sources.
	paths := map[string]string{}
	pulled, err := os.ReadDir(filepath.Join(corpus, "pulled"))
	if err != nil {
		t.Fatalf("corpus: %v", err)
	}
	for _, e := range pulled {
		if strings.HasSuffix(e.Name(), ".fit") {
			paths[e.Name()] = filepath.Join(corpus, "pulled", e.Name())
		}
	}
	planRaw, err := os.ReadFile(filepath.Join(corpus, "backfill-plan.json"))
	if err != nil {
		t.Fatalf("corpus: %v", err)
	}
	var plan map[string]string
	if err := json.Unmarshal(planRaw, &plan); err != nil {
		t.Fatalf("backfill-plan.json: %v", err)
	}
	for name, src := range plan {
		if _, ok := paths[name]; !ok {
			paths[name] = filepath.Join(corpus, "backfill", src)
		}
	}
	names := make([]string, 0, len(paths))
	for n := range paths {
		names = append(names, n)
	}
	sort.Strings(names)
	t.Logf("gate corpus: %d activities", len(names))

	// Go pass first: decode + register, and derive each file's kind for the
	// python side (grade_metrics needs it and takes it on trust).
	type goSide struct {
		streams *activityStreams
		metrics *activityMetrics
		err     error
	}
	goRes := make(map[string]*goSide, len(names))
	var mu sync.Mutex
	jobs := make(chan string)
	var wg sync.WaitGroup
	for w := 0; w < runtime.NumCPU(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for name := range jobs {
				data, err := os.ReadFile(paths[name])
				g := &goSide{}
				if err == nil {
					g.streams, g.err = decodeActivity(data)
				} else {
					g.err = err
				}
				if g.err == nil {
					g.metrics = computeMetrics(name, "0000-00-00", g.streams)
				}
				mu.Lock()
				goRes[name] = g
				mu.Unlock()
			}
		}()
	}
	for _, n := range names {
		jobs <- n
	}
	close(jobs)
	wg.Wait()

	decodeFailures := 0
	for _, n := range names {
		if goRes[n].err != nil {
			t.Errorf("go decode %s: %v", n, goRes[n].err)
			decodeFailures++
		}
	}
	if decodeFailures > 0 {
		t.Fatalf("%d of %d files failed the Go decode — the measured fact is all %d decode clean",
			decodeFailures, len(names), len(names))
	}

	// Python pass: shard the list over one helper process per CPU.
	helper := filepath.Join(t.TempDir(), "gate_helper.py")
	if err := os.WriteFile(helper, []byte(gateHelperPy), 0o644); err != nil {
		t.Fatal(err)
	}
	toolsDir, err := filepath.Abs("../tools")
	if err != nil {
		t.Fatal(err)
	}
	kindOf := func(sport string) string {
		switch sport {
		case "running":
			return "run"
		case "cycling":
			return "bike"
		}
		return ""
	}
	python := os.Getenv("RC_PYTHON") // the interpreter with fitdecode
	if python == "" {
		python = "python3"
	}
	shards := runtime.NumCPU()
	pyRes := make(map[string]*gateResult, len(names))
	var pwg sync.WaitGroup
	var perr []error
	for sh := 0; sh < shards; sh++ {
		pwg.Add(1)
		go func(sh int) {
			defer pwg.Done()
			cmd := exec.Command(python, helper, toolsDir, "./data/athlete.json")
			in, err := cmd.StdinPipe()
			if err == nil {
				var out *os.File
				out, err = os.CreateTemp(t.TempDir(), "gate-out-*")
				if err == nil {
					defer out.Close()
					cmd.Stdout = out
					cmd.Stderr = os.Stderr
					if err = cmd.Start(); err == nil {
						for i := sh; i < len(names); i += shards {
							n := names[i]
							fmt.Fprintf(in, "%s\t%s\t%s\n", n, kindOf(goRes[n].streams.Sport), paths[n])
						}
						in.Close()
						err = cmd.Wait()
					}
					if err == nil {
						_, err = out.Seek(0, 0)
					}
					if err == nil {
						sc := bufio.NewScanner(out)
						sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
						for sc.Scan() {
							var r gateResult
							if e := json.Unmarshal(sc.Bytes(), &r); e != nil {
								err = e
								break
							}
							mu.Lock()
							pyRes[r.Name] = &r
							mu.Unlock()
						}
						if err == nil {
							err = sc.Err()
						}
					}
				}
			}
			if err != nil {
				mu.Lock()
				perr = append(perr, fmt.Errorf("shard %d: %w", sh, err))
				mu.Unlock()
			}
		}(sh)
	}
	pwg.Wait()
	for _, e := range perr {
		t.Fatal(e)
	}

	// The comparison. Streams must match value-for-value; metrics must match
	// at the mirror's own rounding. One asymmetry is the mirror's own limit:
	// grade_metrics.py crashes (round(None)) on a file whose HR is entirely
	// dropout, where this register measures it as no-valid-HR — explained,
	// and only when the Go side agrees there is no valid HR.
	streamDiffs, metricDiffs, metricsCompared, mirrorRefused := 0, 0, 0, 0
	for _, n := range names {
		py := pyRes[n]
		if py == nil {
			t.Errorf("%s: no python result", n)
			streamDiffs++
			continue
		}
		if len(py.Streams) == 0 {
			t.Errorf("%s: python produced no streams (%s)", n, py.Error)
			streamDiffs++
			continue
		}
		if msg := diffStreams(goRes[n].streams, py.Streams); msg != "" {
			t.Errorf("%s: streams: %s", n, msg)
			streamDiffs++
		}
		if py.Error != "" {
			if why := mirrorCrashExplained(py.Error, goRes[n].metrics, goRes[n].streams, d); why != "" {
				mirrorRefused++
			} else {
				t.Errorf("%s: python metrics: %s", n, py.Error)
				metricDiffs++
			}
			continue
		}
		if len(py.Metrics) > 0 {
			metricsCompared++
			if msg := diffMetrics(d, goRes[n].metrics, goRes[n].streams, py.Metrics); msg != "" {
				t.Errorf("%s: metrics: %s", n, msg)
				metricDiffs++
			}
		}
	}
	t.Logf("gate: %d activities, %d stream diffs, %d metric diffs over %d comparisons, %d mirror-refused (all-dropout HR)",
		len(names), streamDiffs, metricDiffs, metricsCompared, mirrorRefused)
}

// mirrorCrashExplained names the mirror-side crash mode when grade_metrics
// dies on a degenerate value the Go register correctly omits — all measured
// crash modes are None reaching round() or arithmetic. Explained ONLY when
// the Go side agrees the corresponding value does not exist; anything else
// is a real diff.
func mirrorCrashExplained(errText string, m *activityMetrics, s *activityStreams, d *dataset) string {
	if !strings.Contains(errText, "__round__") && !strings.Contains(errText, "unsupported operand") {
		return ""
	}
	inBand, _ := bikeGradeInput(s, d.Athlete.HR["bikeLo"], d.Athlete.HR["bikeHi"], d.Athlete.HR["bikeCap"])
	switch {
	case m.AvgHR == nil:
		return "all-dropout HR"
	case s.Sport == "cycling" && inBand == nil:
		return "no after-warm-up window (ride under 600 s)"
	case m.HRDrift == nil:
		return "degenerate drift half"
	case s.Sport == "running" && runFirst20Mean(s) == nil:
		return "no valid first-20-min sample"
	}
	return ""
}

// diffStreams compares the Go streams against the mirror's JSON, exactly.
func diffStreams(g *activityStreams, raw json.RawMessage) string {
	var py struct {
		Time      []float64 `json:"time"`
		HeartRate []float64 `json:"heart_rate"`
		Watts     []float64 `json:"watts"`
		Velocity  []float64 `json:"velocity_smooth"`
		Cadence   []float64 `json:"cadence"`
	}
	if err := json.Unmarshal(raw, &py); err != nil {
		return err.Error()
	}
	if msg := diffInts("time", g.Time, py.Time); msg != "" {
		return msg
	}
	if (g.HaveHR) != (py.HeartRate != nil) {
		return fmt.Sprintf("heart_rate presence: go=%v py=%v", g.HaveHR, py.HeartRate != nil)
	}
	if g.HaveHR {
		if msg := diffInts("heart_rate", g.HR, py.HeartRate); msg != "" {
			return msg
		}
	}
	if (g.HaveWatts) != (py.Watts != nil) {
		return fmt.Sprintf("watts presence: go=%v py=%v", g.HaveWatts, py.Watts != nil)
	}
	if g.HaveWatts {
		if msg := diffInts("watts", g.Watts, py.Watts); msg != "" {
			return msg
		}
	}
	if (g.HaveVel) != (py.Velocity != nil) {
		return fmt.Sprintf("velocity presence: go=%v py=%v", g.HaveVel, py.Velocity != nil)
	}
	if g.HaveVel {
		if len(g.Vel) != len(py.Velocity) {
			return fmt.Sprintf("velocity length: go=%d py=%d", len(g.Vel), len(py.Velocity))
		}
		for i := range g.Vel {
			if g.Vel[i] != py.Velocity[i] {
				return fmt.Sprintf("velocity[%d]: go=%v py=%v", i, g.Vel[i], py.Velocity[i])
			}
		}
	}
	if (g.HaveCad) != (py.Cadence != nil) {
		return fmt.Sprintf("cadence presence: go=%v py=%v", g.HaveCad, py.Cadence != nil)
	}
	if g.HaveCad {
		if msg := diffInts("cadence", g.Cad, py.Cadence); msg != "" {
			return msg
		}
	}
	return ""
}

func diffInts(what string, g []int, py []float64) string {
	if len(g) != len(py) {
		return fmt.Sprintf("%s length: go=%d py=%d", what, len(g), len(py))
	}
	for i := range g {
		if float64(g[i]) != py[i] {
			return fmt.Sprintf("%s[%d]: go=%d py=%v", what, i, g[i], py[i])
		}
	}
	return ""
}

// diffMetrics compares the Go register's numbers, rounded the mirror's way,
// against grade_metrics.py's output for the same streams.
func diffMetrics(d *dataset, m *activityMetrics, s *activityStreams, raw json.RawMessage) string {
	var py map[string]any
	if err := json.Unmarshal(raw, &py); err != nil {
		return err.Error()
	}
	a := d.Athlete
	kind := "run"
	if s.Sport == "cycling" {
		kind = "bike"
	}

	var diffs []string
	check := func(what string, goVal *float64, pyVal any, digits int) {
		switch {
		case goVal == nil && pyVal == nil:
		case goVal == nil:
			diffs = append(diffs, fmt.Sprintf("%s: go absent, py %v", what, pyVal))
		case pyVal == nil:
			diffs = append(diffs, fmt.Sprintf("%s: go %v, py absent", what, *goVal))
		default:
			pv, ok := pyVal.(float64)
			if !ok || pyRound(*goVal, digits) != pv {
				diffs = append(diffs, fmt.Sprintf("%s: go %v (rounds %v), py %v",
					what, *goVal, pyRound(*goVal, digits), pyVal))
			}
		}
	}
	dig := func(m map[string]any, path ...string) any {
		var cur any = m
		for _, p := range path {
			mm, ok := cur.(map[string]any)
			if !ok {
				return nil
			}
			cur = mm[p]
		}
		return cur
	}

	if ev, ok := dig(py, "elapsed").(float64); !ok || int(ev) != m.ElapsedS {
		diffs = append(diffs, fmt.Sprintf("elapsed: go %d, py %v", m.ElapsedS, dig(py, "elapsed")))
	}
	if mv, ok := dig(py, "moving").(float64); !ok || int(mv) != m.MovingS {
		diffs = append(diffs, fmt.Sprintf("moving: go %d, py %v", m.MovingS, dig(py, "moving")))
	}
	check("hr.avg", m.AvgHR, dig(py, "hr", "avg"), 1)
	check("hr.dropout_share", m.DropoutShare, dig(py, "hr", "dropout_share"), 4)
	check("hr.drift", m.HRDrift, dig(py, "hr", "drift"), 1)
	if mx, ok := dig(py, "hr", "max").(float64); ok {
		if m.MaxHR == nil || *m.MaxHR != int(mx) {
			diffs = append(diffs, fmt.Sprintf("hr.max: go %v, py %v", m.MaxHR, mx))
		}
	} else if m.MaxHR != nil {
		diffs = append(diffs, fmt.Sprintf("hr.max: go %v, py absent", *m.MaxHR))
	}
	check("decoupling_pct", m.DecouplingPct, dig(py, "decoupling_pct"), 2)
	check("power.avg", m.AvgPower, dig(py, "power", "avg"), 1)
	if s.HaveWatts {
		mx := 0
		for _, w := range s.Watts {
			if w > mx {
				mx = w
			}
		}
		if pm, ok := dig(py, "power", "max").(float64); !ok || int(pm) != mx {
			diffs = append(diffs, fmt.Sprintf("power.max: go %d, py %v", mx, dig(py, "power", "max")))
		}
		if kind == "bike" {
			check("power.best_60s", bestRolling(s.Time, intsToFloats(s.Watts), 60),
				dig(py, "power", "best_60s"), 1)
		}
	}
	var cadOut *float64
	if m.AvgCadence != nil {
		c := *m.AvgCadence
		if kind == "run" {
			c *= 2
		}
		cadOut = &c
	}
	check("cadence", cadOut, dig(py, "cadence"), 1)

	if kind == "run" {
		check("grade_input.under_grade_cap_share",
			runGradeShare(s, a.HR["gradeCap"]), dig(py, "grade_input", "under_grade_cap_share"), 4)
		check("first_20min.avg", runFirst20Mean(s), dig(py, "first_20min", "avg"), 1)
	} else {
		inBand, secsOver := bikeGradeInput(s, a.HR["bikeLo"], a.HR["bikeHi"], a.HR["bikeCap"])
		check("grade_input.in_band_share_after_warmup",
			inBand, dig(py, "grade_input", "in_band_share_after_warmup"), 4)
		if so, ok := dig(py, "grade_input", "secs_over_cap").(float64); ok {
			if secsOver == nil || *secsOver != int(so) {
				diffs = append(diffs, fmt.Sprintf("secs_over_cap: go %v, py %v", secsOver, so))
			}
		} else if secsOver != nil {
			diffs = append(diffs, fmt.Sprintf("secs_over_cap: go %v, py absent", *secsOver))
		}
	}
	return strings.Join(diffs, "; ")
}
