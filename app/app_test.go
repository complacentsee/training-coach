package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"html/template"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func chicago(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("load tz: %v", err)
	}
	return loc
}

// requireAthleteData skips a test that needs the real plan files. The public
// checkout ships only the embedded defaults; the athlete's ./data exists on
// the machines that run the plan, and the suite must pass without it.
func requireAthleteData(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("./data/athlete.json"); err != nil {
		t.Skip("no ./data in this checkout — the defaults cover these code paths")
	}
}

func liveData(t *testing.T) *dataset {
	t.Helper()
	requireAthleteData(t)
	d, err := loadDataset("./data", chicago(t))
	if err != nil {
		t.Fatalf("load ./data: %v", err)
	}
	return d
}

/* ── units ─────────────────────────────────────────────────────────────── */

func TestQuantityRoundTrip(t *testing.T) {
	for _, s := range []string{"7 mi", "6.5 mi", "13.5 mi", "10 km", "400 m"} {
		d, err := ParseDistance(s)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		u := Imperial
		if strings.HasSuffix(s, "km") || strings.HasSuffix(s, " m") {
			u = Metric
		}
		if got := d.In(u); got != s {
			t.Errorf("ParseDistance(%q).In(%s) = %q, want %q", s, u, got, s)
		}
	}
	for _, s := range []string{"9:45/mi", "11:00/mi", "4:30/km", "6:03/km"} {
		p, err := ParsePace(s)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		u := Imperial
		if strings.HasSuffix(s, "/km") {
			u = Metric
		}
		if got := p.In(u); got != s {
			t.Errorf("ParsePace(%q).In(%s) = %q, want %q", s, u, got, s)
		}
	}
	for _, s := range []string{"25 lb", "50 lb", "5 lb", "12 kg", "77.1 kg"} {
		w, err := ParseWeight(s)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		u := Imperial
		if strings.HasSuffix(s, "kg") {
			u = Metric
		}
		if got := w.In(u); got != s {
			t.Errorf("ParseWeight(%q).In(%s) = %q, want %q", s, u, got, s)
		}
	}
}

func TestQuantityConversion(t *testing.T) {
	cases := []struct{ in, metric string }{
		{"6 mi", "9.7 km"},
		{"1 mi", "1.6 km"},
		{"13 mi", "20.9 km"},
	}
	for _, c := range cases {
		d, _ := ParseDistance(c.in)
		if got := d.In(Metric); got != c.metric {
			t.Errorf("%s in metric = %q, want %q", c.in, got, c.metric)
		}
	}
	// Bodyweight must not be rounded to whole kilos; 77.1 is not 77.
	w, _ := ParseWeight("77.1 kg")
	if got := w.In(Metric); got != "77.1 kg" {
		t.Errorf("77.1 kg in metric = %q, want %q", got, "77.1 kg")
	}
	p, _ := ParsePace("9:45/mi")
	if got := p.In(Metric); got != "6:04/km" {
		t.Errorf("9:45/mi in metric = %q, want %q", got, "6:04/km")
	}
}

func TestLongFormDistance(t *testing.T) {
	for _, c := range []struct {
		in   string
		u    Units
		want string
	}{
		{"27 mi", Imperial, "27 miles"},
		{"1 mi", Imperial, "1 mile"},
		{"6.5 mi", Imperial, "6.5 miles"},
		{"27 mi", Metric, "43.5 kilometres"},
		{"1 km", Metric, "1 kilometre"},
	} {
		d, err := ParseDistance(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if got := d.InLong(c.u); got != c.want {
			t.Errorf("%s long in %s = %q, want %q", c.in, c.u, got, c.want)
		}
	}
}

// emphasise is the only path by which block copy reaches the page unescaped.
// It must produce <strong>, <em> and <br> and nothing else, whatever the data
// says — a data file is not allowed to become a markup injection vector.
func TestEmphasiseEscapesEverythingElse(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"*Runs* are graded", "<strong>Runs</strong> are graded"},
		{"no markers here", "no markers here"},
		{"a *b* c *d*", "a <strong>b</strong> c <strong>d</strong>"},
		{"<script>alert(1)</script>", "&lt;script&gt;alert(1)&lt;/script&gt;"},
		{"*<img src=x onerror=y>*", "<strong>&lt;img src=x onerror=y&gt;</strong>"},
		{"5 > 3 & 2 < 4", "5 &gt; 3 &amp; 2 &lt; 4"},

		// The second emphasis level and line breaks, which session detail uses.
		{"_jog_ float", "<em>jog</em> float"},
		{"a _b_ c _d_", "a <em>b</em> c <em>d</em>"},
		{"*GOAL 5K*\n2 WU + RACE", "<strong>GOAL 5K</strong><br>2 WU + RACE"},
		{"*a _b_ c*", "<strong>a <em>b</em> c</strong>"},
		{"_<b>x</b>_", "<em>&lt;b&gt;x&lt;/b&gt;</em>"},
		// A newline inside emphasis still breaks, and still escapes.
		{"*a\n<i>b</i>*", "<strong>a<br>&lt;i&gt;b&lt;/i&gt;</strong>"},
	} {
		if got := string(emphasise(c.in)); got != c.want {
			t.Errorf("emphasise(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Session detail is the full prescription the published plan carried and the
// app did not. It resolves like every other data string, and it must never be
// a weaker copy of the label it sits under.
func TestSessionDetailResolvesAndAddsToTheLabel(t *testing.T) {
	d := liveData(t)
	b := d.Blocks[0]

	var withDetail int
	for _, w := range b.Weeks {
		c := b.ctxFor(d.Athlete, w.N)
		for di, s := range w.Days {
			if s.Detail == "" {
				continue
			}
			withDetail++
			got, err := c.resolve(s.Detail)
			if err != nil {
				t.Fatalf("week %d day %d: detail: %v", w.N, di+1, err)
			}
			if plain := strings.NewReplacer("*", "", "_", "").Replace(got); strings.EqualFold(plain, s.Label) {
				t.Errorf("week %d day %d: detail %q only restates the label %q",
					w.N, di+1, got, s.Label)
			}
		}
	}
	if withDetail == 0 {
		t.Fatal("no session carries detail; the migration did not land")
	}
}

// Week tags are authored because they cannot be derived: falling volume alone
// cannot tell a down week from a taper.
func TestWeekTagsAreAuthoredAndConsistent(t *testing.T) {
	d := liveData(t)
	b := d.Blocks[0]

	tagged := map[string][]int{}
	for _, w := range b.Weeks {
		for _, tag := range w.Tags {
			tagged[tag] = append(tagged[tag], w.N)
		}
	}
	if len(tagged) == 0 {
		t.Fatal("no week carries tags; the migration did not land")
	}
	// race must fall on the week that actually holds the race session.
	var raceWeeks []int
	for _, w := range b.Weeks {
		for _, s := range w.Days {
			if s.Tag == "RACE" {
				raceWeeks = append(raceWeeks, w.N)
			}
		}
	}
	if got := tagged["race"]; len(raceWeeks) > 0 && !sameInts(got, raceWeeks) {
		t.Errorf(`weeks tagged "race" are %v, but the RACE session is in %v`, got, raceWeeks)
	}
	// peak must be a week at the block's maximum volume.
	var max Distance
	for _, w := range b.Weeks {
		if v := w.Volume(); v > max {
			max = v
		}
	}
	for _, n := range tagged["peak"] {
		if v := b.WeekByN(n).Volume(); v != max {
			t.Errorf(`week %d is tagged "peak" but runs %s, not the block max %s`,
				n, v.In(Imperial), max.In(Imperial))
		}
	}
}

/* ── issues ────────────────────────────────────────────────────────────── */

func upto(n int) *int { return &n }

func testIssue(bands ...Band) Issue {
	return Issue{Key: "k", Name: "K", Scale: Scale{Min: 0, Max: 10}, Bands: bands}
}

// The bands are what turn a number into an instruction, so a set that leaves a
// gap, doubles back, or never terminates is worse than no bands at all — it
// would render a rating with no action against it.
func TestIssueBandsMustBeAscendingAndTotal(t *testing.T) {
	ok := testIssue(
		Band{UpTo: upto(2), Tone: "go", Label: "Go"},
		Band{UpTo: upto(5), Tone: "caution", Label: "Caution"},
		Band{Tone: "stop", Label: "Stop"},
	)
	if err := ok.validate(); err != nil {
		t.Fatalf("a well-formed issue was rejected: %v", err)
	}
	for v, want := range map[int]string{0: "go", 2: "go", 3: "caution", 5: "caution", 6: "stop", 10: "stop"} {
		if got := ok.BandFor(v); got == nil || got.Tone != want {
			t.Errorf("BandFor(%d) = %v, want tone %s", v, got, want)
		}
	}

	for name, bad := range map[string]Issue{
		"last band bounded": testIssue(
			Band{UpTo: upto(2), Tone: "go", Label: "Go"},
			Band{UpTo: upto(10), Tone: "stop", Label: "Stop"}),
		"bands do not advance": testIssue(
			Band{UpTo: upto(5), Tone: "go", Label: "Go"},
			Band{UpTo: upto(3), Tone: "caution", Label: "Caution"},
			Band{Tone: "stop", Label: "Stop"}),
		"upto off the scale": testIssue(
			Band{UpTo: upto(99), Tone: "go", Label: "Go"},
			Band{Tone: "stop", Label: "Stop"}),
		"unknown tone": testIssue(
			Band{UpTo: upto(2), Tone: "chartreuse", Label: "Go"},
			Band{Tone: "stop", Label: "Stop"}),
		"no label": testIssue(
			Band{UpTo: upto(2), Tone: "go"},
			Band{Tone: "stop", Label: "Stop"}),
		"no bands": testIssue(),
	} {
		if err := bad.validate(); err == nil {
			t.Errorf("%s should have been rejected", name)
		}
	}

	// A scale is a row of buttons on a phone; an inverted or enormous one is not.
	inverted := Issue{Key: "k", Name: "K", Scale: Scale{Min: 5, Max: 5},
		Bands: []Band{{Tone: "go", Label: "Go"}}}
	if err := inverted.validate(); err == nil {
		t.Error("a zero-width scale should have been rejected")
	}
	huge := Issue{Key: "k", Name: "K", Scale: Scale{Min: 0, Max: 100},
		Bands: []Band{{Tone: "go", Label: "Go"}}}
	if err := huge.validate(); err == nil {
		t.Error("a 101-point scale should have been rejected")
	}
}

// The log is append-only and is the athlete's health record. A hundred entries were
// written before issues were declared, with kind "calf" and no key; they have
// to keep reading as the issue they always were.
func TestLegacyCalfEntriesReadAsTheCalfIssue(t *testing.T) {
	dir := t.TempDir()
	log := `{"ts":"2026-08-03T07:00:00Z","date":"2026-08-03","kind":"calf","val":"2","note":"sore when pressed"}
{"ts":"2026-08-04T07:00:00Z","date":"2026-08-04","kind":"calf","val":"3"}
{"ts":"2026-08-05T07:00:00Z","date":"2026-08-05","kind":"issue","key":"calf","val":"1"}
{"ts":"2026-08-05T09:00:00Z","date":"2026-08-05","kind":"issue","key":"calf","val":"0","note":"re-rated"}
{"ts":"2026-08-05T07:00:00Z","date":"2026-08-05","kind":"issue","key":"hip","val":"4"}
{"ts":"2026-08-05T07:00:00Z","date":"2026-08-05","kind":"task","key":"hips","val":"done"}
`
	if err := os.WriteFile(filepath.Join(dir, "entries.jsonl"), []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	calf := s.Ratings("calf")
	if len(calf) != 3 {
		t.Fatalf("calf ratings = %d, want 3 (two legacy, one new)", len(calf))
	}
	if calf[0].Val != "2" || calf[0].Date != "2026-08-03" {
		t.Errorf("oldest calf rating = %+v, want the 3 Aug legacy entry", calf[0])
	}
	// Append-only: the re-rating wins, and it does not add a second row.
	if got := calf[2].Val; got != "0" {
		t.Errorf("5 Aug calf rating = %q, want the re-rating %q", got, "0")
	}
	if e := s.RatingOn("calf", "2026-08-05"); e == nil || e.Val != "0" || e.Note != "re-rated" {
		t.Errorf("RatingOn(calf, 5 Aug) = %+v, want the re-rating", e)
	}
	if e := s.RatingOn("calf", "2026-08-09"); e != nil {
		t.Errorf("RatingOn on a day with no rating = %+v, want nil", e)
	}
	// A different issue is a different series, and a task is neither.
	if got := s.Ratings("hip"); len(got) != 1 || got[0].Val != "4" {
		t.Errorf("hip ratings = %+v, want one entry", got)
	}
	if got := s.Ratings("hips"); len(got) != 0 {
		t.Errorf("a task key must not read as an issue: %+v", got)
	}
}

// The point of the exercise: nothing about the app is calf-specific any more.
// The embedded example carries a different issue on a different scale, so a
// regression that hardcodes either shows up here.
func TestTheExampleAthleteIsNotCarryingACalf(t *testing.T) {
	d, err := loadDataset(t.TempDir(), time.UTC) // empty volume → embedded defaults
	if err != nil {
		t.Fatalf("load defaults: %v", err)
	}
	if len(d.Athlete.Issues) != 1 {
		t.Fatalf("example athlete has %d issues, want 1", len(d.Athlete.Issues))
	}
	is := d.Athlete.Issues[0]
	if is.Key == "calf" {
		t.Error("the example issue is a calf again; it exists to prove the app is not calf-shaped")
	}
	if is.Scale.Min == 0 && is.Scale.Max == 10 {
		t.Error("the example scale is 0–10 again; it exists to prove the scale is not hardcoded")
	}
	if got := len(is.Scale.Values()); got != is.Scale.Max-is.Scale.Min+1 {
		t.Errorf("scale renders %d buttons for a %d–%d scale", got, is.Scale.Min, is.Scale.Max)
	}
	// Its rehab phases must cover the block, and belong to it.
	b := d.Blocks[0]
	for w := 1; w <= b.WeekCount(); w++ {
		if b.PhaseForIssue(is.Key, w) == nil {
			t.Errorf("week %d has no %s phase", w, is.Key)
		}
	}
}

// A phase naming an undeclared issue, or an issue pointing at a guide that is
// not in the library, both render something with no heading. Load time is the
// place to find that out.
func TestUndeclaredIssueReferencesAreRejected(t *testing.T) {
	d := liveData(t)
	b := d.Blocks[0]

	saved := b.Phases
	b.Phases = append([]Phase{{Name: "Ghost", Weeks: WeekRange{From: 1}, Issue: "shoulder"}}, saved...)
	if err := d.dryRun(b); err == nil {
		t.Error("a phase for an undeclared issue should have been rejected")
	}
	b.Phases = saved

	d.Athlete.Issues = append(d.Athlete.Issues, Issue{
		Key: "ghost", Name: "Ghost", Guide: "no-such-guide",
		Scale: Scale{Min: 0, Max: 3}, Bands: []Band{{Tone: "go", Label: "Go"}},
	})
	if err := d.dryRun(b); err == nil {
		t.Error("an issue pointing at a missing guide should have been rejected")
	}
}

// Eight long runs finish at marathon or threshold pace, and on those the
// block's governing rule inverts: pace is the input and heart rate is meant to
// climb through it. Keyed off the kind alone, the app told him to cap HR at 155
// and walk when the alarm fired — in the middle of a prescribed tempo. The
// published document had always known better, via a regex on the label; the
// session says it itself now, so both read the same thing and the regex is gone.
func TestPacedLongRunsDoNotGetTheAllEasyRule(t *testing.T) {
	d := liveData(t)
	b := d.Blocks[0]
	paced := regexp.MustCompile(`@\s*[MT]\b`)

	var withFinish, plain int
	for _, w := range b.Weeks {
		c := b.ctxFor(d.Athlete, w.N)
		for di, s := range w.Days {
			if s.Kind != KindLong {
				continue
			}
			sc := *c
			sc.Session = &s
			sc.InBlock = true
			lines, err := sc.resolveAll(b.TargetsFor(s))
			if err != nil {
				t.Fatalf("week %d day %d: %v", w.N, di+1, err)
			}
			joined := strings.Join(lines, " ")

			if paced.MatchString(s.Label + s.Detail) {
				withFinish++
				if strings.Contains(joined, "walk 60 s") {
					t.Errorf("week %d: a long run finishing at pace still says to walk when the alarm fires:\n  %s\n  %s",
						w.N, s.Label, joined)
				}
				if !strings.Contains(joined, "not on heart rate") {
					t.Errorf("week %d: a paced long run does not say pace governs the segment:\n  %s\n  %s",
						w.N, s.Label, joined)
				}
				continue
			}
			plain++
			if strings.Contains(joined, "not on heart rate") {
				t.Errorf("week %d: an all-easy long run was given the paced-finish rule:\n  %s", w.N, s.Label)
			}
		}
	}
	if withFinish == 0 || plain == 0 {
		t.Fatalf("expected both kinds of long run; got %d paced and %d all-easy", withFinish, plain)
	}
}

// Session targets replace the kind's rather than adding to them, because the
// lines they displace are not merely incomplete — they are wrong for that
// session. Most specific wins: session, then tag, then kind.
func TestTargetPrecedence(t *testing.T) {
	b := &Block{Targets: Targets{
		Kinds: map[string][]string{"long": {"kind line"}},
		Tags:  map[string][]string{"TT": {"tag line"}},
	}}
	for _, c := range []struct {
		name string
		s    Session
		want string
	}{
		{"kind only", Session{Kind: KindLong}, "kind line"},
		{"tag beats kind", Session{Kind: KindLong, Tag: "TT"}, "tag line"},
		{"session beats tag", Session{Kind: KindLong, Tag: "TT", Targets: []string{"session line"}}, "session line"},
		{"session beats kind", Session{Kind: KindLong, Targets: []string{"session line"}}, "session line"},
	} {
		got := b.TargetsFor(c.s)
		if len(got) != 1 || got[0] != c.want {
			t.Errorf("%s: TargetsFor = %v, want [%q]", c.name, got, c.want)
		}
	}
	// An unknown tag falls through to the kind rather than to nothing.
	if got := b.TargetsFor(Session{Kind: KindLong, Tag: "NOPE"}); len(got) != 1 || got[0] != "kind line" {
		t.Errorf("an unknown tag should fall through to the kind, got %v", got)
	}
}

// The lookahead showed one session. On the Friday of week 1 that meant
// Saturday's long run and then nothing until a benchmark eighteen days out —
// Sunday's recovery run, the one whose heart-rate ceiling is different, was
// invisible on the day you would plan the weekend.
func TestComingUpShowsTheRestOfTheWeekend(t *testing.T) {
	d := liveData(t)
	b := d.Blocks[0]
	guides, err := b.ResolveGuides(d.Athlete, 1)
	if err != nil {
		t.Fatal(err)
	}

	friday := b.DayOf(0, 4) // week 1, Friday — a rest day
	got := comingUp(b, guides, friday)
	if len(got) < 2 {
		t.Fatalf("from Friday the card shows %d rows, want the weekend: %+v", len(got), got)
	}
	if !strings.Contains(got[0].Label, "10 mi") || got[0].In != 1 {
		t.Errorf("first row is %+v, want tomorrow's long run", got[0])
	}
	if !strings.Contains(got[1].Label, "recovery") || got[1].In != 2 {
		t.Errorf("second row is %+v, want Sunday's recovery run", got[1])
	}

	// Rows are ordered by day. A tagged session can fall between the two
	// ordinary ones, so appending benchmarks last would put them out of order.
	for i := 1; i < len(got); i++ {
		if got[i].In < got[i-1].In {
			t.Errorf("rows are out of order: %+v", got)
			break
		}
	}

	// Nothing distant. The LT test is eighteen days from here, and a row about
	// it would be true, useless, and take the place of something imminent.
	for _, r := range got {
		if r.IsBench {
			t.Errorf("a benchmark %d days out is not 'coming up': %+v", r.In, r)
		}
	}

	// It appears when it enters the week, and not the day before that.
	lt := b.DayOf(3, 1) // week 4 Tuesday: the LT field test
	atHorizon := lt.AddDate(0, 0, -benchHorizon)
	if !hasBench(comingUp(b, guides, atHorizon)) {
		t.Errorf("no benchmark row %d days out, at the horizon", benchHorizon)
	}
	if hasBench(comingUp(b, guides, atHorizon.AddDate(0, 0, -1))) {
		t.Errorf("a benchmark row appeared %d days out, past the horizon", benchHorizon+1)
	}

	// A day that is both the next session and the next benchmark collapses to
	// one row — the benchmark's, which describes it better.
	beforeLT := b.DayOf(3, 0) // week 4 Monday; Tuesday is the LT test
	rows := comingUp(b, guides, beforeLT)
	seen := map[int]int{}
	for _, r := range rows {
		seen[r.In]++
	}
	for in, n := range seen {
		if n > 1 {
			t.Errorf("day +%d appears %d times: %+v", in, n, rows)
		}
	}
	var benches int
	for _, r := range rows {
		if r.IsBench {
			benches++
		}
	}
	if benches != 1 {
		t.Errorf("expected exactly one benchmark row from week 4 Monday, got %d: %+v", benches, rows)
	}

	// The last days of the block simply run out rather than wrapping.
	lastDay := b.DayOf(b.WeekCount()-1, 6)
	if rows := comingUp(b, guides, lastDay); len(rows) != 0 {
		t.Errorf("after the final session the card should be empty, got %+v", rows)
	}
}

func hasBench(rows []upcoming) bool {
	for _, r := range rows {
		if r.IsBench {
			return true
		}
	}
	return false
}

// renderToday executes the Today page against the live data, so a test can
// assert on what the browser is actually sent.
func renderToday(t *testing.T) string {
	t.Helper()
	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		t.Fatal(err)
	}
	a, err := newAssets(staticSub)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(t.TempDir()) // empty log: nothing rated today
	if err != nil {
		t.Fatal(err)
	}
	s := &server{assets: a, loc: chicago(t), dataDir: "./data", store: store}
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	tpl, err := template.New("").Funcs(s.makeFuncs()).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	s.tpl = tpl

	d := s.ds()
	day := s.day(d)
	blk := d.Current(day)
	td := todayData{
		Nav: "today", Title: "Today", Today: day,
		Issues: s.issueViews(d, blk, 1, day.Format("2006-01-02")),
	}
	var b strings.Builder
	if err := tpl.ExecuteTemplate(&b, "today.html", td); err != nil {
		t.Fatalf("render today.html: %v", err)
	}
	return b.String()
}

// Tapping a rating has to change the prescription with it. That means the card
// arrives carrying what each value means, because the alternative — fetching it
// — leaves the old instruction sitting under the new number until the next page
// load, and the instruction it leaves can be the opposite of the right one.
func TestRatingButtonsCarryTheirOwnMeaning(t *testing.T) {
	html := renderToday(t)
	d := liveData(t)
	is := &d.Athlete.Issues[0]

	// The element the tap fills must exist before there is anything to put in
	// it; it is hidden while empty.
	if !strings.Contains(html, `<p class="issue-action"`) {
		t.Error("no .issue-action element on an unrated card — a tap would have nothing to fill")
	}

	dot := regexp.MustCompile(`<button type="button" class="dot[^>]*data-val="(\d+)"[^>]*data-tone="([^"]*)"[^>]*data-label="([^"]*)"[^>]*data-rx="([^"]*)"`)
	found := dot.FindAllStringSubmatch(html, -1)
	if len(found) != len(is.Scale.Values()) {
		t.Fatalf("%d buttons carry the full attribute set, want %d", len(found), len(is.Scale.Values()))
	}
	for _, m := range found {
		v, _ := strconv.Atoi(m[1])
		want := is.BandFor(v)
		if want == nil {
			t.Errorf("value %d falls in no band", v)
			continue
		}
		if m[2] != want.Tone || m[3] != want.Label {
			t.Errorf("value %d carries tone/label %q/%q, want %q/%q", v, m[2], m[3], want.Tone, want.Label)
		}
		// html/template strips the "data-" prefix before applying its attribute
		// heuristics, so data-action was treated as a URL and percent-encoded —
		// the text arrived as "Train%20as%20written." and rendered as such.
		// Hence data-rx, and hence this check.
		if strings.Contains(m[4], "%20") {
			t.Errorf("value %d: the prescription was URL-encoded (%q) — does the attribute name collide with an html/template URL attribute?", v, m[4])
		}
	}
}

/* ── the artifact exporter ─────────────────────────────────────────────── */

// citesStale returns the 1-based line numbers where a superseded figure is
// stated as fact. A sentence that calls the number stale is the correction
// itself — "it came off a stale 225 W FTP and a stale 72.6 kg weight" has to be
// allowed to name the number it is retiring.
func citesStale(doc, figure string) []int {
	var out []int
	for i, line := range strings.Split(doc, "\n") {
		at := strings.Index(line, figure)
		if at < 0 {
			continue
		}
		from := at - 60
		if from < 0 {
			from = 0
		}
		if strings.Contains(strings.ToLower(line[from:at]), "stale") {
			continue
		}
		out = append(out, i+1)
	}
	return out
}

// The header used to read "16-WEEK BUILD" as a constant. It is the block's
// name, and the app's own name is a separate thing that outlives the block.
func TestBrandIsTheBlockAndTheAppNameIsNot(t *testing.T) {
	d := liveData(t)
	if got := d.Athlete.App.Name; got == "" {
		t.Error("the app has no name")
	}
	if strings.EqualFold(d.Athlete.App.Name, d.Blocks[0].Name) {
		t.Errorf("the app is named after the block (%q); a new block would rename the app", d.Athlete.App.Name)
	}
	// Defaults fill in, so an athlete.json that says nothing still installs.
	var bare Athlete
	bare.Schema = athleteSchema
	if err := bare.validate(); err != nil {
		t.Fatalf("an athlete declaring no app should still validate: %v", err)
	}
	if bare.App.Name == "" || bare.App.Short == "" ||
		bare.App.Theme.Light == "" || bare.App.Theme.Dark == "" {
		t.Errorf("app defaults are incomplete: %+v", bare.App)
	}
	bad := Athlete{Schema: athleteSchema, App: App{Theme: Theme{Light: "red", Dark: "#12171B"}}}
	if err := bad.validate(); err == nil {
		t.Error("a theme colour that is not a hex triplet should have been rejected")
	}
}

// Every helper that exists because a hand-computed figure went stale.
func TestBodyweightDerivations(t *testing.T) {
	d := liveData(t)
	c := d.Blocks[0].ctxFor(d.Athlete, 1)

	for _, tc := range []struct{ expr, want string }{
		{`{{perkg .Athlete.Power.ftp}}`, "2.78"}, // was hand-written as 2.95
		{`{{pctBW "50 lb"}}`, "29%"},             // was hand-written as 31%
		{`{{wt (bwPlus "25 lb")}}`, "195 lb"},    // was hand-written as 185 lb
		{`{{wkg 1.8}}`, "139"},
	} {
		got, err := c.resolve(tc.expr)
		if err != nil {
			t.Fatalf("%s: %v", tc.expr, err)
		}
		if got != tc.want {
			t.Errorf("%s = %q, want %q", tc.expr, got, tc.want)
		}
	}

	// Without a bodyweight they must refuse rather than invent one.
	bare := &Ctx{Athlete: &Athlete{}, Block: d.Blocks[0]}
	for _, expr := range []string{`{{perkg 214}}`, `{{pctBW "50 lb"}}`, `{{wt (bwPlus "25 lb")}}`} {
		if _, err := bare.resolve(expr); err == nil {
			t.Errorf("%s should refuse when the athlete has no weight", expr)
		}
	}
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestQuantitiesNeedUnits(t *testing.T) {
	for _, s := range []string{"7", "", "mi", "7 furlongs"} {
		if _, err := ParseDistance(s); err == nil {
			t.Errorf("ParseDistance(%q) should have failed", s)
		}
	}
	if _, err := ParsePace("9:45"); err == nil {
		t.Error("ParsePace(\"9:45\") should have failed: no unit")
	}
}

/* ── week ranges ───────────────────────────────────────────────────────── */

func TestWeekRange(t *testing.T) {
	cases := []struct {
		in   string
		want []int // weeks 1..6 that should match
	}{
		{"1-4", []int{1, 2, 3, 4}},
		{"5", []int{5}},
		{"4+", []int{4, 5, 6}},
		{"2–3", []int{2, 3}}, // en dash
	}
	for _, c := range cases {
		r, err := ParseWeekRange(c.in)
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		var got []int
		for w := 1; w <= 6; w++ {
			if r.Contains(w) {
				got = append(got, w)
			}
		}
		if len(got) != len(c.want) {
			t.Errorf("%q matched %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%q matched %v, want %v", c.in, got, c.want)
				break
			}
		}
	}
	for _, bad := range []string{"", "0-3", "4-2", "abc", "-3"} {
		if _, err := ParseWeekRange(bad); err == nil {
			t.Errorf("ParseWeekRange(%q) should have failed", bad)
		}
	}
}

func TestCoverageCatchesGapsAndOverlaps(t *testing.T) {
	mk := func(ss ...string) []WeekRange {
		out := make([]WeekRange, len(ss))
		for i, s := range ss {
			r, err := ParseWeekRange(s)
			if err != nil {
				t.Fatalf("%q: %v", s, err)
			}
			out[i] = r
		}
		return out
	}
	if err := checkCoverage("ok", mk("1-3", "4-9", "10-16"), 16); err != nil {
		t.Errorf("contiguous cover reported an error: %v", err)
	}
	if err := checkCoverage("gap", mk("1-3", "5-16"), 16); err == nil {
		t.Error("a gap at week 4 went undetected")
	} else if !strings.Contains(err.Error(), "4") {
		t.Errorf("gap error should name week 4, got %v", err)
	}
	if err := checkCoverage("overlap", mk("1-5", "4-16"), 16); err == nil {
		t.Error("an overlap at weeks 4-5 went undetected")
	}
	if err := checkCoverage("short", mk("1-10"), 16); err == nil {
		t.Error("a range that stops at week 10 of 16 went undetected")
	}
}

/* ── block dates ───────────────────────────────────────────────────────── */

// The timezone off-by-one lives here: a date parsed in UTC while the handler
// works in America/Chicago lands five hours away, which is enough for a
// whole-day difference to truncate to the wrong week.
func TestBlockBoundaries(t *testing.T) {
	d := liveData(t)
	b := d.Blocks[0]
	loc := b.location()

	day := func(s string) time.Time {
		tm, err := time.ParseInLocation("2006-01-02", s, loc)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		return tm
	}

	cases := []struct {
		date   string
		inside bool
		week   int
		dayIdx int
	}{
		{"2026-08-02", false, 0, 0}, // the Sunday before
		{"2026-08-03", true, 1, 0},  // first Monday
		{"2026-08-09", true, 1, 6},  // first Sunday
		{"2026-08-10", true, 2, 0},
		{"2026-11-16", true, 16, 0},
		{"2026-11-22", true, 16, 6}, // last day
		{"2026-11-23", false, 0, 0}, // the Monday after
	}
	for _, c := range cases {
		wk, di, ok := b.Locate(day(c.date))
		if ok != c.inside {
			t.Errorf("%s: inside=%v, want %v", c.date, ok, c.inside)
			continue
		}
		if !ok {
			continue
		}
		if wk.N != c.week || di != c.dayIdx {
			t.Errorf("%s: week %d day %d, want week %d day %d", c.date, wk.N, di, c.week, c.dayIdx)
		}
	}

	if got := b.EndDate().Format("2006-01-02"); got != "2026-11-22" {
		t.Errorf("EndDate = %s, want 2026-11-22", got)
	}
	// Every week's Monday must land on a Monday, in every hour of the day.
	for _, w := range b.Weeks {
		if wd := w.Start().Weekday(); wd != time.Monday {
			t.Errorf("week %d starts on a %s", w.N, wd)
		}
	}
}

func TestWeekDerivations(t *testing.T) {
	d := liveData(t)
	b := d.Blocks[0]

	cases := []struct {
		n      int
		dates  string
		volume string
		meso   string
	}{
		{1, "Aug 3–9", "27 mi", "Tissue Prep"},
		{5, "Aug 31–Sep 6", "30 mi", "Economy"},
		{9, "Sep 28–Oct 4", "33 mi", "VO₂max"},
		{16, "Nov 16–22", "23 mi", "Sharpen"},
	}
	for _, c := range cases {
		w := b.WeekByN(c.n)
		if got := w.Dates(); got != c.dates {
			t.Errorf("week %d dates = %q, want %q", c.n, got, c.dates)
		}
		if got := w.Volume().In(Imperial); got != c.volume {
			t.Errorf("week %d volume = %q, want %q", c.n, got, c.volume)
		}
		if got := w.Mesocycle().Name; got != c.meso {
			t.Errorf("week %d mesocycle = %q, want %q", c.n, got, c.meso)
		}
	}
}

// Phases and easy-pace bands change on different week boundaries. Collapsing
// them into one series would put phase B's start at week 5 instead of 4. The
// phases belong to the calf, not the block, which is why they are looked up by
// issue — the block's own phases are a separate, currently empty series.
func TestPhaseAndBandBoundariesDiffer(t *testing.T) {
	d := liveData(t)
	b := d.Blocks[0]

	phases := map[int]string{1: "A", 3: "A", 4: "B", 9: "B", 10: "C", 16: "C"}
	for week, want := range phases {
		p := b.PhaseForIssue("calf", week)
		if p == nil {
			t.Fatalf("week %d has no calf phase", week)
		}
		if !strings.Contains(p.Name, "Phase "+want) {
			t.Errorf("week %d is %q, want phase %s", week, p.Name, want)
		}
	}

	bands := map[int]string{
		1: "9:45–11:00/mi", 4: "9:45–11:00/mi",
		5: "9:30–10:30/mi", 9: "9:30–10:30/mi",
		10: "9:15–10:00/mi", 13: "9:15–10:00/mi",
		14: "8:45–9:30/mi", 16: "8:45–9:30/mi",
	}
	for week, want := range bands {
		got, err := b.ctxFor(d.Athlete, week).lookupVar("easyBand")
		if err != nil {
			t.Fatalf("week %d: %v", week, err)
		}
		if got != want {
			t.Errorf("week %d easyBand = %q, want %q", week, got, want)
		}
	}
}

/* ── resolution ────────────────────────────────────────────────────────── */

func TestTargetsResolveToTheOriginalProse(t *testing.T) {
	d := liveData(t)
	b := d.Blocks[0]
	c := b.ctxFor(d.Athlete, 1)

	got, err := c.resolveAll(b.Targets.Kinds["easy"])
	if err != nil {
		t.Fatalf("easy targets: %v", err)
	}
	want := []string{
		"HR cap 155 · first 20 min ≤ 145",
		"Target average 142–150",
		"Pace is an output: 9:45–11:00/mi",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("easy target %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// A step key is the log's identity for a checked-off movement, so one key must
// never mean two different movements. Positional keys broke this silently: in
// Strength A, "#3" was the banded psoas march in weeks 1-3 and the straight-knee
// heel raise from week 4 — the same log entry meaning two different exercises on
// the tissue this block exists to protect.
func TestOneKeyNeverMeansTwoMovements(t *testing.T) {
	d := liveData(t)
	b := d.Blocks[0]

	type seen struct {
		movement string
		week     int
	}
	first := map[string]seen{}
	for w := 1; w <= b.WeekCount(); w++ {
		gs, err := b.ResolveGuides(d.Athlete, w)
		if err != nil {
			t.Fatalf("week %d: %v", w, err)
		}
		for _, g := range gs {
			for _, st := range g.Steps {
				if st.Key == "" || st.Guide == "" {
					continue
				}
				if prev, ok := first[st.Key]; ok {
					if prev.movement != st.Guide {
						t.Errorf("key %q is %q in week %d but %q in week %d",
							st.Key, prev.movement, prev.week, st.Guide, w)
					}
					continue
				}
				first[st.Key] = seen{st.Guide, w}
			}
		}
	}
	if len(first) == 0 {
		t.Fatal("no keyed steps found at all")
	}
}

// Sets exist so a session cut short records what happened. The units the Today
// badge counts must be the sets, never the sets plus the exercise box.
func TestProgressUnitsCountSetsNotBoth(t *testing.T) {
	d := liveData(t)
	b := d.Blocks[0]
	gs, err := b.ResolveGuides(d.Athlete, 5) // Strength A at full six movements
	if err != nil {
		t.Fatal(err)
	}
	g := gs["task-strength-a"]

	want := 0
	for _, st := range g.Steps {
		if st.Sets < 2 {
			t.Errorf("step %q declares %d sets; every Strength A movement is multi-set", st.Key, st.Sets)
		}
		want += st.Sets
	}
	units := ProgressUnits(g)
	if len(units) != want {
		t.Errorf("ProgressUnits gave %d, want %d (the sum of sets)", len(units), want)
	}
	for _, u := range units {
		if !strings.Contains(u, ":") {
			t.Errorf("unit %q is an exercise key, not a set key — the two would double-count", u)
		}
	}

	// A single-set step contributes itself, not a phantom set key.
	hips := gs["task-hips"]
	for _, st := range hips.Steps {
		if st.Sets < 2 && len(st.SetKeys()) != 0 {
			t.Errorf("single-set step %q produced set keys %v", st.Key, st.SetKeys())
		}
	}
}

func TestGuideVariantsSwitchOnPhase(t *testing.T) {
	d := liveData(t)
	b := d.Blocks[0]
	for _, c := range []struct {
		week             int
		title, firstStep string
	}{
		{1, "Calf work — Phase A · tolerance", "5 × 45 s bent-knee isometric hold"},
		{5, "Calf work — Phase B · heavy slow", "4 × 8 straight-knee heel raise"},
		{12, "Calf work — Phase C · elastic", "3 × 30 pogo hops"},
	} {
		gs, err := b.ResolveGuides(d.Athlete, c.week)
		if err != nil {
			t.Fatalf("week %d: %v", c.week, err)
		}
		g := gs["task-calf"]
		if g.Title != c.title {
			t.Errorf("week %d title = %q, want %q", c.week, g.Title, c.title)
		}
		if len(g.Steps) == 0 || !strings.HasPrefix(g.Steps[0].Text, c.firstStep) {
			t.Errorf("week %d first step = %q, want prefix %q", c.week, g.Steps[0].Text, c.firstStep)
		}
	}
}

// Checklist rows are predicates, and outside the block there is no session at
// all. A nil-dereference here is a blank Today page on the day before it starts.
func TestChecklistPredicates(t *testing.T) {
	d := liveData(t)
	b := d.Blocks[0]

	keysOn := func(week, dayIdx int, inBlock bool) []string {
		c := b.ctxFor(d.Athlete, week)
		c.InBlock = inBlock
		if inBlock {
			sess := b.WeekByN(week).Days[dayIdx]
			c.Session = &sess
		}
		var out []string
		for _, tk := range checklist(c, b) {
			out = append(out, tk.key)
		}
		return out
	}

	// Week 1: Tue is the quality day, Fri is rest, Sun is a recovery run.
	for _, c := range []struct {
		name   string
		got    []string
		want   string
		absent string
	}{
		{"quality day gets Strength A", keysOn(1, 1, true), "strength-a", "strength-b"},
		{"rest day has no session row", keysOn(1, 4, true), "calf", "session"},
		{"outside the block", keysOn(1, 0, false), "calf-check", "session"},
	} {
		joined := strings.Join(c.got, ",")
		if !strings.Contains(joined, c.want) {
			t.Errorf("%s: %v should include %q", c.name, c.got, c.want)
		}
		for _, k := range c.got {
			if k == c.absent {
				t.Errorf("%s: %v should not include %q", c.name, c.got, c.absent)
			}
		}
	}
}

/* ── current vs archived ───────────────────────────────────────────────── */

// mkBlock builds a bare block of all-rest weeks, enough to exercise the date
// arithmetic without carrying a whole plan.
func mkBlock(t *testing.T, id, start string, weeks int, archived bool) *Block {
	t.Helper()
	b := &Block{Schema: blockSchema, ID: id, Name: id, Start: start, Archived: archived}
	for i := 1; i <= weeks; i++ {
		w := &Week{N: i, Days: make([]Session, 7), block: b}
		for d := range w.Days {
			w.Days[d] = Session{Kind: KindRest, Label: "Rest"}
		}
		b.Weeks = append(b.Weeks, w)
	}
	b.SetLocation(chicago(t))
	return b
}

func TestCurrentBlockSelection(t *testing.T) {
	loc := chicago(t)
	day := func(s string) time.Time {
		tm, err := time.ParseInLocation("2006-01-02", s, loc)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		return tm
	}
	spring := mkBlock(t, "spring", "2026-03-02", 8, false)  // 2 Mar – 26 Apr
	autumn := mkBlock(t, "autumn", "2026-08-03", 16, false) // 3 Aug – 22 Nov
	d := &dataset{Blocks: []*Block{spring, autumn}}         // sorted by start

	cases := []struct {
		name string
		date string
		want string
	}{
		{"inside the first", "2026-03-02", "spring"},
		{"last day of the first", "2026-04-26", "spring"},
		{"inside the second", "2026-08-05", "autumn"},
		{"last day of the second", "2026-11-22", "autumn"},
		// The gap between two blocks looks forward, so Today counts down to
		// the next one rather than reporting the old one complete.
		{"in the gap between them", "2026-05-15", "autumn"},
		{"the day after the first ends", "2026-04-27", "autumn"},
		{"the day before the second starts", "2026-08-02", "autumn"},
		// Nothing ahead: hold onto the most recent rather than show nothing.
		{"after everything", "2026-12-25", "autumn"},
		// Nothing started yet: the soonest to come.
		{"before everything", "2026-01-01", "spring"},
	}
	for _, c := range cases {
		got := d.Current(day(c.date))
		if got == nil {
			t.Errorf("%s (%s): no current block", c.name, c.date)
			continue
		}
		if got.ID != c.want {
			t.Errorf("%s (%s): current is %q, want %q", c.name, c.date, got.ID, c.want)
		}
	}
}

// Archived is for the block that was abandoned rather than finished — dates
// alone cannot say that, so it must override them.
func TestArchivedFlagOverridesDates(t *testing.T) {
	loc := chicago(t)
	inside, _ := time.ParseInLocation("2006-01-02", "2026-08-05", loc)

	autumn := mkBlock(t, "autumn", "2026-08-03", 16, true) // contains the day, but archived
	spring := mkBlock(t, "spring", "2026-03-02", 8, false)
	d := &dataset{Blocks: []*Block{spring, autumn}}

	if got := d.Current(inside); got.ID != "spring" {
		t.Errorf("current is %q, want the un-archived %q even though autumn contains today", got.ID, "spring")
	}
	if d.IsCurrent(autumn, inside) {
		t.Error("an archived block reported itself as current")
	}

	// Everything archived: still show something rather than a blank front page.
	spring.Archived = true
	if got := d.Current(inside); got == nil {
		t.Error("all-archived gave no block at all")
	} else if got.ID != "autumn" {
		t.Errorf("all-archived fell back to %q, want the most recent %q", got.ID, "autumn")
	}
}

func TestBlockForResolvesQueryParam(t *testing.T) {
	loc := chicago(t)
	today, _ := time.ParseInLocation("2006-01-02", "2026-08-05", loc)
	spring := mkBlock(t, "spring", "2026-03-02", 8, false)
	autumn := mkBlock(t, "autumn", "2026-08-03", 16, false)
	d := &dataset{Blocks: []*Block{spring, autumn}}

	if b, ok := d.blockFor("", today); !ok || b.ID != "autumn" {
		t.Errorf("empty param gave %v, want the current block", b)
	}
	if b, ok := d.blockFor("spring", today); !ok || b.ID != "spring" {
		t.Errorf("explicit id gave %v, want spring", b)
	}
	// A stale bookmark must 404, not silently show a different block's plan.
	if _, ok := d.blockFor("no-such-block", today); ok {
		t.Error("an unknown block id resolved instead of failing")
	}
}

func TestBlockSpanAndVolume(t *testing.T) {
	d := liveData(t)
	b := d.Blocks[0]
	if got := b.Span(); got != "Aug – Nov 2026" {
		t.Errorf("Span = %q, want %q", got, "Aug – Nov 2026")
	}
	// 484 miles is the sum of all sixteen weekly totals.
	if got := b.Volume().InLong(Imperial); got != "484 miles" {
		t.Errorf("Volume = %q, want %q", got, "484 miles")
	}
}

/* ── loading ───────────────────────────────────────────────────────────── */

func TestEmbeddedDefaultsStandAlone(t *testing.T) {
	dir := t.TempDir() // empty: nothing on the volume at all
	d, err := loadDataset(dir, time.UTC)
	if err != nil {
		t.Fatalf("a fresh container must serve the embedded example: %v", err)
	}
	if len(d.Blocks) != 1 {
		t.Errorf("embedded defaults gave %d blocks, want 1", len(d.Blocks))
	}
	for logical, origin := range d.Origins {
		if !strings.HasPrefix(origin, "embedded:") {
			t.Errorf("%s came from %q, want the embedded copy", logical, origin)
		}
	}
}

// The volume replaces a directory wholesale. Merging file by file let the
// example guides leak in beside the real ones, and made which definition won
// depend on the alphabet.
func TestVolumeReplacesTheLibraryWholesale(t *testing.T) {
	d := liveData(t)
	if _, leaked := d.Library["m-example"]; leaked {
		t.Error("the embedded example library leaked into the live one")
	}
	if got := len(d.Library); got != 45 {
		t.Errorf("live library has %d guides, want 45", got)
	}
}

// A block may override a library guide by id, for the block that wants a
// movement cued differently without forking the whole library. The code path
// has existed since the templating rewrite and was validated at load, but had
// never been exercised with real content — so this is the first thing that
// proves an override actually reaches a page, and that it stays inside the
// block that declares it.
func TestPerBlockGuideOverride(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{"blocks", "library"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	copyFile(t, "./data/athlete.json", filepath.Join(dir, "athlete.json"))
	for _, n := range []string{"movements.json", "sessions.json", "tasks.json", "index.json"} {
		copyFile(t, filepath.Join("./data/library", n), filepath.Join(dir, "library", n))
	}

	// Two blocks off one library. The second overrides a movement; the first
	// must not notice.
	requireAthleteData(t)
	raw, err := os.ReadFile("./data/blocks/2026-08-16-week-build.json")
	if err != nil {
		t.Fatal(err)
	}
	var plain map[string]any
	if err := json.Unmarshal(raw, &plain); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blocks", "a.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	const overridden = "Bent-knee isometric hold — CUED FOR THIS BLOCK ONLY"
	plain["id"] = "override-block"
	plain["start"] = "2027-08-02" // a Monday, well clear of the first block
	plain["guides"] = map[string]any{
		"m-iso": map[string]any{
			"id":    "m-iso",
			"title": overridden,
			"why":   "{{var \"easyBand\"}} still resolves inside an override.",
		},
	}
	out, err := json.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blocks", "b.json"), out, 0o644); err != nil {
		t.Fatal(err)
	}

	d, err := loadDataset(dir, time.UTC)
	if err != nil {
		t.Fatalf("a block with a guide override failed to load: %v", err)
	}
	if len(d.Blocks) != 2 {
		t.Fatalf("loaded %d blocks, want 2", len(d.Blocks))
	}

	var base, over *Block
	for _, b := range d.Blocks {
		if b.ID == "override-block" {
			over = b
		} else {
			base = b
		}
	}
	if base == nil || over == nil {
		t.Fatal("could not find both blocks")
	}

	og, err := over.ResolveGuides(d.Athlete, 1)
	if err != nil {
		t.Fatalf("overriding block: %v", err)
	}
	if got := og["m-iso"].Title; got != overridden {
		t.Errorf("override did not win: m-iso title = %q, want %q", got, overridden)
	}
	// The override is a whole guide, not a merge: fields it omits are gone.
	if got := og["m-iso"].Sections; len(got) != 0 {
		t.Errorf("override merged with the library entry; it should replace it (%d sections survived)", len(got))
	}
	// Templates inside an override resolve against the overriding block.
	if why := og["m-iso"].Why; !strings.Contains(why, "/mi") {
		t.Errorf("a template inside an override did not resolve: %q", why)
	}
	// Every other guide still comes from the library.
	if got := og["m-dbl"].Title; got != "Double-leg heel raise" {
		t.Errorf("an unrelated guide changed under the override: %q", got)
	}

	bg, err := base.ResolveGuides(d.Athlete, 1)
	if err != nil {
		t.Fatalf("base block: %v", err)
	}
	if got := bg["m-iso"].Title; got == overridden {
		t.Error("one block's override leaked into another block")
	}
	if len(bg["m-iso"].Sections) == 0 {
		t.Error("the base block lost the library's sections")
	}
	// And the shared library itself is untouched.
	if d.Library["m-iso"].Title == overridden {
		t.Error("the override mutated the shared library")
	}
}

// macOS tar ships AppleDouble "._name.json" side-files, which are binary. They
// once reached the server, were parsed as data, and crash-looped the container.
func TestHiddenFilesAreIgnored(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{"blocks", "library"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	copyFile(t, "./data/athlete.json", filepath.Join(dir, "athlete.json"))
	for _, n := range []string{"movements.json", "sessions.json", "tasks.json", "index.json"} {
		copyFile(t, filepath.Join("./data/library", n), filepath.Join(dir, "library", n))
	}
	copyFile(t, "./data/blocks/2026-08-16-week-build.json", filepath.Join(dir, "blocks", "b.json"))

	junk := []byte{0x00, 0x05, 0x16, 0x07, 0x00, 0x02, 0x00, 0x00}
	for _, p := range []string{
		"._athlete.json", "blocks/._b.json", "library/._index.json", "library/.DS_Store.json",
	} {
		if err := os.WriteFile(filepath.Join(dir, p), junk, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	d, err := loadDataset(dir, time.UTC)
	if err != nil {
		t.Fatalf("AppleDouble side-files broke the load: %v", err)
	}
	if len(d.Blocks) != 1 {
		t.Errorf("got %d blocks, want 1", len(d.Blocks))
	}
	// Junk must not perturb the content hash either, or verify reports a
	// mismatch that no amount of re-pushing will fix.
	if fingerprint(dir) == "" {
		t.Error("fingerprint came back empty")
	}
}

func TestValidationRejectsBadData(t *testing.T) {
	requireAthleteData(t)
	base, err := os.ReadFile("./data/blocks/2026-08-16-week-build.json")
	if err != nil {
		t.Fatalf("read block: %v", err)
	}

	cases := []struct {
		name   string
		mangle func(map[string]any)
		expect string
	}{
		{"a phase gap", func(m map[string]any) {
			m["phases"] = []any{map[string]any{"name": "A", "weeks": "1-3", "detail": "x"}}
		}, "no entry covers week"},
		{"a var that stops short", func(m map[string]any) {
			m["vars"] = map[string]any{"easyBand": []any{
				map[string]any{"weeks": "1-4", "value": "x"}}}
		}, "no entry covers week"},
		{"an unknown guide in the checklist", func(m map[string]any) {
			m["checklist"] = []any{map[string]any{"key": "k", "label": "l", "guide": "task-nope"}}
		}, "not in the library"},
		{"a start that is not a Monday", func(m map[string]any) {
			m["start"] = "2026-08-04"
		}, "blocks begin on a Monday"},
		{"an unknown athlete field", func(m map[string]any) {
			m["targets"] = map[string]any{"kinds": map[string]any{
				"easy": []any{"HR cap {{.Athlete.HR.nosuchkey}}"}}}
		}, "nosuchkey"},
		{"a stale schema version", func(m map[string]any) {
			m["schema"] = "block/0"
		}, "this build understands"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var m map[string]any
			if err := json.Unmarshal(base, &m); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			c.mangle(m)

			dir := t.TempDir()
			for _, sub := range []string{"blocks", "library"} {
				if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			copyFile(t, "./data/athlete.json", filepath.Join(dir, "athlete.json"))
			for _, n := range []string{"movements.json", "sessions.json", "tasks.json", "index.json"} {
				copyFile(t, filepath.Join("./data/library", n), filepath.Join(dir, "library", n))
			}
			out, _ := json.Marshal(m)
			if err := os.WriteFile(filepath.Join(dir, "blocks", "b.json"), out, 0o644); err != nil {
				t.Fatal(err)
			}

			_, err := loadDataset(dir, time.UTC)
			if err == nil {
				t.Fatalf("%s loaded without complaint", c.name)
			}
			if !strings.Contains(err.Error(), c.expect) {
				t.Errorf("error was %q, want it to mention %q", err, c.expect)
			}
		})
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	if strings.HasPrefix(from, "./data") {
		requireAthleteData(t)
	}
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatalf("read %s: %v", from, err)
	}
	if err := os.WriteFile(to, b, 0o644); err != nil {
		t.Fatalf("write %s: %v", to, err)
	}
}

// A reload that fails must leave the previous data serving. Half-applied data
// is worse than slightly stale data.
func TestFailedReloadKeepsServing(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{"blocks", "library"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	copyFile(t, "./data/athlete.json", filepath.Join(dir, "athlete.json"))
	for _, n := range []string{"movements.json", "sessions.json", "tasks.json", "index.json"} {
		copyFile(t, filepath.Join("./data/library", n), filepath.Join(dir, "library", n))
	}
	blockPath := filepath.Join(dir, "blocks", "b.json")
	copyFile(t, "./data/blocks/2026-08-16-week-build.json", blockPath)

	s := &server{dataDir: dir, loc: time.UTC}
	if err := s.reload(); err != nil {
		t.Fatalf("first load: %v", err)
	}
	good := s.ds().Rev

	if err := os.WriteFile(blockPath, []byte("{ this is not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.reload(); err == nil {
		t.Fatal("reload accepted a corrupt block")
	}
	if s.ds().Rev != good {
		t.Errorf("a failed reload swapped the data anyway: rev %s → %s", good, s.ds().Rev)
	}
	if len(s.ds().Blocks) != 1 {
		t.Error("the previously loaded block is no longer being served")
	}
}

/* ── FIT workout steps ─────────────────────────────────────────────────── */

func TestParseStepTime(t *testing.T) {
	good := map[string]int{
		"0:20": 20, "3:00": 180, "75:00": 4500, "1:15:00": 4500, "0:59": 59,
	}
	for in, want := range good {
		got, err := parseStepTime(in)
		if err != nil {
			t.Errorf("parseStepTime(%q): %v", in, err)
		} else if got != want {
			t.Errorf("parseStepTime(%q) = %d, want %d", in, got, want)
		}
	}
	// A bare "180" is the killer: parseClock would read it as seconds, so a
	// three-minute rep typed without its colon would silently become three.
	for _, in := range []string{"180", "3:75", "0:00", "-1:00", "1:2:3:4", "abc", "3:"} {
		if _, err := parseStepTime(in); err == nil {
			t.Errorf("parseStepTime(%q) accepted", in)
		}
	}
}

func TestFitNameDerivation(t *testing.T) {
	cases := []struct {
		week  int
		day   int
		label string
		name  string
		slug  string
	}{
		// The worked example from the plan: strip, transliterate, slug.
		{6, 1, "4×5:00 @ T", "W06 Tu 4x5:00 @ T", "W06-Tu-4x5_00-T.fit"},
		// Over 24 characters cuts at a word boundary.
		{1, 1, "Easy run + 4 × 20 s strides", "W01 Tu Easy run + 4 x", "W01-Tu-Easy-run-4-x.fit"},
		// Emphasis is stripped, not rendered; typography maps to ASCII.
		{12, 5, "*Race day* — 5K", "W12 Sa Race day - 5K", "W12-Sa-Race-day-5K.fit"},
	}
	for _, c := range cases {
		name, err := fitName(c.week, c.day, c.label)
		if err != nil {
			t.Errorf("fitName(%d, %d, %q): %v", c.week, c.day, c.label, err)
			continue
		}
		if name != c.name {
			t.Errorf("fitName(%d, %d, %q) = %q, want %q", c.week, c.day, c.label, name, c.name)
		}
		if len(name) > fitNameMax {
			t.Errorf("%q is %d chars, cap is %d", name, len(name), fitNameMax)
		}
		if got := fitSlug(name); got != c.slug {
			t.Errorf("fitSlug(%q) = %q, want %q", name, got, c.slug)
		}
	}
	// An unmapped rune must refuse loudly, not drop.
	if _, err := fitName(1, 0, "Café run"); err == nil || !strings.Contains(err.Error(), "transliterate") {
		t.Errorf("unmapped rune: err = %v, want a transliteration refusal", err)
	}
}

// Every steps day of both fixtures gets a name the watch can hold: printable
// ASCII, and unique in its first 15 characters within the block. The week+day
// prefix makes a real collision unreachable through data, so the collision
// branch is exercised on synthetic names.
func TestWorkoutNamesAreASCIIAndUniqueInFifteen(t *testing.T) {
	for _, dir := range []string{"./data", ""} {
		d, err := loadDataset(dir, chicago(t))
		if err != nil {
			t.Fatalf("load %q: %v", dir, err)
		}
		for _, b := range d.Blocks {
			var names []fitNamed
			for _, w := range b.Weeks {
				for di, sess := range w.Days {
					if len(sess.Steps) == 0 {
						continue
					}
					name, err := fitName(w.N, di, sess.Label)
					if err != nil {
						t.Errorf("%s week %d day %d: %v", b.ID, w.N, di+1, err)
						continue
					}
					if err := fitASCIIField("name", name); err != nil {
						t.Errorf("%s week %d day %d: %v", b.ID, w.N, di+1, err)
					}
					names = append(names, fitNamed{Day: b.ID, Name: name})
				}
			}
			if err := checkFitNameCollisions(names); err != nil {
				t.Errorf("%s: %v", b.ID, err)
			}
		}
	}

	collide := []fitNamed{
		{Day: "week 1 day 2", Name: "W01 Tu Same Name Here A"},
		{Day: "week 1 day 4", Name: "W01 Tu Same Name Here B"},
	}
	if err := checkFitNameCollisions(collide); err == nil || !strings.Contains(err.Error(), "15") {
		t.Errorf("first-15 collision: err = %v, want the dedupe refusal", err)
	}
	distinct := []fitNamed{
		{Day: "week 1 day 2", Name: "W01 Tu Strides"},
		{Day: "week 2 day 2", Name: "W02 Tu Strides"},
	}
	if err := checkFitNameCollisions(distinct); err != nil {
		t.Errorf("distinct prefixes refused: %v", err)
	}
}

// TestValidationRejectsBadSteps is the steps half of the mangle table: one
// case per fatal loader rule, each written into a copy of the live block so
// the refusal comes from the same path a bad push would take.
func TestValidationRejectsBadSteps(t *testing.T) {
	requireAthleteData(t)
	base, err := os.ReadFile("./data/blocks/2026-08-16-week-build.json")
	if err != nil {
		t.Fatalf("read block: %v", err)
	}

	leaf := func(kv map[string]any) map[string]any { return kv }
	longNote := strings.Repeat("x", 201)
	fiftyOne := make([]any, 51)
	for i := range fiftyOne {
		fiftyOne[i] = leaf(map[string]any{"role": "active", "time": "0:30"})
	}

	cases := []struct {
		name   string
		day    int // index into week 1: 0 is a bike, 1 is a 7 mi quality run
		label  string
		steps  []any
		expect string
	}{
		{"steps on a rest day", 4, "", []any{
			leaf(map[string]any{"role": "active", "time": "5:00", "hr": []any{140, 150}})},
			"runs and trainer rides"},
		{"dist on a bike step", 0, "", []any{
			leaf(map[string]any{"role": "active", "dist": "10 mi", "power": []any{140, 150}})},
			"trainer steps are timed"},
		{"pace on a bike step", 0, "", []any{
			leaf(map[string]any{"role": "active", "time": "5:00", "pace": []any{"7:00/mi", "7:10/mi"}})},
			"the trainer takes power"},
		{"power on a run step", 1, "", []any{
			leaf(map[string]any{"role": "active", "time": "5:00", "power": []any{140, 150}})},
			"not in the dialect"},
		{"a power band out of range", 0, "", []any{
			leaf(map[string]any{"role": "active", "time": "5:00", "power": []any{10, 150}})},
			"outside 20–1000"},
		{"a high-first power band", 0, "", []any{
			leaf(map[string]any{"role": "active", "time": "5:00", "power": []any{250, 200}})},
			"high-first"},
		{"a zero-width power band", 0, "", []any{
			leaf(map[string]any{"role": "active", "time": "5:00", "power": []any{252, 252}})},
			"no width"},
		{"a bike day over its minutes", 0, "", []any{
			leaf(map[string]any{"role": "active", "time": "50:00", "power": []any{130, 146}})},
			"more than 2% over"},
		{"repeat and leaf fields mixed", 1, "", []any{
			map[string]any{"repeat": 2, "role": "active",
				"steps": []any{leaf(map[string]any{"role": "active", "time": "1:00"})}}},
			"mixes repeat and leaf"},
		{"repeat below two", 1, "", []any{
			map[string]any{"repeat": 1,
				"steps": []any{leaf(map[string]any{"role": "active", "time": "1:00"})}}},
			"at least twice"},
		{"repeat with no body", 1, "", []any{
			map[string]any{"repeat": 3}},
			"empty body"},
		{"a nested repeat", 1, "", []any{
			map[string]any{"repeat": 2, "steps": []any{
				map[string]any{"repeat": 2,
					"steps": []any{leaf(map[string]any{"role": "active", "time": "1:00"})}}}}},
			"repeats cannot nest"},
		{"no role", 1, "", []any{
			leaf(map[string]any{"time": "1:00"})},
			"no role"},
		{"a templated role", 1, "", []any{
			leaf(map[string]any{"role": "{{.Kind}}", "time": "1:00"})},
			"roles are literal"},
		{"an unknown role", 1, "", []any{
			leaf(map[string]any{"role": "sprint", "time": "1:00"})},
			"unknown role"},
		{"dist and time together", 1, "", []any{
			leaf(map[string]any{"role": "active", "dist": "1 mi", "time": "1:00"})},
			"exactly one of dist or time"},
		{"neither dist nor time", 1, "", []any{
			leaf(map[string]any{"role": "active"})},
			"exactly one of dist or time"},
		{"pace and hr together", 1, "", []any{
			leaf(map[string]any{"role": "active", "time": "4:00",
				"pace": []any{"7:00/mi", "7:10/mi"}, "hr": []any{140, 150}})},
			"at most one target"},
		{"a one-ended pace band", 1, "", []any{
			leaf(map[string]any{"role": "active", "time": "4:00", "pace": []any{"7:00/mi"}})},
			"want two"},
		{"a three-ended hr band", 1, "", []any{
			leaf(map[string]any{"role": "active", "time": "4:00", "hr": []any{140, 150, 160}})},
			"want two"},
		{"past the step budget", 1, "", fiftyOne,
			"at most 50"},
		{"a template typo", 1, "", []any{
			leaf(map[string]any{"role": "warmup", "dist": "2 mi",
				"hr": []any{"{{.Athlete.HR.nosuchkey}}", 150}})},
			"nosuchkey"},
		{"an unparseable dist", 1, "", []any{
			leaf(map[string]any{"role": "warmup", "dist": "fast"})},
			"does not start with a number"},
		{"a zero dist", 1, "", []any{
			leaf(map[string]any{"role": "warmup", "dist": "0 mi"})},
			"not a positive distance"},
		{"an unparseable pace", 1, "", []any{
			leaf(map[string]any{"role": "active", "time": "4:00",
				"pace": []any{"fast", "7:30/mi"}})},
			`want "M:SS/mi" or "M:SS/km"`},
		{"a zero pace", 1, "", []any{
			leaf(map[string]any{"role": "active", "time": "4:00",
				"pace": []any{"0:00/mi", "7:30/mi"}})},
			"non-positive pace"},
		{"an hr bound resolving to no number", 1, "", []any{
			leaf(map[string]any{"role": "active", "time": "4:00",
				"hr": []any{"easy", 150}})},
			"did not resolve to a bpm number"},
		{"a colonless time", 1, "", []any{
			leaf(map[string]any{"role": "active", "time": "180", "hr": []any{140, 150}})},
			"colon is required"},
		{"seconds over 59", 1, "", []any{
			leaf(map[string]any{"role": "active", "time": "3:75", "hr": []any{140, 150}})},
			"must be under 60"},
		{"a slow-first pace band", 1, "", []any{
			leaf(map[string]any{"role": "active", "time": "4:00",
				"pace": []any{"7:30/mi", "7:20/mi"}})},
			"slow-first"},
		{"a high-first hr band", 1, "", []any{
			leaf(map[string]any{"role": "active", "time": "4:00", "hr": []any{160, 140}})},
			"high-first"},
		{"an hr bound outside the human range", 1, "", []any{
			leaf(map[string]any{"role": "active", "time": "4:00", "hr": []any{20, 140}})},
			"outside 30–250"},
		{"an untargeted two-minute active", 1, "", []any{
			leaf(map[string]any{"role": "active", "time": "2:00"})},
			"no countdown"},
		{"a note past the display cap", 1, "", []any{
			leaf(map[string]any{"role": "active", "time": "0:20", "note": longNote})},
			"at most 200"},
		{"an untransliterable note", 1, "", []any{
			leaf(map[string]any{"role": "active", "time": "0:20", "note": "☂ if wet"})},
			"cannot transliterate"},
		{"a template resolving to nothing", 1, "", []any{
			leaf(map[string]any{"role": "warmup", "dist": "{{if false}}2 mi{{end}}"})},
			"resolved to nothing"},
		{"steps measuring over the session", 1, "", []any{
			leaf(map[string]any{"role": "warmup", "dist": "8 mi"})},
			"more than 2% over"},
		{"an untransliterable label", 1, "☂ mystery run", []any{
			leaf(map[string]any{"role": "warmup", "dist": "2 mi"})},
			"cannot transliterate"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var m map[string]any
			if err := json.Unmarshal(base, &m); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			day := m["weeks"].([]any)[0].(map[string]any)["days"].([]any)[c.day].(map[string]any)
			day["steps"] = c.steps
			if c.label != "" {
				day["label"] = c.label
			}

			dir := t.TempDir()
			for _, sub := range []string{"blocks", "library"} {
				if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			copyFile(t, "./data/athlete.json", filepath.Join(dir, "athlete.json"))
			for _, n := range []string{"movements.json", "sessions.json", "tasks.json", "index.json"} {
				copyFile(t, filepath.Join("./data/library", n), filepath.Join(dir, "library", n))
			}
			out, _ := json.Marshal(m)
			if err := os.WriteFile(filepath.Join(dir, "blocks", "b.json"), out, 0o644); err != nil {
				t.Fatal(err)
			}

			_, err := loadDataset(dir, time.UTC)
			if err == nil {
				t.Fatalf("%s loaded without complaint", c.name)
			}
			if !strings.Contains(err.Error(), c.expect) {
				t.Errorf("error was %q, want it to mention %q", err, c.expect)
			}
		})
	}
}

// TestStepsAgreeWithTheSessionTheyElaborate is the live-data guard, landed
// with the first authored steps. The reconciliation half re-derives what the
// loader already enforces, pinned here so it shows up as a named red test
// rather than a container restart-loop. The migration half is the real
// tripwire: the loader silently drops an unknown JSON key, so a push where
// the field was renamed or stripped would load perfectly and serve a plan
// with no downloads — a live block must carry at least one steps day from
// the tracer onward.
func TestStepsAgreeWithTheSessionTheyElaborate(t *testing.T) {
	d := liveData(t)
	total := 0
	for _, b := range d.Blocks {
		for _, w := range b.Weeks {
			for di, sess := range w.Days {
				if len(sess.Steps) == 0 {
					continue
				}
				total++
				c := b.ctxFor(d.Athlete, w.N)
				sc := *c
				sc.Session = &sess
				sc.InBlock = true
				rs, err := resolveSteps(&sc, sess)
				if err != nil {
					t.Fatalf("week %d day %d: %v", w.N, di+1, err)
				}
				if sess.Dist > 0 {
					if sum := stepsDistance(rs); sum > float64(sess.Dist)*1.02 {
						t.Errorf("week %d day %d: steps measure %s but the session says %s",
							w.N, di+1, Distance(sum).In(d.Athlete.Units), sess.Dist.In(d.Athlete.Units))
					}
				}
			}
		}
	}
	if total == 0 {
		t.Error("no steps days in the live data — if steps were just authored, the field is being dropped on the way in (renamed key? stripping script?)")
	}
}

/* ── the /fit route and its pills ──────────────────────────────────────── */

// fitTestMux is a server over dataDir ("" = the embedded defaults) with the
// routes these tests exercise, registered with the same patterns main uses so
// PathValue works.
func fitTestMux(t *testing.T, dataDir string) *http.ServeMux {
	t.Helper()
	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		t.Fatal(err)
	}
	a, err := newAssets(staticSub)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &server{assets: a, loc: chicago(t), dataDir: dataDir, store: store}
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	tpl, err := template.New("").Funcs(s.makeFuncs()).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	s.tpl = tpl

	mux := http.NewServeMux()
	mux.HandleFunc("GET /calendar", s.calendar)
	mux.HandleFunc("GET /week/{n}", s.week)
	mux.HandleFunc("GET /fit/{date}", s.fitFile)
	mux.HandleFunc("GET /fit.zip", s.fitZip)
	mux.HandleFunc("GET /zwo/{date}", s.zwoFile)
	mux.HandleFunc("GET /zwo.zip", s.zwoZip)
	mux.HandleFunc("GET /watch", s.watchPage)
	mux.HandleFunc("GET /api/activities", s.getActivities)
	mux.HandleFunc("GET /api/activity", s.getActivity)
	mux.HandleFunc("POST /api/activity", s.postActivity)
	mux.HandleFunc("POST /api/entry", s.postEntry)
	mux.HandleFunc("GET /api/issue-trend", s.getIssueTrend)
	return mux
}

func get(mux *http.ServeMux, url string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", url, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// Every miss is a 404 and never a fallback file: a synthesized workout for an
// unstructured day is junk pushed to a watch.
func TestFitRouteMisses(t *testing.T) {
	mux := fitTestMux(t, "")
	for _, c := range []struct{ name, url string }{
		{"unknown block id", "/fit/2026-01-06?block=nope"},
		{"malformed date", "/fit/not-a-date"},
		{"unpadded date", "/fit/2026-1-6"},
		{"date outside the block", "/fit/2030-01-01"},
		{"a rest day", "/fit/2026-01-05"},
		{"a run day without steps", "/fit/2026-01-07"},
	} {
		if rec := get(mux, c.url, nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s: GET %s = %d, want 404", c.name, c.url, rec.Code)
		}
	}
}

func TestFitRouteServesADownload(t *testing.T) {
	mux := fitTestMux(t, "")
	rec := get(mux, "/fit/2026-01-06", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /fit/2026-01-06 = %d, want 200", rec.Code)
	}
	body := rec.Body.Bytes()
	if len(body) < 14 || string(body[8:12]) != ".FIT" {
		t.Errorf("body is %d bytes and lacks the .FIT magic at offset 8", len(body))
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	cd := rec.Header().Get("Content-Disposition")
	if !regexp.MustCompile(`^attachment; filename="W01-Tu-[A-Za-z0-9._-]+\.fit"$`).MatchString(cd) {
		t.Errorf("Content-Disposition = %q, want an attachment with a W01-Tu-….fit filename", cd)
	}

	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on a FIT download — the manifest's caching pattern is the contract")
	}
	again := get(mux, "/fit/2026-01-06", map[string]string{"If-None-Match": etag})
	if again.Code != http.StatusNotModified {
		t.Errorf("If-None-Match replay = %d, want 304", again.Code)
	}
	if again.Body.Len() != 0 {
		t.Errorf("a 304 carried %d body bytes", again.Body.Len())
	}
}

// The pill appears for exactly the steps days, and only there — a download
// link on an unstructured day would 404.
func TestFitPillRendersForExactlyTheStepsDays(t *testing.T) {
	mux := fitTestMux(t, "")
	for _, c := range []struct {
		url   string
		pills int
		hrefs []string
	}{
		{"/week/1", 1, []string{`href="/fit/2026-01-06"`}},
		{"/week/2", 1, []string{`href="/fit/2026-01-13"`}},
		// The calendar is where a week's files get grabbed in advance: a pill
		// per steps day, plus the whole-block zip at the top.
		{"/calendar", 3, []string{`href="/fit/2026-01-06"`, `href="/fit/2026-01-13"`, `href="/fit.zip"`}},
	} {
		rec := get(mux, c.url, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", c.url, rec.Code)
		}
		html := rec.Body.String()
		if n := strings.Count(html, `class="fitbtn"`); n != c.pills {
			t.Errorf("%s renders %d FIT pills, want %d (only the steps days)", c.url, n, c.pills)
		}
		for _, h := range c.hrefs {
			if !strings.Contains(html, h) {
				t.Errorf("%s: no pill with %s", c.url, h)
			}
		}
		if !strings.Contains(html, `href="/watch"`) {
			t.Errorf("%s: the Watch tab is missing from the nav", c.url)
		}
	}
}

// The zip is the same bytes the per-day route serves, bundled under the same
// slug names — asserted byte-for-byte, so the two download paths cannot
// quietly diverge.
func TestFitZipBundlesExactlyTheStepsDays(t *testing.T) {
	mux := fitTestMux(t, "")
	rec := get(mux, "/fit.zip", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /fit.zip = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename="example-base-block-workouts.zip"` {
		t.Errorf("Content-Disposition = %q", cd)
	}

	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	want := map[string]string{
		"W01-Tu-Easy-run-4-x.fit": "/fit/2026-01-06",
		"W02-Tu-3-x-4_00.fit":     "/fit/2026-01-13",
	}
	if len(zr.File) != len(want) {
		t.Fatalf("zip has %d entries, want %d", len(zr.File), len(want))
	}
	for _, f := range zr.File {
		url, ok := want[f.Name]
		if !ok {
			t.Errorf("unexpected entry %q", f.Name)
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		var got bytes.Buffer
		if _, err := got.ReadFrom(rc); err != nil {
			t.Fatal(err)
		}
		rc.Close()
		single := get(mux, url, nil)
		if !bytes.Equal(got.Bytes(), single.Body.Bytes()) {
			t.Errorf("%s differs from %s — the bundle and the single download must be the same bytes", f.Name, url)
		}
	}

	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on the zip")
	}
	if again := get(mux, "/fit.zip", map[string]string{"If-None-Match": etag}); again.Code != http.StatusNotModified {
		t.Errorf("If-None-Match replay = %d, want 304", again.Code)
	}
}

// A zip of nothing is the fallback-file mistake in a different wrapper.
func TestFitZipMisses(t *testing.T) {
	if rec := get(fitTestMux(t, ""), "/fit.zip?block=nope", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown block = %d, want 404", rec.Code)
	}

	// A block whose days carry no steps: the defaults copy, stripped.
	dir := t.TempDir()
	for _, sub := range []string{"blocks", "library"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	copyFile(t, "./defaults/athlete.json", filepath.Join(dir, "athlete.json"))
	for _, n := range []string{"guides.json", "index.json"} {
		copyFile(t, filepath.Join("./defaults/library", n), filepath.Join(dir, "library", n))
	}
	raw, err := os.ReadFile("./defaults/blocks/example-base-block.json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, w := range m["weeks"].([]any) {
		for _, d := range w.(map[string]any)["days"].([]any) {
			delete(d.(map[string]any), "steps")
		}
	}
	out, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, "blocks", "b.json"), out, 0o644); err != nil {
		t.Fatal(err)
	}
	if rec := get(fitTestMux(t, dir), "/fit.zip", nil); rec.Code != http.StatusNotFound {
		t.Errorf("stepless block = %d, want 404 — an empty zip is a fallback file", rec.Code)
	}
}

// The two dialects come from one resolvedSteps, so this checks the .zwo half:
// element shapes, the FTP fractions, and the ramps.
func TestZwoRendersTheSameStepsForZwift(t *testing.T) {
	steps := []resolvedStep{
		{Role: "warmup", Secs: 720, PowerLo: 118, PowerHi: 139},
		{Repeat: 4, Body: []resolvedStep{
			{Role: "active", Secs: 180, PowerLo: 252, PowerHi: 252},
			{Role: "recovery", Secs: 180, PowerLo: 107, PowerHi: 107},
		}},
		{Role: "active", Secs: 300, PowerLo: 0}, // untargeted → FreeRide
		{Role: "cooldown", Secs: 840, PowerLo: 96, PowerHi: 118},
	}
	b, err := zwoFor(`W02 We VO2 4x3' @252 W`, steps, 214)
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	for _, want := range []string{
		`<name>W02 We VO2 4x3&apos; @252 W</name>`,
		`<Warmup Duration="720" PowerLow="0.551" PowerHigh="0.650"/>`,
		`<IntervalsT Repeat="4" OnDuration="180" OffDuration="180" OnPower="1.178" OffPower="0.500"/>`,
		`<FreeRide Duration="300"/>`,
		`<Cooldown Duration="840" PowerLow="0.449" PowerHigh="0.551"/>`,
		`FTP 214 W`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("zwo lacks %s\n%s", want, out)
		}
	}
	if _, err := zwoFor("X", steps, 0); err == nil {
		t.Error("a zero FTP must refuse — the fractions would divide by it")
	}
}

// Bike days serve both dialects; run days only FIT — and the zwo zip carries
// exactly the bike days, well-formed.
func TestZwoRoutesServeBikeDaysOnly(t *testing.T) {
	requireAthleteData(t) // the asserted dates are the real block's bike days
	mux := fitTestMux(t, "./data")
	rec := get(mux, "/zwo/2026-08-12", nil) // week 2 Wednesday, VO2 4x3
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /zwo/2026-08-12 = %d, want 200", rec.Code)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, `.zwo"`) {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if !strings.Contains(rec.Body.String(), "<workout_file>") {
		t.Error("body is not a zwo document")
	}
	for _, c := range []struct{ name, url string }{
		{"a run steps day", "/zwo/2026-09-08"},
		{"an FTP test day left in Zwift", "/zwo/2026-08-05"},
		{"a rest day", "/zwo/2026-08-07"},
	} {
		if rec := get(mux, c.url, nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", c.name, rec.Code)
		}
	}

	zrec := get(mux, "/zwo.zip", nil)
	if zrec.Code != http.StatusOK {
		t.Fatalf("GET /zwo.zip = %d", zrec.Code)
	}
	zr, err := zip.NewReader(bytes.NewReader(zrec.Body.Bytes()), int64(zrec.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) != 38 {
		t.Errorf("zwo.zip has %d entries, want 38 (every bike day, no FTP tests)", len(zr.File))
	}
	for _, f := range zr.File {
		if !strings.HasSuffix(f.Name, ".zwo") {
			t.Errorf("entry %q is not a .zwo", f.Name)
		}
	}
}

// The watch page lists exactly the steps days, with the derived on-watch
// name, the slug the file will land under, and the per-day URL the sender
// fetches.
func TestWatchPageListsStepsDays(t *testing.T) {
	mux := fitTestMux(t, "")
	rec := get(mux, "/watch", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /watch = %d, want 200", rec.Code)
	}
	html := rec.Body.String()
	for _, want := range []string{
		`data-url="/fit/2026-01-06"`, `data-url="/fit/2026-01-13"`,
		`data-slug="W01-Tu-Easy-run-4-x.fit"`, `data-slug="W02-Tu-3-x-4_00.fit"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("/watch lacks %s", want)
		}
	}
	// The sender script must arrive content-hashed — an unhashed path means
	// the embed missed it and the page would 404 its own machinery.
	if !regexp.MustCompile(`/static/webmtp\.[0-9a-f]{12}\.js`).MatchString(html) {
		t.Error("/watch does not load a hashed webmtp.js")
	}
	if n := strings.Count(html, "data-url="); n != 2 {
		t.Errorf("/watch lists %d rows, want 2 — only the steps days", n)
	}
	if rec := get(mux, "/watch?block=nope", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown block = %d, want 404", rec.Code)
	}
}

// An archived block's pills and downloads thread ?block= exactly as the
// week-pager links do, so a past block serves its own plan — and the same
// date without the parameter is somebody else's date range, which is a 404,
// not a fallback.
func TestFitArchivedBlockThreading(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{"blocks", "library"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	copyFile(t, "./defaults/athlete.json", filepath.Join(dir, "athlete.json"))
	for _, n := range []string{"guides.json", "index.json"} {
		copyFile(t, filepath.Join("./defaults/library", n), filepath.Join(dir, "library", n))
	}
	base, err := os.ReadFile("./defaults/blocks/example-base-block.json")
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, id, start string) {
		var m map[string]any
		if err := json.Unmarshal(base, &m); err != nil {
			t.Fatal(err)
		}
		m["id"], m["start"] = id, start
		out, _ := json.Marshal(m)
		if err := os.WriteFile(filepath.Join(dir, "blocks", name), out, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.json", "old-block", "2026-01-05")
	write("b.json", "new-block", "2026-06-01") // later, so old-block is archived

	mux := fitTestMux(t, dir)
	rec := get(mux, "/week/1?block=old-block", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("archived week = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `href="/fit/2026-01-06?block=old-block"`) {
		t.Error("the archived week's pill does not thread ?block=")
	}
	if rec := get(mux, "/calendar?block=old-block", nil); !strings.Contains(rec.Body.String(), `href="/fit/2026-01-06?block=old-block"`) {
		t.Error("the archived calendar's pill does not thread ?block=")
	} else if !strings.Contains(rec.Body.String(), `href="/fit.zip?block=old-block"`) {
		t.Error("the archived calendar's zip pill does not thread ?block=")
	}
	if rec := get(mux, "/fit.zip?block=old-block", nil); rec.Code != http.StatusOK {
		t.Errorf("archived zip = %d, want 200", rec.Code)
	}
	if rec := get(mux, "/fit/2026-01-06?block=old-block", nil); rec.Code != http.StatusOK {
		t.Errorf("archived download = %d, want 200 — re-running a past session is legitimate", rec.Code)
	}
	if rec := get(mux, "/fit/2026-01-06", nil); rec.Code != http.StatusNotFound {
		t.Errorf("the same date against the current block = %d, want 404 — no silent fallback across blocks", rec.Code)
	}
}

/* ── the activity store ────────────────────────────────────────────────── */

// The trend endpoint serves the declared scale, the bands in order, and one
// point per rated day with the last write winning — against whatever issue
// the dataset declares, so a hardcoded key or scale would fail on defaults.
func TestIssueTrendServesScaleBandsAndHistory(t *testing.T) {
	dir := t.TempDir() // embedded defaults: a declared issue on its own scale
	mux := fitTestMux(t, dir)
	d, err := loadDataset(dir, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Athlete.Issues) == 0 {
		t.Fatal("the defaults declare no issue; this test needs one")
	}
	is := d.Athlete.Issues[0]
	lo, hi := is.Scale.Min, is.Scale.Min+1
	entry := func(date string, val int, note string) []byte {
		b, _ := json.Marshal(map[string]any{
			"date": date, "kind": "issue", "key": is.Key,
			"val": strconv.Itoa(val), "note": note,
		})
		return b
	}
	for _, e := range [][]byte{
		entry("2026-01-05", lo, ""),
		entry("2026-01-06", hi, "stiff"),
		entry("2026-01-06", lo, "walked it off"), // re-rate: last wins
	} {
		if rec := post(mux, "/api/entry", e); rec.Code != http.StatusNoContent {
			t.Fatalf("seed entry = %d: %s", rec.Code, rec.Body.String())
		}
	}

	rec := get(mux, "/api/issue-trend?key="+is.Key, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("trend = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Name     string `json:"name"`
		Min, Max int
		Bands    []struct {
			UpTo  *int   `json:"upto"`
			Tone  string `json:"tone"`
			Label string `json:"label"`
		}
		Points []struct {
			Date string `json:"date"`
			Val  int    `json:"val"`
			Note string `json:"note"`
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Name != is.Name || out.Min != is.Scale.Min || out.Max != is.Scale.Max {
		t.Errorf("declaration = %q %d–%d, want %q %d–%d", out.Name, out.Min, out.Max, is.Name, is.Scale.Min, is.Scale.Max)
	}
	if len(out.Bands) != len(is.Bands) || out.Bands[0].Label != is.Bands[0].Label {
		t.Errorf("bands = %+v, want the %d declared", out.Bands, len(is.Bands))
	}
	if last := out.Bands[len(out.Bands)-1]; last.UpTo != nil {
		t.Error("the last band carries an upto; it must catch the top")
	}
	want := []struct {
		date string
		val  int
	}{{"2026-01-05", lo}, {"2026-01-06", lo}}
	if len(out.Points) != len(want) {
		t.Fatalf("points = %+v, want %d (one per day, last write wins)", out.Points, len(want))
	}
	for i, w := range want {
		if out.Points[i].Date != w.date || out.Points[i].Val != w.val {
			t.Errorf("point %d = %+v, want %v", i, out.Points[i], w)
		}
	}
	if out.Points[1].Note != "walked it off" {
		t.Errorf("re-rated note = %q, want the last write's", out.Points[1].Note)
	}

	if rec := get(mux, "/api/issue-trend?key=no-such-issue", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown key = %d, want 404", rec.Code)
	}
}

func post(mux *http.ServeMux, url string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", url, bytes.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// fitBytes is a body that passes the magic check: eight header bytes, ".FIT"
// at 8–11, then whatever payload distinguishes one fake recording from
// another. The server never decodes past the magic, so this is enough.
func fitBytes(payload string) []byte {
	return append([]byte("\x0e\x10\x43\x08\x00\x00\x00\x00.FIT"), payload...)
}

// Activities are health data beside the plan, not part of it: the data Rev
// and the fingerprint must both be byte-identical before and after the
// activities directory appears, and the load must still succeed — a data
// error at startup crash-loops the container, so this is the crash-loop
// guard too.
func TestActivityStoreIsInvisibleToTheDataRev(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{"blocks", "library"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	copyFile(t, "./data/athlete.json", filepath.Join(dir, "athlete.json"))
	for _, n := range []string{"movements.json", "sessions.json", "tasks.json", "index.json"} {
		copyFile(t, filepath.Join("./data/library", n), filepath.Join(dir, "library", n))
	}
	copyFile(t, "./data/blocks/2026-08-16-week-build.json", filepath.Join(dir, "blocks", "b.json"))

	before, err := loadDataset(dir, time.UTC)
	if err != nil {
		t.Fatalf("load without activities: %v", err)
	}
	fpBefore := fingerprint(dir)

	adir := filepath.Join(dir, "activities")
	if err := os.MkdirAll(adir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{
		"2026-08-12-07-00-00.fit", "2026-08-13-06-30-00.fit",
		"2026-08-13-06-30-00.fit.123.tmp", // a write stranded mid-upload
	} {
		if err := os.WriteFile(filepath.Join(adir, n), fitBytes(n), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	after, err := loadDataset(dir, time.UTC)
	if err != nil {
		t.Fatalf("the activities directory broke the load: %v", err)
	}
	if after.Rev != before.Rev {
		t.Errorf("data Rev moved: %s -> %s — stored activities must be invisible to it", before.Rev, after.Rev)
	}
	if fp := fingerprint(dir); fp != fpBefore {
		t.Errorf("fingerprint moved: %s -> %s — the reload poller would churn on every upload", fpBefore, fp)
	}
}

// Every refusal leaves the store untouched — asserted collectively at the
// end, where the directory must not even exist, since nothing may be created
// before a request has fully qualified.
func TestActivityPostRefusals(t *testing.T) {
	dir := t.TempDir()
	mux := fitTestMux(t, dir)
	ok := fitBytes("x")

	oversize := make([]byte, activityMaxBytes+1)
	copy(oversize[8:], ".FIT")

	for _, c := range []struct {
		name  string
		query string
		body  []byte
		want  int
	}{
		{"missing name", "", ok, http.StatusBadRequest},
		{"path escape", "../x.fit", ok, http.StatusBadRequest},
		{"path separator", "a/b.fit", ok, http.StatusBadRequest},
		{"hidden file", ".hidden.fit", ok, http.StatusBadRequest},
		{"AppleDouble", "._junk.fit", ok, http.StatusBadRequest},
		{"wrong extension", "x.txt", ok, http.StatusBadRequest},
		{"tmp suffix", "x.fit.tmp", ok, http.StatusBadRequest},
		{"over-long name", strings.Repeat("a", 99) + ".fit", ok, http.StatusBadRequest},
		{"no magic", "x.fit", []byte("0123456789abcdef"), http.StatusBadRequest},
		{"too short for magic", "x.fit", []byte(".FIT"), http.StatusBadRequest},
		{"oversized body", "x.fit", oversize, http.StatusRequestEntityTooLarge},
	} {
		if rec := post(mux, "/api/activity?name="+c.query, c.body); rec.Code != c.want {
			t.Errorf("%s: POST = %d, want %d", c.name, rec.Code, c.want)
		}
	}

	if ents, err := os.ReadDir(filepath.Join(dir, "activities")); err == nil {
		if len(ents) != 0 {
			t.Errorf("refusals left %d entries in the store", len(ents))
		}
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestActivityRoundTrip(t *testing.T) {
	dir := t.TempDir()
	mux := fitTestMux(t, dir)

	// An empty store — the directory does not even exist yet — is [], never
	// null: the sender iterates the reply without a guard.
	rec := get(mux, "/api/activities", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/activities = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Fatalf("empty store lists as %q, want []", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("list Content-Type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("list Cache-Control = %q — health data must not be cached", cc)
	}

	const name = "2026-08-12-07-00-00.fit"
	body := fitBytes("a real recording")
	if rec := post(mux, "/api/activity?name="+name, body); rec.Code != http.StatusNoContent {
		t.Fatalf("POST = %d, want 204: %s", rec.Code, rec.Body.String())
	}
	stored, err := os.ReadFile(filepath.Join(dir, "activities", name))
	if err != nil {
		t.Fatalf("stored file: %v", err)
	}
	if !bytes.Equal(stored, body) {
		t.Error("stored bytes differ from the upload")
	}
	if ents, _ := os.ReadDir(filepath.Join(dir, "activities")); len(ents) != 1 {
		t.Errorf("store holds %d entries, want 1 — a temp file survived the publish", len(ents))
	}

	rec = get(mux, "/api/activity?name="+name, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), body) {
		t.Error("download differs from the upload")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename="`+name+`"` {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("download Cache-Control = %q — health data must not be cached", cc)
	}

	if rec := get(mux, "/api/activity?name=2026-01-01-00-00-00.fit", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown name = %d, want 404", rec.Code)
	}
	if rec := get(mux, "/api/activity?name=../x.fit", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid name = %d, want 400", rec.Code)
	}

	// A stranded temp, a junk-named file and a subdirectory are not stored
	// activities: the listing admits only names the store itself would take.
	for _, junk := range []string{name + ".123.tmp", "._junk.fit"} {
		if err := os.WriteFile(filepath.Join(dir, "activities", junk), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "activities", "sub.fit"), 0o755); err != nil {
		t.Fatal(err)
	}

	rec = get(mux, "/api/activities", nil)
	var list []activityInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].Name != name || list[0].Size != int64(len(body)) {
		t.Errorf("list = %+v, want one entry %s of %d bytes", list, name, len(body))
	}
}

// A recording is never overwritten: the second POST under a taken name is a
// 409 and the first bytes stay.
func TestActivityRepostConflicts(t *testing.T) {
	dir := t.TempDir()
	mux := fitTestMux(t, dir)
	const name = "2026-08-12-07-00-00.fit"
	first := fitBytes("the original recording")
	if rec := post(mux, "/api/activity?name="+name, first); rec.Code != http.StatusNoContent {
		t.Fatalf("first POST = %d", rec.Code)
	}
	if rec := post(mux, "/api/activity?name="+name, fitBytes("an impostor")); rec.Code != http.StatusConflict {
		t.Errorf("second POST = %d, want 409", rec.Code)
	}
	stored, err := os.ReadFile(filepath.Join(dir, "activities", name))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, first) {
		t.Error("the conflicting POST replaced the stored bytes")
	}
}

// The publish itself refuses a taken name, not just the handler's stat fast
// path: os.Rename here would pass the sequential 409 test and still lose a
// recording to a concurrent duplicate, so the hard link's refusal is pinned
// directly.
func TestActivityPublishNeverReplaces(t *testing.T) {
	dir := t.TempDir()
	orig := []byte("the original recording")
	if err := os.WriteFile(filepath.Join(dir, "a.fit"), orig, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := publishActivity(dir, "a.fit", []byte("an impostor")); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("publish onto a taken name: err = %v, want fs.ErrExist", err)
	}
	stored, err := os.ReadFile(filepath.Join(dir, "a.fit"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, orig) {
		t.Error("publish replaced the stored bytes")
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Errorf("dir holds %d entries, want 1 — the refused temp survived", len(ents))
	}
}

// Device names are timestamps, so descending name order is newest first.
// os.ReadDir already returns ascending, which is what an unsorted handler
// would leak — the uploads arrive oldest-first here to prove the sort is real.
func TestActivitiesListNewestFirst(t *testing.T) {
	dir := t.TempDir()
	mux := fitTestMux(t, dir)
	older, newer := "2026-08-01-06-00-00.fit", "2026-08-12-07-00-00.fit"
	for _, n := range []string{older, newer} {
		if rec := post(mux, "/api/activity?name="+n, fitBytes(n)); rec.Code != http.StatusNoContent {
			t.Fatalf("POST %s = %d", n, rec.Code)
		}
	}
	var list []activityInfo
	if err := json.Unmarshal(get(mux, "/api/activities", nil).Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != newer || list[1].Name != older {
		t.Errorf("list order = %+v, want %s before %s", list, newer, older)
	}
}

// A stepless block still gets the watch page — pulling recordings off the
// watch needs no rows to send — while an unknown block id stays a 404.
func TestWatchPageRendersForSteplessBlock(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{"blocks", "library"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	copyFile(t, "./defaults/athlete.json", filepath.Join(dir, "athlete.json"))
	for _, n := range []string{"guides.json", "index.json"} {
		copyFile(t, filepath.Join("./defaults/library", n), filepath.Join(dir, "library", n))
	}
	raw, err := os.ReadFile("./defaults/blocks/example-base-block.json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, w := range m["weeks"].([]any) {
		for _, d := range w.(map[string]any)["days"].([]any) {
			delete(d.(map[string]any), "steps")
		}
	}
	out, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, "blocks", "b.json"), out, 0o644); err != nil {
		t.Fatal(err)
	}

	mux := fitTestMux(t, dir)
	if rec := get(mux, "/watch", nil); rec.Code != http.StatusOK {
		t.Errorf("stepless block = %d, want 200 — the page pulls as well as sends", rec.Code)
	}
	if rec := get(mux, "/watch?block=nope", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown block = %d, want 404", rec.Code)
	}
}
