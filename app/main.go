package main

import (
	"archive/zip"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// buildHash is stamped at link time from the Makefile's SRC_HASH so the running
// app can report exactly which build it is. Since the plan now lives on the
// volume rather than in the binary, /healthz reports a data revision alongside
// it -- the two move independently, and a stale one of either is a real bug.
var buildHash = "dev"

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// reloadEvery is how often the data directory is restatted. Claude drops a new
// block over ssh and it is live within this, with no image build or restart.
const reloadEvery = 30 * time.Second

type server struct {
	data    atomic.Pointer[dataset]
	stamp   atomic.Pointer[string]
	store   *Store
	metrics *metricsDB
	weather *weatherService
	grader  *grader
	tpl     *template.Template
	loc     *time.Location // fallback timezone from the flag
	assets  *assets
	dataDir string
}

func main() {
	addr := flag.String("addr", envOr("ADDR", ":8080"), "listen address")
	dataDir := flag.String("data", envOr("DATA_DIR", "./data"), "writable data directory")
	tz := flag.String("tz", envOr("TZ", "America/Chicago"), "fallback timezone when the athlete declares none")
	check := flag.Bool("validate", false, "load and validate the data, then exit")
	flag.Parse()

	loc, err := time.LoadLocation(*tz)
	if err != nil {
		log.Printf("unknown timezone %q, falling back to UTC: %v", *tz, err)
		loc = time.UTC
	}

	if *check {
		d, err := loadDataset(*dataDir, loc)
		if err != nil {
			log.Fatalf("FAILED %s: %v", *dataDir, err)
		}
		log.Printf("ok  rev=%s  athlete=%s  units=%s  blocks=%d  guides=%d",
			d.Rev, d.Athlete.Name, d.Athlete.Units, len(d.Blocks), len(d.Library))
		for _, b := range d.Blocks {
			log.Printf("    %-28s %s → %s  %d weeks", b.ID, b.Start,
				b.EndDate().Format("2006-01-02"), b.WeekCount())
		}
		return
	}

	store, err := OpenStore(*dataDir)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatalf("static: %v", err)
	}
	assetSet, err := newAssets(staticSub)
	if err != nil {
		log.Fatalf("assets: %v", err)
	}

	s := &server{store: store, loc: loc, assets: assetSet, dataDir: *dataDir}
	if err := s.reload(); err != nil {
		log.Fatalf("data: %v", err)
	}
	// The metrics DB is a derived cache over the activity archive; openMetricsDB
	// self-heals a corrupt file, so an error here is environmental (permissions,
	// disk) and as fatal as the store's would be. The reconcile back-fills any
	// archived file the DB does not hold — on a fresh or version-bumped DB that
	// is the whole archive, so it runs off the request path.
	s.metrics, err = openMetricsDB(*dataDir)
	if err != nil {
		log.Fatalf("metrics: %v", err)
	}
	// A typo'd GRADER_* is fatal, not silently off: a grader that fails to
	// arm is visible in ten seconds, one that quietly never runs costs a
	// week of dry-mode comparison.
	gcfg, err := graderConfigFromEnv()
	if err != nil {
		log.Fatalf("grader: %v", err)
	}
	if gcfg.Mode != "off" {
		s.grader = newGrader(s, gcfg)
		log.Printf("grader: armed  mode=%s dialect=%s model=%s base=%s",
			gcfg.Mode, gcfg.Dialect, gcfg.Model, gcfg.BaseURL)
	}
	// Weather is a lookup the grading stack makes and the recording stack
	// does not; off unless asked for.
	s.weather = newWeatherService(s.metrics)
	if s.weather.enabled {
		log.Printf("weather:  lookups on (open-meteo, coarse position)")
	}
	go s.startupReconcile()
	tpl, err := template.New("").Funcs(s.makeFuncs()).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		log.Fatalf("templates: %v", err)
	}
	s.tpl = tpl

	srv := &http.Server{
		Addr:              *addr,
		Handler:           logging(s.routes()),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	ctx, stopWatch := context.WithCancel(context.Background())
	go s.watch(ctx)

	go func() {
		d := s.ds()
		log.Printf("listening on %s  (build=%s data=%s rev=%s tz=%s blocks=%d)",
			*addr, buildHash, *dataDir, d.Rev, d.Loc, len(d.Blocks))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	stopWatch()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	s.metrics.close()
	log.Print("stopped")
}

// routes is the whole HTTP surface, in one place. It is a method rather than
// part of main() so the tests mount the same table the binary serves: a test
// mux assembled by hand is a second declaration of this, and the copy had
// already drifted nine routes before anyone noticed — which is the failure the
// app's own data files are arranged to prevent.
func (s *server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", s.assets.handler())
	mux.HandleFunc("GET /{$}", s.today)
	mux.HandleFunc("GET /calendar", s.calendar)
	mux.HandleFunc("GET /week/{n}", s.week)
	mux.HandleFunc("GET /workouts", s.workouts)
	mux.HandleFunc("GET /fit/{date}", s.fitFile)
	mux.HandleFunc("GET /fit.zip", s.fitZip)
	mux.HandleFunc("GET /zwo/{date}", s.zwoFile)
	mux.HandleFunc("GET /zwo.zip", s.zwoZip)
	mux.HandleFunc("GET /watch", s.watchPage)
	mux.HandleFunc("GET /blocks", s.blocks)
	mux.HandleFunc("GET /manifest.webmanifest", s.manifest)
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("POST /api/entry", s.postEntry)
	mux.HandleFunc("POST /api/reload", s.postReload)
	mux.HandleFunc("GET /api/entries", s.getEntries)
	mux.HandleFunc("GET /api/guides", s.getGuides)
	mux.HandleFunc("GET /api/tasks", s.getTasks)
	mux.HandleFunc("GET /api/activities", s.getActivities)
	mux.HandleFunc("GET /api/activity", s.getActivity)
	mux.HandleFunc("POST /api/activity", s.postActivity)
	mux.HandleFunc("GET /api/activity-metrics", s.getActivityMetrics)
	mux.HandleFunc("GET /api/day", s.getDay)
	mux.HandleFunc("GET /api/session-history", s.getSessionHistory)
	mux.HandleFunc("GET /api/issue-trend", s.getIssueTrend)
	return mux
}

/* ── data lifecycle ────────────────────────────────────────────────────── */

func (s *server) ds() *dataset { return s.data.Load() }

// startupReconcile back-fills the metrics cache from the archive, then
// retries recent ungraded days. Metrics first, then grades: a grade that
// failed after its import succeeded retries here on the next startup, per
// the idempotency contract. Runs off the request path — on a fresh or
// version-bumped DB this is the whole archive.
func (s *server) startupReconcile() {
	s.metrics.reconcile(s.activitiesDir(), s.ds().Loc, s.weather)
	if s.grader != nil {
		s.grader.reconcile()
	}
}

// reload swaps in a fresh dataset. A failed load leaves the old one serving:
// half-applied data would be worse than slightly stale data, and the error is
// logged loudly enough to find.
func (s *server) reload() error {
	d, err := loadDataset(s.dataDir, s.loc)
	if err != nil {
		return err
	}
	stamp := fingerprint(s.dataDir)
	s.data.Store(d)
	s.stamp.Store(&stamp)
	return nil
}

func (s *server) watch(ctx context.Context) {
	t := time.NewTicker(reloadEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cur := fingerprint(s.dataDir)
			if prev := s.stamp.Load(); prev != nil && *prev == cur {
				continue
			}
			old := s.ds().Rev
			if err := s.reload(); err != nil {
				log.Printf("reload FAILED, still serving rev=%s: %v", old, err)
				// Remember the bad stamp so a broken file is reported once,
				// not every thirty seconds until someone fixes it.
				s.stamp.Store(&cur)
				continue
			}
			log.Printf("reloaded  rev %s → %s", old, s.ds().Rev)
		}
	}
}

// day is the calendar day in the athlete's timezone, midnight-anchored.
func (s *server) day(d *dataset) time.Time {
	now := time.Now().In(d.Loc)
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, d.Loc)
}

func (s *server) units() Units {
	if d := s.ds(); d != nil && d.Athlete != nil && d.Athlete.Units.valid() {
		return d.Athlete.Units
	}
	return Imperial
}

/* ── pages ─────────────────────────────────────────────────────────────── */

type todayData struct {
	Nav         string
	Title       string
	Today       time.Time
	InBlock     bool
	DaysToStart int
	Finished    bool // the current block has ended and nothing replaces it yet
	BlockName   string
	BlockSpan   string
	Week        *Week
	MesoName    string
	DayIdx      int
	Session     Session
	Skippable   bool // an actual session, so there is something to skip
	Skipped     bool
	SkipNote    string
	Detail      string
	Targets     []string
	GuideID     string
	FitURL      string // set only when the session carries steps
	ZwoURL      string // bike steps days only: the same session for Zwift
	PhaseName   string
	PhaseDetail string
	Tasks       []taskView
	Issues      []issueView
	Next        []upcoming
}

// issueView is one issue's card: the scale to tap, what today's rating means,
// and where its rehab has got to. Everything here is declared in athlete.json
// except the rating itself, which comes from the log.
type issueView struct {
	Key     string
	Heading string
	Ask     string
	Hint    string
	Range   string
	Guide   string
	Dots    []dotView
	Today   int // below Scale.Min when nothing is logged today
	Rated   bool
	Note    string
	Band    *Band
	Last    *Entry
	Phase   string // the rehab stage this week, if the block declares one
	Detail  string
}

// dotView is one button on the scale, carrying what the server says that value
// means. The meaning travels with the button so tapping it can update the card
// immediately -- otherwise the prescription only appears on the next refresh,
// and worse, re-rating across a band boundary leaves the old instruction
// sitting under the new number. Sending the resolved band per value rather than
// the band list keeps BandFor in one language: the browser looks up, it does
// not decide.
type dotView struct {
	Value  int
	Tone   string
	Label  string
	Action string
}

// upcoming is a row in the "coming up" card. The next ordinary session and the
// next dated benchmark answer different questions -- what am I doing tomorrow,
// versus what do I need to be ready for -- so both are shown, and collapsed to
// one row when they are the same day.
type upcoming struct {
	When    string
	Label   string
	Guide   string
	In      int
	IsBench bool
}

type taskView struct {
	Key      string
	Label    string
	Done     bool
	Guide    string
	SubDone  int
	SubTotal int
}

func (s *server) today(w http.ResponseWriter, r *http.Request) {
	d := s.ds()
	day := s.day(d)
	iso := day.Format("2006-01-02")

	blk := d.Current(day)
	if blk == nil {
		http.Error(w, "no training block loaded", http.StatusInternalServerError)
		return
	}

	td := todayData{Nav: "today", Title: "Today", Today: day}
	wk, di, ok := blk.Locate(day)
	td.InBlock, td.Week, td.DayIdx = ok, wk, di

	td.BlockName, td.BlockSpan = blk.Name, blk.Span()
	weekN := 1
	if ok {
		weekN = wk.N
	} else {
		td.DaysToStart = blk.DaysUntilStart(day)
		// Past the end with nothing to move on to. This is the state that used
		// to read "Pre-block" underneath "Block complete".
		td.Finished = td.DaysToStart <= 0
		if td.Finished {
			weekN = blk.WeekCount()
		}
	}

	guides, err := blk.ResolveGuides(d.Athlete, weekN)
	if err != nil {
		log.Printf("guides: %v", err)
		http.Error(w, "guides unavailable", http.StatusInternalServerError)
		return
	}
	ctx := blk.ctxFor(d.Athlete, weekN)
	ctx.InBlock = ok

	if ok {
		td.Session = wk.Days[di]
		ctx.Session = &td.Session
		if td.Targets, err = ctx.resolveAll(blk.TargetsFor(td.Session)); err != nil {
			log.Printf("targets: %v", err)
		}
		if td.Detail, err = ctx.resolve(td.Session.Detail); err != nil {
			log.Printf("detail: %v", err)
		}
		td.GuideID = td.Session.GuideID()
		if len(td.Session.Steps) > 0 {
			td.FitURL = "/fit/" + iso
			if td.Session.Kind.IsBike() {
				td.ZwoURL = "/zwo/" + iso
			}
		}
		if p := wk.Phase(); p != nil {
			td.PhaseName, td.PhaseDetail = p.Name, p.Detail
		}
		if m := wk.Mesocycle(); m != nil {
			td.MesoName = m.Name
		}
		td.Next = comingUp(blk, guides, day)
	}

	done := s.store.TasksFor(iso)
	for _, t := range checklist(ctx, blk) {
		tv := taskView{Key: t.key, Label: t.label, Done: done[t.key], Guide: t.guide}
		if g, ok := guides[t.guide]; ok && len(g.Steps) > 0 {
			units := ProgressUnits(g)
			tv.SubTotal = len(units)
			for _, k := range units {
				if done[k] {
					tv.SubDone++
				}
			}
		}
		td.Tasks = append(td.Tasks, tv)
	}

	// A session can be marked not-done. A rest day cannot: there is nothing
	// to skip, and offering it would be noise on the one day that needs none.
	td.Skippable = ok && td.Session.Kind != KindRest
	if e, skipped := s.store.SkipOn(iso); skipped {
		td.Skipped, td.SkipNote = true, e.Note
	}

	td.Issues = s.issueViews(d, blk, weekN, iso)
	s.render(w, "today.html", td)
}

// issueViews builds a card per declared issue. An athlete with none simply
// gets no cards, which is what makes the app usable by someone who is not
// carrying an injury at all.
func (s *server) issueViews(d *dataset, blk *Block, week int, iso string) []issueView {
	var out []issueView
	for i := range d.Athlete.Issues {
		is := &d.Athlete.Issues[i]
		v := issueView{
			Key: is.Key, Heading: is.Heading(), Ask: is.Ask, Hint: is.Hint,
			Range: is.Range(), Guide: is.Guide,
			Today: is.Scale.Min - 1,
		}
		for _, n := range is.Scale.Values() {
			dot := dotView{Value: n}
			if b := is.BandFor(n); b != nil {
				dot.Tone, dot.Label, dot.Action = b.Tone, b.Label, b.Action
			}
			v.Dots = append(v.Dots, dot)
		}

		if hist := s.store.Ratings(is.Key); len(hist) > 0 {
			v.Last = &hist[len(hist)-1]
		}
		if e := s.store.RatingOn(is.Key, iso); e != nil {
			if n, err := strconv.Atoi(strings.TrimSpace(e.Val)); err == nil && is.Scale.contains(n) {
				v.Today, v.Rated, v.Note, v.Band = n, true, e.Note, is.BandFor(n)
			}
		}
		if p := blk.PhaseForIssue(is.Key, week); p != nil {
			v.Phase, v.Detail = p.Name, p.Detail
		}
		out = append(out, v)
	}
	return out
}

type calendarData struct {
	Nav       string
	Title     string
	Intro     string
	Archive   *archiveBanner
	Rows      []calRow
	TodayW    int
	TodayD    int
	Headings  []string
	Grading   *Grading
	FitZipURL string // set only when the block has at least one steps day
	ZwoZipURL string // set only when the block has at least one bike steps day
}

// archiveBanner appears on any page showing a block that is not the one being
// trained. Without it, a past block's calendar is indistinguishable from the
// live one — same layout, same grades, wrong plan.
type archiveBanner struct {
	Name  string
	Span  string
	Query string // "?block=<id>", to thread through week links
}

// archiveOf returns nil when blk is the current block, so the banner and the
// ?block= query string both disappear on the normal path.
func (s *server) archiveOf(d *dataset, blk *Block, day time.Time) *archiveBanner {
	if d.IsCurrent(blk, day) {
		return nil
	}
	return &archiveBanner{Name: blk.Name, Span: blk.Span(), Query: "?block=" + url.QueryEscape(blk.ID)}
}

type calRow struct {
	Week      *Week
	Volume    string
	Cells     []calCell
	WeekGrade string
	WeekNote  string
	WeekRange string // the legend's range for that letter, looked up not restated
}

type calCell struct {
	Session Session
	Date    time.Time
	Guide   string
	Grade   string
	Note    string
	Range   string // the legend's range for Grade, so the popup can say what the letter meant
	Skipped bool
	FitURL  string // set only when the session carries steps
	ZwoURL  string // bike steps days only
}

func (s *server) calendar(w http.ResponseWriter, r *http.Request) {
	d := s.ds()
	day := s.day(d)
	blk, ok := d.blockFor(r.URL.Query().Get("block"), day)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := blk.ctxFor(d.Athlete, 1)

	cd := calendarData{Nav: "calendar", Title: "The whole block",
		Archive:  s.archiveOf(d, blk, day),
		Headings: []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}}
	cd.Intro, _ = ctx.resolve(blk.Intro)
	cd.Grading = resolveGrading(ctx, blk.Grading)

	if wk, di, ok := blk.Locate(day); ok {
		cd.TodayW, cd.TodayD = wk.N, di
	}
	// The legend is the one declaration of what a letter means; this is a
	// lookup into it, never a second copy.
	bandOf := map[string]string{}
	if cd.Grading != nil {
		for _, b := range cd.Grading.Bands {
			bandOf[b.Grade] = b.Range
		}
	}
	grades, weekGrades := s.store.Grades(), s.store.WeekGrades()
	skips := s.store.Skips()
	for wi, wk := range blk.Weeks {
		row := calRow{Week: wk, Volume: wk.Volume().In(s.units())}
		if g, ok := weekGrades[wk.StartISO()]; ok {
			row.WeekGrade, row.WeekNote = g.Val, g.Note
			row.WeekRange = bandOf[g.Val]
		}
		for i, sess := range wk.Days {
			dt := blk.DayOf(wi, i)
			c := calCell{Session: sess, Date: dt, Guide: sess.GuideID()}
			if len(sess.Steps) > 0 {
				c.FitURL = "/fit/" + dt.Format("2006-01-02")
				if cd.Archive != nil {
					c.FitURL += cd.Archive.Query
				}
				if sess.Kind.IsBike() {
					c.ZwoURL = "/zwo/" + dt.Format("2006-01-02")
					if cd.Archive != nil {
						c.ZwoURL += cd.Archive.Query
					}
					if cd.ZwoZipURL == "" {
						cd.ZwoZipURL = "/zwo.zip"
						if cd.Archive != nil {
							cd.ZwoZipURL += cd.Archive.Query
						}
					}
				}
				if cd.FitZipURL == "" {
					cd.FitZipURL = "/fit.zip"
					if cd.Archive != nil {
						cd.FitZipURL += cd.Archive.Query
					}
				}
			}
			if g, ok := grades[dt.Format("2006-01-02")]; ok {
				c.Grade, c.Note = g.Val, g.Note
				c.Range = bandOf[g.Val]
			}
			// A day he said he did not do reads differently from a blank
			// one, and the reason he gave is what the cell says on hover.
			if sk, ok := skips[dt.Format("2006-01-02")]; ok {
				c.Skipped = true
				if c.Note == "" {
					c.Note = strings.TrimSpace("Skipped — " + sk.Note)
				}
			}
			row.Cells = append(row.Cells, c)
		}
		cd.Rows = append(cd.Rows, row)
	}
	s.render(w, "calendar.html", cd)
}

// resolveGrading renders the calendar legend. It is structured rather than a
// blob of markup so the grade colours stay in the stylesheet.
func resolveGrading(c *Ctx, g *Grading) *Grading {
	if g == nil {
		return nil
	}
	out := *g
	out.Note, _ = c.resolve(g.Note)
	out.Footer, _ = c.resolve(g.Footer)
	out.Bands = make([]GradeBand, len(g.Bands))
	for i, b := range g.Bands {
		b.Range, _ = c.resolve(b.Range)
		out.Bands[i] = b
	}
	return &out
}

type weekData struct {
	Nav        string
	Title      string
	Week       *Week
	MesoName   string
	VolumeLong string
	Archive    *archiveBanner
	Days       []dayView
	Prev       int
	Next       int
	Headings   []string
}

type dayView struct {
	Name    string
	Date    time.Time
	Session Session
	Detail  string
	Targets []string
	GuideID string
	FitURL  string // set only when the session carries steps
	ZwoURL  string // bike steps days only
}

func (s *server) week(w http.ResponseWriter, r *http.Request) {
	d := s.ds()
	day := s.day(d)
	blk, ok := d.blockFor(r.URL.Query().Get("block"), day)
	if !ok {
		http.NotFound(w, r)
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 || n > blk.WeekCount() {
		http.NotFound(w, r)
		return
	}
	wk := blk.WeekByN(n)
	ctx := blk.ctxFor(d.Athlete, n)

	wd := weekData{Nav: "calendar", Title: "Week " + strconv.Itoa(n), Week: wk,
		Archive:    s.archiveOf(d, blk, day),
		VolumeLong: wk.Volume().InLong(s.units()),
		Headings:   []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}}
	if m := wk.Mesocycle(); m != nil {
		wd.MesoName = m.Name
	}
	for i, sess := range wk.Days {
		sc := ctx.forSession(&sess)
		targets, err := sc.resolveAll(blk.TargetsFor(sess))
		if err != nil {
			log.Printf("week %d day %d targets: %v", n, i+1, err)
		}
		detail, err := sc.resolve(sess.Detail)
		if err != nil {
			log.Printf("week %d day %d detail: %v", n, i+1, err)
		}
		dv := dayView{
			Name:    wd.Headings[i],
			Date:    blk.DayOf(n-1, i),
			Session: sess,
			Detail:  detail,
			Targets: targets,
			GuideID: sess.GuideID(),
		}
		if len(sess.Steps) > 0 {
			// Threaded through ?block= exactly as the week-pager links are,
			// so an archived block's download serves that block's plan.
			dv.FitURL = "/fit/" + dv.Date.Format("2006-01-02")
			if wd.Archive != nil {
				dv.FitURL += wd.Archive.Query
			}
			if sess.Kind.IsBike() {
				dv.ZwoURL = "/zwo/" + dv.Date.Format("2006-01-02")
				if wd.Archive != nil {
					dv.ZwoURL += wd.Archive.Query
				}
			}
		}
		wd.Days = append(wd.Days, dv)
	}
	if n > 1 {
		wd.Prev = n - 1
	}
	if n < blk.WeekCount() {
		wd.Next = n + 1
	}
	s.render(w, "week.html", wd)
}

type wkRow struct {
	Guide  string
	Name   string
	Detail string
	When   string
}

type wkGroup struct {
	Title string
	Note  string
	Rows  []wkRow
}

type workoutsData struct {
	Nav     string
	Title   string
	Archive *archiveBanner
	Groups  []wkGroup
	Count   int
}

type blocksData struct {
	Nav   string
	Title string
	Rows  []blockRow
}

type blockRow struct {
	ID       string
	Name     string
	Span     string
	Goal     string
	Weeks    int
	Volume   string
	Status   string // "current" or "archived"
	Detail   string // what the status means in dates
	Query    string // "" for the current block, "?block=<id>" otherwise
	Grades   map[string]int
	Sessions int
}

// blocks is the archive index. Grades come from the log by date, so a finished
// block's record assembles itself with no per-block storage at all.
func (s *server) blocks(w http.ResponseWriter, r *http.Request) {
	d := s.ds()
	day := s.day(d)
	grades := s.store.Grades()

	bd := blocksData{Nav: "blocks", Title: "Blocks"}
	for i := len(d.Blocks) - 1; i >= 0; i-- { // newest first
		b := d.Blocks[i]
		row := blockRow{
			ID: b.ID, Name: b.Name, Span: b.Span(),
			Weeks:  b.WeekCount(),
			Volume: b.Volume().InLong(s.units()),
			Grades: map[string]int{},
		}
		if b.Goal.Event != "" {
			row.Goal = b.Goal.Event
			if b.Goal.Target != "" {
				row.Goal += " · " + b.Goal.Target
			}
		}
		if d.IsCurrent(b, day) {
			row.Status = "current"
			switch {
			case b.Contains(day):
				if wk, _, ok := b.Locate(day); ok {
					row.Detail = fmt.Sprintf("week %d of %d", wk.N, b.WeekCount())
				}
			case b.StartDate().After(day):
				n := b.DaysUntilStart(day)
				row.Detail = fmt.Sprintf("starts in %d day%s", n, plural(n))
			default:
				row.Detail = "complete"
			}
		} else {
			row.Status = "archived"
			row.Detail = "finished " + b.EndDate().Format("2 Jan 2006")
			if b.StartDate().After(day) {
				row.Detail = "starts " + b.StartDate().Format("2 Jan 2006")
			}
			row.Query = "?block=" + url.QueryEscape(b.ID)
		}
		for wi, wk := range b.Weeks {
			for di, sess := range wk.Days {
				if sess.Kind == KindRest {
					continue
				}
				row.Sessions++
				if g, ok := grades[b.DayOf(wi, di).Format("2006-01-02")]; ok && g.Val != "" {
					row.Grades[g.Val]++
				}
			}
		}
		bd.Rows = append(bd.Rows, row)
	}
	s.render(w, "blocks.html", bd)
}

// workouts is the index of everything prescribed anywhere in the block, with
// how often it appears, each row opening its guide. The layout comes from
// library/index.json; the counts and dates are computed from the block, so
// they cannot drift from the sessions they describe.
func (s *server) workouts(w http.ResponseWriter, r *http.Request) {
	d := s.ds()
	day := s.day(d)
	blk, ok := d.blockFor(r.URL.Query().Get("block"), day)
	if !ok {
		http.NotFound(w, r)
		return
	}
	week := 1
	if wk, _, ok := blk.Locate(day); ok {
		week = wk.N
	}
	guides, err := blk.ResolveGuides(d.Athlete, week)
	if err != nil {
		log.Printf("guides: %v", err)
		http.Error(w, "guides unavailable", http.StatusInternalServerError)
		return
	}
	ctx := blk.ctxFor(d.Athlete, week)

	wd := workoutsData{Nav: "workouts", Title: "Workouts", Archive: s.archiveOf(d, blk, day)}
	for _, grp := range blk.Index.Groups {
		g := wkGroup{Title: grp.Title}
		g.Note, _ = ctx.resolve(grp.Note)

		for _, row := range grp.Rows {
			gd, ok := guides[row.Guide]
			if !ok {
				continue
			}
			when, _ := ctx.resolve(row.When)
			detail, _ := ctx.resolve(row.Detail)
			if detail == "" {
				detail = describe(gd)
			}
			g.Rows = append(g.Rows, wkRow{Guide: row.Guide, Name: gd.Title, Detail: detail, When: when})
		}

		if grp.Movements != "" {
			var gs []Guide
			for _, gd := range guides {
				if gd.IsMovement() && gd.Group == grp.Movements {
					gs = append(gs, gd)
				}
			}
			sortMovements(gs)
			for _, gd := range gs {
				src := gd.Source
				if gd.Video != "" && src == "" {
					src = "video"
				}
				g.Rows = append(g.Rows, wkRow{Guide: gd.ID, Name: gd.Title, Detail: describe(gd), When: src})
			}
		}

		if len(g.Rows) > 0 {
			wd.Groups = append(wd.Groups, g)
			wd.Count += len(g.Rows)
		}
	}
	s.render(w, "workouts.html", wd)
}

// describe is the one-line summary a workouts row falls back to: the guide's
// own summary, else what its steps contain, else its first section. Derived, so
// the index cannot disagree with the guide it points at.
//
// The result is stripped of emphasis markers rather than rendered with them,
// because it is then truncated -- and a cut that lands inside *…* leaves an
// unclosed marker that would embolden the rest of the line.
func describe(g Guide) string {
	return stripEmph(describeRaw(g))
}

// stripEmph drops the emphasis markers, for every context that takes text
// rather than markup: a truncated summary, a JS string bound for a title
// attribute, a FIT workout name. It is the one opinion on what a bare star
// means, shared with emphasise() above, which renders instead of stripping.
func stripEmph(s string) string {
	return strings.NewReplacer("*", "", "_", "").Replace(s)
}

func describeRaw(g Guide) string {
	if g.Summary != "" {
		return g.Summary
	}
	if len(g.Steps) > 0 {
		names := make([]string, 0, len(g.Steps))
		for _, st := range g.Steps {
			n := st.Text
			if i := strings.Index(n, " — "); i > 0 {
				n = n[:i]
			}
			names = append(names, n)
		}
		return trunc(strings.Join(names, " · "), 110)
	}
	for _, sec := range g.Sections {
		if sec.Label == "Do" {
			return trunc(sec.Text, 90)
		}
	}
	if len(g.Sections) > 0 {
		return trunc(g.Sections[0].Text, 90)
	}
	return ""
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	cut := string(r[:n])
	if i := strings.LastIndex(cut, " "); i > n/2 {
		cut = cut[:i]
	}
	return cut + "…"
}

// nextSessions is how many ordinary sessions the card looks ahead by. One was
// too few: on a rest day it showed Saturday's long run and then nothing until a
// benchmark weeks away, hiding Sunday entirely — the shortest and slowest run
// of the week, so the one most likely to be dropped, and the only day whose
// heart-rate ceiling is different.
const nextSessions = 2

// benchHorizon is how far ahead a benchmark is worth mentioning. Without it the
// card reached for the next one however far away it was, so on a Friday in
// week 1 it announced an LT test eighteen days out — true, useless, and taking
// a row from something actually imminent. A week is the horizon because that is
// the unit the plan is built in, and because the test's own gate is stated in
// it: the calf has to be quiet for seven days beforehand. Sooner than that
// there is nothing to do about it yet.
const benchHorizon = 7

func comingUp(blk *Block, guides map[string]Guide, from time.Time) []upcoming {
	var sessions []upcoming
	var bench *upcoming
scan:
	for wi, wk := range blk.Weeks {
		for i, x := range wk.Days {
			dt := blk.DayOf(wi, i)
			in := DaysBetween(from, dt)
			if in <= 0 {
				continue
			}
			if len(sessions) < nextSessions && x.Kind != KindRest {
				sessions = append(sessions, upcoming{
					When: relDay(dt, in), Label: x.Label, Guide: x.GuideID(), In: in})
			}
			if bench == nil && x.Tag != "" && in <= benchHorizon {
				id := "t-" + x.Tag
				label := x.Tag
				if g, ok := guides[id]; ok {
					label = g.Title
				}
				bench = &upcoming{When: relDay(dt, in), Label: label, Guide: id, In: in, IsBench: true}
			}
			// Past the horizon no benchmark can still qualify, so once the
			// sessions are in there is nothing left to find. Without this the
			// scan runs to the end of the block on every request in the common
			// case where the next benchmark is weeks away.
			if len(sessions) == nextSessions && (bench != nil || in >= benchHorizon) {
				break scan
			}
		}
	}

	out := make([]upcoming, 0, len(sessions)+1)
	for _, s := range sessions {
		// A benchmark row already describes its day, in more useful words.
		if bench != nil && s.In == bench.In {
			continue
		}
		out = append(out, s)
	}
	if bench != nil {
		out = append(out, *bench)
	}
	// The benchmark can fall between the two sessions — a tagged Tuesday with
	// an ordinary Wednesday behind it — so order by day rather than by which
	// list a row came from.
	sort.SliceStable(out, func(i, j int) bool { return out[i].In < out[j].In })
	return out
}

func relDay(d time.Time, in int) string {
	switch in {
	case 0:
		return "Today"
	case 1:
		return "Tomorrow"
	default:
		return d.Format("Mon 2 Jan")
	}
}

/* ── exports ─────────────────────────────────────────────────────────────── */

// exportKind is one workout file format. The FIT and Zwift routes differ in
// which sessions they carry, how a day encodes and what the file is called —
// locating the day, resolving its steps, the 404 rules, the ETag dance and the
// zip loop were copied between them and are written once here. The two file
// routes had already drifted into two spellings of identical error handling.
type exportKind struct {
	tag     string // log prefix
	zipName string // the archive filename's suffix
	carries func(*Session) bool
	slug    func(name string) string
	encode  func(d *dataset, sess *Session, name string, rs []resolvedStep, serial uint32, created time.Time) ([]byte, error)
}

var fitExport = exportKind{
	tag:     "fit",
	zipName: "workouts",
	carries: func(*Session) bool { return true },
	slug:    fitSlug,
	encode: func(_ *dataset, sess *Session, name string, rs []resolvedStep, serial uint32, created time.Time) ([]byte, error) {
		return fitWorkoutFor(name, rs, fitSportFor(sess.Kind), serial, created).Encode()
	},
}

// Bike days only: a .zwo for a run would be a file with no home. Targets are
// fractions of Zwift's own FTP setting, which is why the athlete's FTP is read
// here rather than baked into the steps.
var zwoExport = exportKind{
	tag:     "zwo",
	zipName: "zwift",
	carries: func(sess *Session) bool { return sess.Kind.IsBike() },
	slug:    zwoSlug,
	encode: func(d *dataset, _ *Session, name string, rs []resolvedStep, _ uint32, _ time.Time) ([]byte, error) {
		return zwoFor(name, rs, d.Athlete.Power["ftp"])
	},
}

// encodeDay resolves one day and encodes it, returning the file's name, its
// bytes and its deterministic identity time. Identity derives from (data Rev,
// block id, ISO date) whatever the format, so any data edit rotates every file
// — a batch must never dedupe, and a re-import must look new.
func (k exportKind) encodeDay(d *dataset, blk *Block, wk *Week, di int, date time.Time) (string, []byte, time.Time, error) {
	sess := wk.Days[di]
	sc := blk.ctxFor(d.Athlete, wk.N).forSession(&sess)
	rs, err := resolveSteps(sc, sess)
	if err != nil {
		return "", nil, time.Time{}, err
	}
	name, err := fitName(wk.N, di, sess.Label)
	if err != nil {
		return "", nil, time.Time{}, err
	}
	serial, created := fitIdentity(d.Rev, blk.ID, date)
	body, err := k.encode(d, &sess, name, rs, serial, created)
	if err != nil {
		return "", nil, time.Time{}, err
	}
	return k.slug(name), body, created, nil
}

// serveDownload writes a generated file with its validator. Every export ends
// this way: the bytes are deterministic per data revision, so the body's hash
// is the ETag and an unchanged file costs a 304 instead of a re-encode.
func serveDownload(w http.ResponseWriter, r *http.Request, ctype, filename string, body []byte) {
	etag := etagFor(body)
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=0, must-revalidate")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(body)
}

// exportFile serves a day's structured session as a workout file.
// Date-addressed because ISO date is the app's universal day key. Encoding at
// request time from one dataset snapshot costs ~1 KB of arithmetic, and dryRun
// has already proved every loaded steps day encodes — the error branch is
// defensive, not reachable for loaded data. Every miss is a 404, never a
// fallback file: a synthesized workout for an unstructured day is junk pushed
// to a watch.
func (s *server) exportFile(w http.ResponseWriter, r *http.Request, k exportKind) {
	d := s.ds()
	date, err := time.ParseInLocation("2006-01-02", r.PathValue("date"), d.Loc)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	blk, ok := d.blockFor(r.URL.Query().Get("block"), s.day(d))
	if !ok {
		http.NotFound(w, r)
		return
	}
	wk, di, ok := blk.Locate(date)
	if !ok {
		http.NotFound(w, r)
		return
	}
	sess := wk.Days[di]
	if len(sess.Steps) == 0 || !k.carries(&sess) {
		http.NotFound(w, r)
		return
	}
	slug, body, _, err := k.encodeDay(d, blk, wk, di, date)
	if err != nil {
		log.Printf("%s %s: %v", k.tag, r.PathValue("date"), err)
		http.Error(w, "workout unavailable", http.StatusInternalServerError)
		return
	}
	// Neither format has an IANA registration; octet-stream plus attachment
	// guarantees a download everywhere rather than a browser tab of mojibake.
	serveDownload(w, r, "application/octet-stream", slug, body)
}

// exportZip bundles every day a format carries into one archive, for loading a
// whole block in a single session. Entries are the same bytes the per-day route
// serves, under the same names, stored uncompressed — these are a few hundred
// bytes each and an unzip tool is the only consumer. A block with no day of
// this kind is a 404, because a zip of nothing is the fallback-file mistake in
// a different wrapper.
func (s *server) exportZip(w http.ResponseWriter, r *http.Request, k exportKind) {
	d := s.ds()
	blk, ok := d.blockFor(r.URL.Query().Get("block"), s.day(d))
	if !ok {
		http.NotFound(w, r)
		return
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	n := 0
	for wi, wk := range blk.Weeks {
		for di, sess := range wk.Days {
			if len(sess.Steps) == 0 || !k.carries(&sess) {
				continue
			}
			slug, body, created, err := k.encodeDay(d, blk, wk, di, blk.DayOf(wi, di))
			if err == nil {
				var f io.Writer
				// Modified is the file's own deterministic identity time,
				// so the archive's bytes are as reproducible as its entries'.
				if f, err = zw.CreateHeader(&zip.FileHeader{
					Name: slug, Method: zip.Store, Modified: created,
				}); err == nil {
					_, err = f.Write(body)
				}
			}
			if err != nil {
				log.Printf("%s.zip week %d day %d: %v", k.tag, wk.N, di+1, err)
				http.Error(w, "workouts unavailable", http.StatusInternalServerError)
				return
			}
			n++
		}
	}
	if err := zw.Close(); err != nil {
		log.Printf("%s.zip: %v", k.tag, err)
		http.Error(w, "workouts unavailable", http.StatusInternalServerError)
		return
	}
	if n == 0 {
		http.NotFound(w, r)
		return
	}
	serveDownload(w, r, "application/zip", fitSlugBase(blk.ID)+"-"+k.zipName+".zip", buf.Bytes())
}

func (s *server) fitFile(w http.ResponseWriter, r *http.Request) { s.exportFile(w, r, fitExport) }
func (s *server) fitZip(w http.ResponseWriter, r *http.Request)  { s.exportZip(w, r, fitExport) }
func (s *server) zwoFile(w http.ResponseWriter, r *http.Request) { s.exportFile(w, r, zwoExport) }
func (s *server) zwoZip(w http.ResponseWriter, r *http.Request)  { s.exportZip(w, r, zwoExport) }

type watchRow struct {
	Date    time.Time
	Name    string // the on-watch workout name
	Slug    string
	URL     string
	Checked bool // pre-selected: within the next fourteen days
}

type watchData struct {
	Nav     string
	Title   string
	Archive *archiveBanner
	Rows    []watchRow
}

// watchPage lists a block's structured days for the WebUSB transfer in
// static/webmtp.js. Unlike /fit.zip, a block with no steps days still gets
// the page — it also pulls recorded activities off the watch, which needs no
// rows — but an unknown block id stays a 404, never a fallback.
func (s *server) watchPage(w http.ResponseWriter, r *http.Request) {
	d := s.ds()
	day := s.day(d)
	blk, ok := d.blockFor(r.URL.Query().Get("block"), day)
	if !ok {
		http.NotFound(w, r)
		return
	}
	wd := watchData{Nav: "watch", Title: "Watch", Archive: s.archiveOf(d, blk, day)}
	for wi, wk := range blk.Weeks {
		for di, sess := range wk.Days {
			if len(sess.Steps) == 0 {
				continue
			}
			name, err := fitName(wk.N, di, sess.Label)
			if err != nil {
				// Unreachable for loaded data — dryRun already derived every
				// name — but a page with a silently missing row would be
				// worse than a visible error.
				log.Printf("watch: week %d day %d: %v", wk.N, di+1, err)
				http.Error(w, "workouts unavailable", http.StatusInternalServerError)
				return
			}
			url := "/fit/" + blk.DayOf(wi, di).Format("2006-01-02")
			if wd.Archive != nil {
				url += wd.Archive.Query
			}
			dt := blk.DayOf(wi, di)
			wd.Rows = append(wd.Rows, watchRow{
				Date: dt, Name: name, Slug: fitSlug(name), URL: url,
				// The watch stores at most 25 workouts, so the default
				// selection is a rolling fortnight, not the block.
				Checked: !dt.Before(day) && dt.Before(day.AddDate(0, 0, 14))})
		}
	}
	s.render(w, "watch.html", wd)
}

/* ── api ───────────────────────────────────────────────────────────────── */

// manifest is rendered rather than served from static/, because its name and
// colours come from athlete.json and change on a data reload. That costs it the
// content-hashed URL the other assets get — the hash is computed once at
// startup, and this file is not fixed at startup — so it is served from a
// stable path with an ETag instead. It is fetched once per install, so
// revalidating it is not a cost worth engineering around.
func (s *server) manifest(w http.ResponseWriter, r *http.Request) {
	d := s.ds()
	app := d.Athlete.App
	icon := func(size, purpose string) map[string]any {
		return map[string]any{
			"src":   s.assets.Path("icons/icon-" + size + ".png"),
			"sizes": size + "x" + size, "type": "image/png", "purpose": purpose,
		}
	}
	b, err := json.MarshalIndent(map[string]any{
		"name":             app.Name,
		"short_name":       app.Short,
		"description":      app.Description,
		"start_url":        "/",
		"scope":            "/",
		"display":          "standalone",
		"orientation":      "portrait",
		"background_color": app.Theme.Dark,
		"theme_color":      app.Theme.Dark,
		"icons": []map[string]any{
			icon("192", "any"), icon("512", "any"), icon("512", "maskable"),
		},
	}, "", "  ")
	if err != nil {
		http.Error(w, "manifest unavailable", http.StatusInternalServerError)
		return
	}
	etag := etagFor(b)
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=0, must-revalidate")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(b)
}

func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	d := s.ds()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": "ok",
		"build":  buildHash,
		"data":   d.Rev,
		"loaded": d.LoadedAt.UTC().Format(time.RFC3339),
		"blocks": len(d.Blocks),
	})
}

// postReload is the manual trigger, for when thirty seconds is thirty seconds
// too long. The poller catches everything else.
func (s *server) postReload(w http.ResponseWriter, r *http.Request) {
	old := s.ds().Rev
	if err := s.reload(); err != nil {
		log.Printf("reload FAILED, still serving rev=%s: %v", old, err)
		http.Error(w, "reload failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, "{\"was\":%q,\"now\":%q}\n", old, s.ds().Rev)
}

func (s *server) postEntry(w http.ResponseWriter, r *http.Request) {
	var e Entry
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&e); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	switch e.Kind {
	case "task", kindIssue, legacyCalfKind, "note", "metric", "grade", kindSkip:
	default:
		http.Error(w, `kind must be one of: task, issue, note, metric, grade, skip`, http.StatusBadRequest)
		return
	}
	// A skip says whether, and optionally why. Anything else in val would
	// read as a state this does not have.
	if e.Kind == kindSkip && e.Val != "skipped" && e.Val != "unskipped" {
		http.Error(w, `skip val must be "skipped" or "unskipped"`, http.StatusBadRequest)
		return
	}
	if e.Date == "" {
		e.Date = s.day(s.ds()).Format("2006-01-02")
	}
	if _, err := time.Parse("2006-01-02", e.Date); err != nil {
		http.Error(w, "date must be YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	// A rating is checked against the scale the issue actually declares, so a
	// 0–3 issue cannot quietly accept an 8 and sit in a band that has no
	// meaning. The legacy kind is still accepted and normalised, because a
	// pinned home screen may still be posting it.
	if key := issueKeyOf(e); key != "" {
		is := s.ds().Athlete.Issue(key)
		if is == nil {
			http.Error(w, "no issue declared with key "+strconv.Quote(key), http.StatusBadRequest)
			return
		}
		n, err := strconv.Atoi(strings.TrimSpace(e.Val))
		if err != nil || !is.Scale.contains(n) {
			http.Error(w, fmt.Sprintf("%s must be an integer %d–%d", key, is.Scale.Min, is.Scale.Max),
				http.StatusBadRequest)
			return
		}
		e.Kind, e.Key, e.Val = kindIssue, key, strconv.Itoa(n)
	}
	if err := s.store.Append(e); err != nil {
		log.Printf("append: %v", err)
		http.Error(w, "could not record", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// getGuides serves every popup body in one document. Small enough (~30 KB) that
// one cached fetch beats a round trip per popup.
//
// Guides are per-block: the library is merged with the block's own overrides,
// and every body is resolved against that block's anchors, so an archived
// block's popup says what it said then. This handler used to answer from the
// current block whatever was asked, which meant a '?' on an archived calendar
// opened the right guide with this block's paces inside it.
func (s *server) getGuides(w http.ResponseWriter, r *http.Request) {
	d := s.ds()
	day := s.day(d)
	blk, ok := d.blockFor(r.URL.Query().Get("block"), day)
	if !ok {
		http.NotFound(w, r)
		return
	}
	// Today's week, when today is inside this block; otherwise its first, which
	// is what an archived or not-yet-started block resolves its guides against.
	week := 1
	if wk, _, ok := blk.Locate(day); ok {
		week = wk.N
	}
	g, err := blk.ResolveGuides(d.Athlete, week)
	if err != nil {
		log.Printf("guides: %v", err)
		http.Error(w, "guides unavailable", http.StatusInternalServerError)
		return
	}
	b, err := json.Marshal(g)
	if err != nil {
		http.Error(w, "guides unavailable", http.StatusInternalServerError)
		return
	}
	etag := etagFor(b)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=0, must-revalidate")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(b)
}

// getTasks returns the done-state for every task key on a date, so the guide
// popups can render their checkboxes correctly without embedding state in the
// cacheable /api/guides document.
func (s *server) getTasks(w http.ResponseWriter, r *http.Request) {
	date := r.URL.Query().Get("date")
	if date == "" {
		date = s.day(s.ds()).Format("2006-01-02")
	}
	if _, err := time.Parse("2006-01-02", date); err != nil {
		http.Error(w, "date must be YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(s.store.TasksFor(date))
}

// getIssueTrend hands the trend popup everything it draws: the declared
// scale, the bands in order, and one point per rated day, oldest first.
// The chart cannot invent a boundary the declaration does not state.
func (s *server) getIssueTrend(w http.ResponseWriter, r *http.Request) {
	is := s.ds().Athlete.Issue(r.URL.Query().Get("key"))
	if is == nil {
		http.NotFound(w, r)
		return
	}
	type band struct {
		UpTo  *int   `json:"upto,omitempty"`
		Tone  string `json:"tone"`
		Label string `json:"label"`
	}
	type point struct {
		Date string `json:"date"`
		Val  int    `json:"val"`
		Note string `json:"note,omitempty"`
	}
	out := struct {
		Key    string  `json:"key"`
		Name   string  `json:"name"`
		Ask    string  `json:"ask,omitempty"`
		Min    int     `json:"min"`
		Max    int     `json:"max"`
		Low    string  `json:"low,omitempty"`
		High   string  `json:"high,omitempty"`
		Bands  []band  `json:"bands"`
		Points []point `json:"points"`
	}{Key: is.Key, Name: is.Name, Ask: is.Ask,
		Min: is.Scale.Min, Max: is.Scale.Max, Low: is.Scale.Low, High: is.Scale.High,
		Bands: []band{}, Points: []point{}}
	for _, b := range is.Bands {
		out.Bands = append(out.Bands, band{UpTo: b.UpTo, Tone: b.Tone, Label: b.Label})
	}
	for _, e := range s.store.Ratings(is.Key) {
		n, err := strconv.Atoi(e.Val)
		if err != nil {
			continue
		}
		out.Points = append(out.Points, point{Date: e.Date, Val: n, Note: e.Note})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

// bandFloor reads the lower bound out of an authored range: "≥80%" and
// "60–79%" open at their first number, "under 20%" opens at nothing. It
// recognises those shapes and no others.
var bandFloorRe = regexp.MustCompile(`^(?:[≥>]=?\s*)?(\d+(?:\.\d+)?)`)

func bandFloor(rng string) (float64, bool) {
	s := strings.TrimSpace(rng)
	if s == "" {
		return 0, false
	}
	if strings.HasPrefix(strings.ToLower(s), "under") || strings.HasPrefix(s, "<") {
		return 0, true
	}
	if m := bandFloorRe.FindStringSubmatch(s); m != nil {
		v, err := strconv.ParseFloat(m[1], 64)
		return v, err == nil
	}
	return 0, false
}

// bandFloors derives every band's floor, or nothing at all. All-or-nothing
// because a half-parsed rubric is worse than a prose one: the caller falls
// back to reading the ranges, which are what the athlete sees anyway. The
// floors must descend strictly in the order the legend lists them, which is
// also a check that the parse understood what it read.
func bandFloors(bands []GradeBand) []float64 {
	if len(bands) == 0 {
		return nil
	}
	out := make([]float64, len(bands))
	for i, b := range bands {
		f, ok := bandFloor(b.Range)
		if !ok {
			return nil
		}
		if i > 0 && f >= out[i-1] {
			return nil // not descending: the parse misread something
		}
		out[i] = f
	}
	if out[len(out)-1] != 0 {
		return nil // the last band must catch everything below
	}
	return out
}

// stepView is one prescribed step as an API reader sees it: the same
// numbers the watch is given, in the athlete's own units, with a repeat
// carrying its body rather than being flattened — "4×3′ at 252 W" is the
// thing being judged, and unrolling it into twelve steps loses that.
type stepView struct {
	Role   string     `json:"role,omitempty"`
	Dist   string     `json:"dist,omitempty"`
	Secs   int        `json:"secs,omitempty"`
	Pace   string     `json:"pace,omitempty"`
	HR     []int      `json:"hr,omitempty"`    // [lo, hi] bpm
	Power  []int      `json:"power,omitempty"` // [lo, hi] watts
	Note   string     `json:"note,omitempty"`
	Repeat int        `json:"repeat,omitempty"`
	Steps  []stepView `json:"steps,omitempty"`
}

func stepViews(rs []resolvedStep, u Units) []stepView {
	out := make([]stepView, 0, len(rs))
	for _, s := range rs {
		v := stepView{Role: s.Role, Secs: s.Secs, Note: s.Note, Repeat: s.Repeat}
		if s.DistM > 0 {
			v.Dist = Distance(s.DistM).In(u)
		}
		if s.PaceFast > 0 && s.PaceSlow > 0 {
			v.Pace = s.PaceSlow.In(u) + "–" + s.PaceFast.In(u)
		}
		if s.HRHi > 0 {
			v.HR = []int{s.HRLo, s.HRHi}
		}
		if s.PowerHi > 0 {
			v.Power = []int{s.PowerLo, s.PowerHi}
		}
		if len(s.Body) > 0 {
			v.Steps = stepViews(s.Body, u)
		}
		out = append(out, v)
	}
	return out
}

// getDay serves one date's fully resolved prescription: the session as the
// athlete would read it, the week it sits in, the block's grading legend,
// and the anchors current at this moment — everything the grader (and any
// future UI) needs to measure a day without re-implementing resolution.
// Strings are template-resolved against the real context and emphasis-
// stripped, because this is an API that serves text, not markup. Misses are
// 400/404, never a fallback: a date outside the block has no prescription,
// and inventing one is how two sources of truth start.
func (s *server) getDay(w http.ResponseWriter, r *http.Request) {
	out, code, msg := s.dayPayload(r.URL.Query().Get("date"), r.URL.Query().Get("block"))
	if code != http.StatusOK {
		http.Error(w, msg, code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

/* The wire shape of one day. These are named types rather than anonymous
   structs inside the builder because two readers depend on the exact field
   set: the grader's get_prescription tool, and whatever reads /api/day. Field
   ORDER here is the JSON's key order, so it is part of the contract. */

// dayBand is one row of the block's grading legend.
type dayBand struct {
	Grade string `json:"grade"`
	Range string `json:"range"`
	// MinPct is the band's floor as a percentage, DERIVED from the authored
	// range beside it — never a second copy of the rubric to drift from. Bands
	// are ordered best first, so the letter is the first band whose floor the
	// share reaches. Reading it off the prose instead put a 21% share in the
	// "under 20%" band once.
	MinPct *float64 `json:"min_pct,omitempty"`
}

type dayLegend struct {
	Note   string    `json:"note,omitempty"`
	Bands  []dayBand `json:"bands"`
	Footer string    `json:"footer,omitempty"`
}

type daySession struct {
	Kind     string     `json:"kind"`
	Label    string     `json:"label"`
	Dist     string     `json:"dist,omitempty"`
	DistM    float64    `json:"dist_m,omitempty"`
	Mins     int        `json:"mins,omitempty"`
	Tag      string     `json:"tag,omitempty"`
	Detail   string     `json:"detail,omitempty"`
	Targets  []string   `json:"targets"`
	HasSteps bool       `json:"has_steps,omitempty"`
	Steps    []stepView `json:"steps,omitempty"`
}

// dayIssue is what the athlete said about an injury that day, and what the
// declaration says to do about it. A session run easy because a rating came
// back high was not a failed quality day, and the grader cannot know that
// without both halves: the number and its instruction.
type dayIssue struct {
	Key    string `json:"key"`
	Name   string `json:"name"`
	Rating int    `json:"rating"`
	Scale  string `json:"scale"`
	Tone   string `json:"tone,omitempty"`
	Label  string `json:"label,omitempty"`
	Action string `json:"action,omitempty"`
	Note   string `json:"note,omitempty"`
}

type dayDoc struct {
	Date     string         `json:"date"`
	Skipped  bool           `json:"skipped,omitempty"`
	SkipNote string         `json:"skip_note,omitempty"`
	Block    string         `json:"block"`
	WeekN    int            `json:"week"`
	Tags     []string       `json:"week_tags"`
	Session  daySession     `json:"session"`
	Grading  *dayLegend     `json:"grading,omitempty"`
	Units    string         `json:"units"`
	Weight   float64        `json:"weight_kg,omitempty"`
	HR       map[string]int `json:"hr,omitempty"`
	Power    map[string]int `json:"power,omitempty"`
	Issues   []dayIssue     `json:"issues,omitempty"`
	// The last time this kind of session was asked for, so a grade is made
	// against something comparable without having to go looking for it.
	Previous *daySibling `json:"previous,omitempty"`
}

// dayPayload builds the response for the handler above and for the grader's
// get_prescription tool — one builder, one truth about what a day asks. It
// locates the day and delegates each part of the document; an error from any
// part is the whole request failing, because half a prescription is worse than
// none.
func (s *server) dayPayload(dateStr, blockID string) (any, int, string) {
	d := s.ds()
	date, err := time.ParseInLocation("2006-01-02", dateStr, d.Loc)
	if err != nil {
		return nil, http.StatusBadRequest, "date must be YYYY-MM-DD"
	}
	blk, ok := d.blockFor(blockID, s.day(d))
	if !ok {
		return nil, http.StatusNotFound, "no such block"
	}
	wk, di, ok := blk.Locate(date)
	if !ok {
		return nil, http.StatusNotFound, "date is outside the block"
	}
	sess := wk.Days[di]
	// The legend is the block's, not this day's, so it keeps the unnarrowed
	// context — same as the calendar page renders it against.
	ctx := blk.ctxFor(d.Athlete, wk.N)

	sv, err := s.daySessionView(d, blk, ctx.forSession(&sess), sess, dateStr)
	if err != nil {
		return nil, http.StatusInternalServerError, "prescription unavailable"
	}

	iso := date.Format("2006-01-02")
	out := dayDoc{
		Date: iso, Block: blk.ID,
		WeekN: wk.N, Tags: wk.Tags,
		Session: sv,
		Grading: dayLegendFor(ctx, blk.Grading),
		Units:   string(d.Athlete.Units),
		Weight:  float64(d.Athlete.Weight),
		HR:      d.Athlete.HR,
		Power:   d.Athlete.Power,
		Issues:  s.dayIssues(d, iso),
	}
	if out.Tags == nil {
		out.Tags = []string{}
	}
	if e, skipped := s.store.SkipOn(iso); skipped {
		out.Skipped, out.SkipNote = true, e.Note
	}
	if h := s.sameKindHistory(d, blk, date, sess.Kind, 1); len(h) > 0 {
		out.Previous = &h[0]
	}
	return out, http.StatusOK, ""
}

// daySessionView resolves the session's own strings against sc and renders its
// quantities in the athlete's units. Strings are emphasis-stripped, because
// this is an API that serves text, not markup.
func (s *server) daySessionView(d *dataset, blk *Block, sc *Ctx, sess Session, dateStr string) (daySession, error) {
	detail, err := sc.resolve(sess.Detail)
	if err != nil {
		log.Printf("day %s: detail: %v", dateStr, err)
		return daySession{}, err
	}
	targets, err := sc.resolveAll(blk.TargetsFor(sess))
	if err != nil {
		log.Printf("day %s: targets: %v", dateStr, err)
		return daySession{}, err
	}
	for i := range targets {
		targets[i] = stripEmph(targets[i])
	}
	if targets == nil {
		targets = []string{}
	}
	sv := daySession{
		Kind: string(sess.Kind), Label: stripEmph(sess.Label),
		Mins: sess.Mins, Tag: sess.Tag,
		Detail: stripEmph(detail), Targets: targets,
		HasSteps: len(sess.Steps) > 0,
	}
	if sess.Dist > 0 {
		sv.Dist = sess.Dist.In(d.Athlete.Units)
		sv.DistM = float64(sess.Dist)
	}
	// The structured form, when the day has one. Without it a grader judging an
	// interval session can only see the athlete's standing anchors — and would
	// measure a VO₂ day against the easy-ride band, which is exactly backwards.
	// What was asked for on THIS day lives here.
	if len(sess.Steps) > 0 {
		rs, err := resolveSteps(sc, sess)
		if err != nil {
			log.Printf("day %s: steps: %v", dateStr, err)
			return daySession{}, err
		}
		sv.Steps = stepViews(rs, d.Athlete.Units)
	}
	return sv, nil
}

// dayIssues is every issue the athlete rated on this date, with the band that
// rating falls in. A rating outside its declared scale is dropped rather than
// reported: it is a number with no instruction attached.
func (s *server) dayIssues(d *dataset, iso string) []dayIssue {
	var out []dayIssue
	for i := range d.Athlete.Issues {
		is := &d.Athlete.Issues[i]
		e := s.store.RatingOn(is.Key, iso)
		if e == nil {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(e.Val))
		if err != nil || !is.Scale.contains(n) {
			continue
		}
		r := dayIssue{Key: is.Key, Name: is.Name, Rating: n,
			Scale: is.Range(), Note: e.Note}
		if b := is.BandFor(n); b != nil {
			r.Tone, r.Label, r.Action = b.Tone, b.Label, stripEmph(b.Action)
		}
		out = append(out, r)
	}
	return out
}

// daySibling is one earlier session of the same kind, measured. A grade is
// only meaningful against a comparable session: a long run is judged next to
// the previous long run, not next to whatever happened most recently. The
// grader had every prior grade in front of it and still compared a 10-mile
// long run to a 6-mile easy run with strides, because the log records the
// ENTRY kind — grade, note, issue — and never the SESSION kind. It had to
// guess from prose. This is the missing fact, stated.
type daySibling struct {
	Date  string `json:"date"`
	Label string `json:"label"`
	Grade string `json:"grade,omitempty"`
	// Elapsed and Dist carry their units, because these numbers are quoted
	// back to the athlete in a note: a comparison reading "16128.53 m vs
	// 16146.8 m, 6009 s vs 5870 s" is arithmetic done at the reader, and the
	// first draft of this feature produced exactly that sentence.
	Elapsed  string   `json:"elapsed_hms,omitempty"`
	Dist     string   `json:"dist,omitempty"`
	ElapsedS int      `json:"elapsed_s,omitempty"`
	Distance float64  `json:"distance_m,omitempty"`
	AvgHR    *float64 `json:"avg_hr,omitempty"`
	UnderCap *float64 `json:"under_grade_cap_share,omitempty"`
	AvgPower *float64 `json:"avg_power,omitempty"`
	Decoup   *float64 `json:"decoupling_pct,omitempty"`
	TempF    *float64 `json:"temp_f,omitempty"`
}

// sameKindHistory walks back through the block for earlier days prescribing
// the same kind, newest first, and measures each one the way this day will be
// measured. Strictly BEFORE the given date, so it can never leak the grade of
// the day under test — blindness costs nothing here because the answer simply
// does not include today.
//
// Confined to this block on purpose: anchors, caps and prescriptions are the
// block's, so a share from an older block is a different measurement wearing
// the same name.
func (s *server) sameKindHistory(d *dataset, blk *Block, before time.Time, kind Kind, limit int) []daySibling {
	if limit <= 0 {
		limit = 6
	}
	grades := s.store.Grades()
	// Which anchor the run rubric measures under is THIS block's declaration.
	// Reaching for "gradeCap" by name works only for an athlete who happens to
	// declare one — the example athlete grades under easyCap, which is why the
	// defaults exist.
	cap, hasCap := d.Athlete.HR[blk.Grading.CapKey()]
	out := []daySibling{}
	for wi := len(blk.Weeks) - 1; wi >= 0 && len(out) < limit; wi-- {
		wk := blk.Weeks[wi]
		for di := len(wk.Days) - 1; di >= 0 && len(out) < limit; di-- {
			sess := wk.Days[di]
			if sess.Kind != kind {
				continue
			}
			dt := blk.DayOf(wi, di)
			if !dt.Before(before) {
				continue
			}
			iso := dt.Format("2006-01-02")
			sib := daySibling{Date: iso, Label: stripEmph(sess.Label)}
			if g, ok := grades[iso]; ok {
				sib.Grade = g.Val
			}
			if m := s.bestActivityOn(iso, kind.IsBike()); m != nil {
				sib.ElapsedS = m.ElapsedS
				sib.Elapsed = fmt.Sprintf("%d:%02d", m.ElapsedS/60, m.ElapsedS%60)
				if m.DistanceM != nil {
					sib.Distance = pyRound(*m.DistanceM, 1)
					sib.Dist = Distance(*m.DistanceM).In(d.Athlete.Units)
				}
				if m.AvgHR != nil {
					v := pyRound(*m.AvgHR, 1)
					sib.AvgHR = &v
				}
				if m.DecouplingPct != nil {
					v := pyRound(*m.DecouplingPct, 2)
					sib.Decoup = &v
				}
				if m.AvgPower != nil {
					v := pyRound(*m.AvgPower, 1)
					sib.AvgPower = &v
				}
				if m.Weather != nil {
					v := m.Weather.TempF
					sib.TempF = &v
				}
				// The rubric's own number, recomputed against the anchors
				// current now rather than whatever they were then — the same
				// rule /api/activity-metrics follows, so two readings of the
				// same session never disagree.
				if hasCap && !kind.IsBike() {
					if share, err := s.metrics.underCapShareSQL(m.Name, cap); err == nil && share != nil {
						v := pyRound(*share, 4)
						sib.UnderCap = &v
					}
				}
			}
			out = append(out, sib)
		}
	}
	return out
}

// bestActivityOn is the day's activity for a sport — the longest, when a day
// carries more than one recording.
//
// byDate answers with name, date and sport only; it is the cheap index the
// reconcile walks. The measurements come from rowByName, and forgetting that
// yields a row of zeroes that looks like a session with no duration rather
// than like a bug.
func (s *server) bestActivityOn(iso string, bike bool) *activityMetrics {
	rows, err := s.metrics.byDate(iso)
	if err != nil {
		log.Printf("history %s: %v", iso, err)
		return nil
	}
	var best *activityMetrics
	for i := range rows {
		if (rows[i].Sport == "cycling") != bike {
			continue
		}
		full, err := s.metrics.rowByName(rows[i].Name)
		if err != nil || full == nil {
			continue
		}
		if best == nil || full.ElapsedS > best.ElapsedS {
			best = full
		}
	}
	return best
}

// sessionHistoryPayload backs GET /api/session-history and the grader's
// session_history tool — the same handler/builder pair as the day and the
// activity metrics, so there is one answer to "how has this kind been going".
func (s *server) sessionHistoryPayload(dateStr, blockID string, limit int) (any, int, string) {
	d := s.ds()
	date, err := time.ParseInLocation("2006-01-02", dateStr, d.Loc)
	if err != nil {
		return nil, http.StatusBadRequest, "date must be YYYY-MM-DD"
	}
	blk, ok := d.blockFor(blockID, s.day(d))
	if !ok {
		return nil, http.StatusNotFound, "no such block"
	}
	wk, di, ok := blk.Locate(date)
	if !ok {
		return nil, http.StatusNotFound, "date is outside the block"
	}
	sess := wk.Days[di]
	out := struct {
		Date     string       `json:"date"`
		Kind     string       `json:"kind"`
		Label    string       `json:"label"`
		Sessions []daySibling `json:"sessions"`
	}{date.Format("2006-01-02"), string(sess.Kind), stripEmph(sess.Label),
		s.sameKindHistory(d, blk, date, sess.Kind, limit)}
	return out, http.StatusOK, ""
}

func (s *server) getSessionHistory(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	out, code, msg := s.sessionHistoryPayload(r.URL.Query().Get("date"), r.URL.Query().Get("block"), limit)
	if code != http.StatusOK {
		http.Error(w, msg, code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

// dayLegendFor renders the block's grading legend, floors derived from the
// authored ranges rather than restated beside them.
func dayLegendFor(ctx *Ctx, g *Grading) *dayLegend {
	g = resolveGrading(ctx, g)
	if g == nil {
		return nil
	}
	l := &dayLegend{Note: stripEmph(g.Note), Footer: stripEmph(g.Footer), Bands: []dayBand{}}
	floors := bandFloors(g.Bands)
	for i, b := range g.Bands {
		bd := dayBand{Grade: b.Grade, Range: b.Range}
		if floors != nil {
			f := floors[i]
			bd.MinPct = &f
		}
		l.Bands = append(l.Bands, bd)
	}
	return l
}

func (s *server) getEntries(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(s.store.All())
}

/* ── plumbing ──────────────────────────────────────────────────────────── */

type task struct{ key, label, guide string }

// checklist is the day's task list, built from the block's declaration. Keys
// are stable so the log stays meaningful if a label is later reworded, which is
// why they are authored rather than derived from the text.
func checklist(c *Ctx, blk *Block) []task {
	var out []task
	for _, it := range blk.Checklist {
		on, err := c.isTrue(it.When)
		if err != nil {
			log.Printf("checklist %q: %v", it.Key, err)
			continue
		}
		if !on {
			continue
		}
		label, err := c.resolve(it.Label)
		if err != nil {
			log.Printf("checklist %q label: %v", it.Key, err)
			continue
		}
		guide, err := c.resolve(it.Guide)
		if err != nil {
			log.Printf("checklist %q guide: %v", it.Key, err)
			continue
		}
		out = append(out, task{key: it.Key, label: label, guide: guide})
	}
	return out
}

var dayNames = [7]string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}

func (s *server) makeFuncs() template.FuncMap {
	return template.FuncMap{
		"asset": s.assets.Path,
		"date":  func(t time.Time) string { return t.Format("Mon 2 Jan") },
		"iso":   func(t time.Time) string { return t.Format("2006-01-02") },
		"lower": strings.ToLower,
		// amount and qty render in the athlete's units, read at request time so
		// a reload that switches units takes effect without a restart.
		"amount":  func(x Session) string { return x.Amount(s.units()) },
		"qty":     func(d Distance) string { return d.In(s.units()) },
		"qtyLong": func(d Distance) string { return d.InLong(s.units()) },
		"emph":    emphasise,
		// multiBlock keeps the Blocks nav item and its cross-links out of the
		// way until there is actually more than one block to choose between.
		"multiBlock": func() bool { return len(s.ds().Blocks) > 1 },
		// watchable shows the Watch tab only when the current block has
		// something to send — /watch is a 404 otherwise, and a tab that
		// 404s is worse than no tab.
		"watchable": func() bool {
			d := s.ds()
			blk := d.Current(s.day(d))
			if blk == nil {
				return false
			}
			for _, wk := range blk.Weeks {
				for _, sess := range wk.Days {
					if len(sess.Steps) > 0 {
						return true
					}
				}
			}
			return false
		},
		"app": func() App { return s.ds().Athlete.App },
		// brand is the block being trained, not the app. The header used to
		// read "16-WEEK BUILD" as a constant, which is the block's name and
		// would be a lie the day an eight-week one starts.
		"brand": func() string {
			d := s.ds()
			if b := d.Current(s.day(d)); b != nil {
				return b.Name
			}
			return d.Athlete.App.Name
		},
		"kindCls":  func(k Kind) string { return "k-" + k.slug() },
		"kindSlug": func(k Kind) string { return k.slug() },
		"dayName": func(i int) string {
			if i < 0 || i > 6 {
				return ""
			}
			return dayNames[i]
		},
	}
}

// emphasise turns *starred* runs of block copy into <strong>, _underscored_
// runs into <em>, and newlines into <br>, escaping everything else. Moving the
// calendar legend into JSON would otherwise have cost it the emphasis it had as
// markup, and session detail carries all three. Only those three tags can ever
// come out of this, so a data file still cannot inject markup into the page.
func emphasise(s string) template.HTML {
	return template.HTML(emphasiseWith(s, htmlEscape))
}

// emphasiseWith is the one implementation of what a star means. The escaper is
// a parameter because the two consumers want different policies: a page served
// by the app takes html/template's, which also escapes quotes, while the
// exporter takes a laxer one so the published documents keep their apostrophes
// instead of several hundred invisible &#39;.
func emphasiseWith(s string, escape func(string) string) string {
	var b strings.Builder
	for i, part := range strings.Split(s, "*") {
		if i%2 == 1 {
			b.WriteString("<strong>")
			writeEm(&b, part, escape)
			b.WriteString("</strong>")
			continue
		}
		writeEm(&b, part, escape)
	}
	return b.String()
}

// writeEm handles the second emphasis level, nested inside the first so that
// *a _b_ c* works. Splitting pairs up delimiters exactly as the outer level
// does, which is what keeps an unclosed one behaving as it always has.
func writeEm(b *strings.Builder, s string, escape func(string) string) {
	for i, part := range strings.Split(s, "_") {
		if i%2 == 1 {
			b.WriteString("<em>")
			writeLines(b, part, escape)
			b.WriteString("</em>")
			continue
		}
		writeLines(b, part, escape)
	}
}

// writeLines is where every byte that reaches the page is escaped. A newline
// becomes a break: session detail is authored as two lines (the headline, then
// the segment breakdown) and JSON has no other way to say so.
func writeLines(b *strings.Builder, s string, escape func(string) string) {
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			b.WriteString("<br>")
		}
		b.WriteString(escape(line))
	}
}

func htmlEscape(s string) string {
	var b strings.Builder
	template.HTMLEscape(&b, []byte(s))
	return b.String()
}

func (s *server) render(w http.ResponseWriter, page string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// HTML is the index into hashed asset URLs, so it must never be served
	// stale: no-cache means "revalidate every time", not "do not store".
	w.Header().Set("Cache-Control", "no-cache")
	if err := s.tpl.ExecuteTemplate(w, page, data); err != nil {
		log.Printf("render %s: %v", page, err)
	}
}

func logging(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
