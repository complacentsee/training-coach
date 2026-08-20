package main

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// defaultsFS is a generic starter block and the movement library, so a fresh
// container serves something coherent before anyone has put data on the volume.
// It is a fallback, not a merge partner: the moment the volume supplies blocks,
// the example ones disappear rather than lingering in the index forever.
//
//go:embed defaults
var defaultsFS embed.FS

// dataset is everything loaded from disk, swapped atomically. Handlers take one
// pointer per request, so a reload can never show them half-old data.
type dataset struct {
	Athlete *Athlete
	Library map[string]*Guide
	Index   *Index
	Blocks  []*Block // ordered by start date

	Loc      *time.Location
	Rev      string // content hash, reported by /healthz
	LoadedAt time.Time
	Origins  map[string]string // logical path → "embedded" or the file it came from
}

// Current is the block being trained. Everything else is archived — there is no
// third state, which is what keeps "between blocks" from needing a design of
// its own: a block that has finished with nothing to replace it simply stays
// current and says so.
//
// The order matters. Preferring the soonest block still to start over the most
// recently finished one is what preserves the "Block starts in N days" screen:
// authoring next year's block in December should move Today onto its countdown,
// not leave it reporting that the old one is complete.
func (d *dataset) Current(day time.Time) *Block {
	if len(d.Blocks) == 0 {
		return nil
	}
	live := make([]*Block, 0, len(d.Blocks))
	for _, b := range d.Blocks {
		if !b.Archived {
			live = append(live, b)
		}
	}
	if len(live) == 0 {
		// Everything is explicitly archived. Rather than show nothing, fall
		// back to the most recent — being wrong about "current" beats a blank
		// front page.
		return d.Blocks[len(d.Blocks)-1]
	}
	for _, b := range live {
		if b.Contains(day) {
			return b
		}
	}
	for _, b := range live { // sorted by start, so the first future one is the soonest
		if b.StartDate().After(day) {
			return b
		}
	}
	return live[len(live)-1]
}

// IsCurrent reports whether b is the block being trained on the given day.
func (d *dataset) IsCurrent(b *Block, day time.Time) bool {
	return b != nil && b == d.Current(day)
}

func (d *dataset) BlockByID(id string) *Block {
	for _, b := range d.Blocks {
		if b.ID == id {
			return b
		}
	}
	return nil
}

// blockFor resolves the ?block= parameter, falling back to the current block.
// An unknown id is an error rather than a silent fallback: a stale bookmark
// should say so, not quietly show a different block's data.
// inAnyBlock reports whether an ISO date falls inside a loaded block. The
// archive holds whatever the watch carried, including years of sessions no
// plan ever asked for; a block date is the subset anything grades.
func (d *dataset) inAnyBlock(iso string) bool {
	t, err := time.ParseInLocation("2006-01-02", iso, d.Loc)
	if err != nil {
		return false
	}
	for _, b := range d.Blocks {
		if _, _, ok := b.Locate(t); ok {
			return true
		}
	}
	return false
}

func (d *dataset) blockFor(id string, day time.Time) (*Block, bool) {
	if id == "" {
		b := d.Current(day)
		return b, b != nil
	}
	b := d.BlockByID(id)
	return b, b != nil
}

/* ── sources ───────────────────────────────────────────────────────────── */

// source resolves a logical path such as "library/movements.json" against the
// volume first and the embedded defaults second.
type source struct {
	dir string // volume data directory
	def fs.FS  // embedded defaults, rooted
}

func (s source) read(logical string) (data []byte, origin string, err error) {
	if s.dir != "" {
		p := filepath.Join(s.dir, filepath.FromSlash(logical))
		b, err := os.ReadFile(p)
		if err == nil {
			return b, p, nil
		}
		if !os.IsNotExist(err) {
			return nil, p, err
		}
	}
	b, err := fs.ReadFile(s.def, logical)
	if err != nil {
		return nil, "", fmt.Errorf("%s: not on the volume and not embedded", logical)
	}
	return b, "embedded:" + logical, nil
}

// readFrom reads one file from a decided side, with no fallback. Directories
// are all-or-nothing, so once listJSON has said which side a directory came
// from, its files must come from that same side.
func (s source) readFrom(logical string, fromVolume bool) (data []byte, origin string, err error) {
	if fromVolume {
		p := filepath.Join(s.dir, filepath.FromSlash(logical))
		b, err := os.ReadFile(p)
		return b, p, err
	}
	b, err := fs.ReadFile(s.def, logical)
	return b, "embedded:" + logical, err
}

// listJSON returns the .json files in a logical directory, volume first. If the
// volume has any, the embedded ones are ignored entirely — a directory is
// wholly one side's or wholly the other's.
func (s source) listJSON(logical string) (paths []string, fromVolume bool) {
	if s.dir != "" {
		ents, err := os.ReadDir(filepath.Join(s.dir, filepath.FromSlash(logical)))
		if err == nil {
			for _, e := range ents {
				if isDataFile(e) {
					paths = append(paths, path.Join(logical, e.Name()))
				}
			}
		}
		if len(paths) > 0 {
			sort.Strings(paths)
			return paths, true
		}
	}
	ents, err := fs.ReadDir(s.def, logical)
	if err == nil {
		for _, e := range ents {
			if isDataFile(e) {
				paths = append(paths, path.Join(logical, e.Name()))
			}
		}
	}
	sort.Strings(paths)
	return paths, false
}

// isDataFile skips anything hidden. macOS tar writes AppleDouble "._name.json"
// side-files when a source file carries an xattr; on the box those extract
// alongside the real ones, and they are binary. Treating them as data crash-
// looped the container the first time this shipped. .DS_Store and editor swap
// files would do the same.
func isDataFile(e fs.DirEntry) bool {
	return !e.IsDir() &&
		strings.HasSuffix(e.Name(), ".json") &&
		!strings.HasPrefix(e.Name(), ".")
}

/* ── loading ───────────────────────────────────────────────────────────── */

func loadDataset(dir string, fallbackLoc *time.Location) (*dataset, error) {
	def, err := fs.Sub(defaultsFS, "defaults")
	if err != nil {
		return nil, fmt.Errorf("embedded defaults: %w", err)
	}
	src := source{dir: dir, def: def}

	d := &dataset{Origins: map[string]string{}, LoadedAt: time.Now()}
	h := sha256.New()
	record := func(logical string, b []byte, origin string) {
		d.Origins[logical] = origin
		fmt.Fprintf(h, "%s\x00%d\x00", logical, len(b))
		h.Write(b)
	}
	// take falls back to the embedded copy; takeFrom does not.
	take := func(logical string) ([]byte, error) {
		b, origin, err := src.read(logical)
		if err != nil {
			return nil, err
		}
		record(logical, b, origin)
		return b, nil
	}
	takeFrom := func(logical string, fromVolume bool) ([]byte, error) {
		b, origin, err := src.readFrom(logical, fromVolume)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", logical, err)
		}
		record(logical, b, origin)
		return b, nil
	}

	// who
	ab, err := take("athlete.json")
	if err != nil {
		return nil, err
	}
	var athlete Athlete
	if err := json.Unmarshal(ab, &athlete); err != nil {
		return nil, fmt.Errorf("athlete.json: %w", err)
	}
	if err := athlete.validate(); err != nil {
		return nil, fmt.Errorf("athlete.json: %w", err)
	}
	d.Athlete = &athlete
	d.Loc = athlete.location(fallbackLoc)

	// The library is volume-or-embedded, never a mix. Merging the two by
	// filename let the example guides leak in alongside the real ones and, worse,
	// made which definition won depend on the alphabet.
	libPaths, libFromVolume := src.listJSON("library")
	if len(libPaths) == 0 {
		return nil, fmt.Errorf("no library found in %s/library or the embedded defaults", dir)
	}
	d.Library = map[string]*Guide{}
	var haveIndex bool
	for _, lp := range libPaths {
		if path.Base(lp) == "index.json" {
			haveIndex = true
			continue
		}
		b, err := takeFrom(lp, libFromVolume)
		if err != nil {
			return nil, err
		}
		var gf GuideFile
		if err := json.Unmarshal(b, &gf); err != nil {
			return nil, fmt.Errorf("%s: %w", lp, err)
		}
		if gf.Schema != librarySchema {
			return nil, fmt.Errorf("%s: schema is %q, this build understands %q", lp, gf.Schema, librarySchema)
		}
		for id, g := range gf.Guides {
			if g.ID == "" {
				g.ID = id
			}
			if g.ID != id {
				return nil, fmt.Errorf("%s: guide keyed %q declares id %q", lp, id, g.ID)
			}
			d.Library[id] = g
		}
	}
	if len(d.Library) == 0 {
		return nil, fmt.Errorf("library is empty — no guides to show")
	}

	// The workouts-page layout comes from the same side as the guides it
	// points at. Falling back to the embedded index over a volume library would
	// name guides that are not there.
	if !haveIndex {
		side := "the embedded defaults"
		if libFromVolume {
			side = dir + "/library"
		}
		return nil, fmt.Errorf("library/index.json is missing from %s", side)
	}
	ib, err := takeFrom("library/index.json", libFromVolume)
	if err != nil {
		return nil, err
	}
	var idx Index
	if err := json.Unmarshal(ib, &idx); err != nil {
		return nil, fmt.Errorf("library/index.json: %w", err)
	}
	if err := idx.validate(); err != nil {
		return nil, fmt.Errorf("library/index.json: %w", err)
	}
	d.Index = &idx

	// the blocks
	blockPaths, fromVolume := src.listJSON("blocks")
	if len(blockPaths) == 0 {
		return nil, fmt.Errorf("no blocks found in %s/blocks or the embedded defaults", dir)
	}
	for _, bp := range blockPaths {
		raw, err := takeFrom(bp, fromVolume)
		if err != nil {
			return nil, err
		}
		var b Block
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, fmt.Errorf("%s: %w", bp, err)
		}
		b.path = bp
		b.SetLocation(d.Loc)
		if err := b.validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", bp, err)
		}
		for _, w := range b.Weeks {
			w.block = &b
		}
		// library ∪ this block's overrides, by id
		b.guides = make(map[string]*Guide, len(d.Library)+len(b.Guides))
		for id, g := range d.Library {
			b.guides[id] = g
		}
		for id, g := range b.Guides {
			if g.ID == "" {
				g.ID = id
			}
			if g.ID != id {
				return nil, fmt.Errorf("%s: guide override keyed %q declares id %q", bp, id, g.ID)
			}
			b.guides[id] = g
		}
		if b.Index == nil {
			b.Index = d.Index
		} else if err := b.Index.validate(); err != nil {
			return nil, fmt.Errorf("%s: index: %w", bp, err)
		}
		d.Blocks = append(d.Blocks, &b)
	}

	for _, id := range dupBlockIDs(d.Blocks) {
		return nil, fmt.Errorf("two blocks share the id %q", id)
	}
	sort.Slice(d.Blocks, func(i, j int) bool { return d.Blocks[i].Start < d.Blocks[j].Start })

	// Every template, every week, before a single request is served. A missing
	// field is a load error here rather than a blank page in November.
	for _, b := range d.Blocks {
		if err := d.dryRun(b); err != nil {
			return nil, fmt.Errorf("%s: %w", b.path, err)
		}
	}

	// A tracked task no loaded block's checklist offers is a typo'd key
	// that would evaluate to silence forever — refused here like every
	// other dangling name.
	for _, is := range d.Athlete.Issues {
		for _, key := range is.Tracks {
			found := false
			for _, b := range d.Blocks {
				for _, it := range b.Checklist {
					if it.Key == key {
						found = true
					}
				}
			}
			if !found {
				return nil, fmt.Errorf("issue %q tracks checklist key %q, which no loaded block offers", is.Key, key)
			}
		}
	}

	d.Rev = hex.EncodeToString(h.Sum(nil))[:12]
	return d, nil
}

func dupBlockIDs(bs []*Block) []string {
	seen := map[string]bool{}
	var dup []string
	for _, b := range bs {
		if seen[b.ID] {
			dup = append(dup, b.ID)
		}
		seen[b.ID] = true
	}
	return dup
}

/* ── load-time validation ──────────────────────────────────────────────── */

// dryRun resolves everything the app could ever render for this block. It is
// the mitigation for the one real risk in moving prose into templates: without
// it, a typo in a field name is a 500 on the day that week comes round.
func (d *dataset) dryRun(b *Block) error {
	var fitNames []fitNamed
	for _, w := range b.Weeks {
		c := b.ctxFor(d.Athlete, w.N)

		for name := range b.Vars {
			if _, err := c.lookupVar(name); err != nil {
				return fmt.Errorf("week %d: %w", w.N, err)
			}
		}
		for kind, lines := range b.Targets.Kinds {
			if _, err := c.resolveAll(lines); err != nil {
				return fmt.Errorf("week %d: targets.kinds.%s%w", w.N, kind, err)
			}
		}
		for tag, lines := range b.Targets.Tags {
			if _, err := c.resolveAll(lines); err != nil {
				return fmt.Errorf("week %d: targets.tags.%s%w", w.N, tag, err)
			}
		}
		for id, g := range b.guides {
			rg, err := g.resolve(c)
			if err != nil {
				return fmt.Errorf("week %d: guide %s: %w", w.N, id, err)
			}
			for i, st := range rg.Steps {
				if st.Guide != "" && b.guides[st.Guide] == nil {
					return fmt.Errorf("guide %s step %d links to %q, which is not in the library", id, i+1, st.Guide)
				}
			}
		}
		for di, sess := range w.Days {
			sc := c.forSession(&sess)
			if id := sess.GuideID(); id != "" && b.guides[id] == nil {
				return fmt.Errorf("week %d day %d (%s) needs guide %q, which is not in the library",
					w.N, di+1, sess.Kind, id)
			}
			if _, err := sc.resolve(sess.Detail); err != nil {
				return fmt.Errorf("week %d day %d: detail: %w", w.N, di+1, err)
			}
			if _, err := sc.resolveAll(sess.Targets); err != nil {
				return fmt.Errorf("week %d day %d: targets%w", w.N, di+1, err)
			}
			if len(sess.Steps) > 0 {
				rs, err := resolveSteps(sc, sess)
				if err != nil {
					return fmt.Errorf("week %d day %d: steps: %w", w.N, di+1, err)
				}
				// The founding label/detail drift bug must not reopen one
				// layer down: the distance the steps prescribe has to fit the
				// distance the session claims. Time leaves make the sum a
				// lower bound, so only the over side is fatal.
				if sess.Dist > 0 {
					if sum := stepsDistance(rs); sum > float64(sess.Dist)*1.02 {
						return fmt.Errorf("week %d day %d: steps measure %s but the session says %s — more than 2%% over",
							w.N, di+1, Distance(sum).In(d.Athlete.Units), sess.Dist.In(d.Athlete.Units))
					}
				}
				// A trainer workout ends when its steps do, so a bike day's
				// timed total is held to the session's minutes the same way.
				if sess.Kind.IsBike() && sess.Mins > 0 {
					if secs := stepsSeconds(rs); float64(secs) > float64(sess.Mins*60)*1.02 {
						return fmt.Errorf("week %d day %d: steps run %s but the session says %d′ — more than 2%% over",
							w.N, di+1, clock(secs), sess.Mins)
					}
				}
				name, err := fitName(w.N, di, sess.Label)
				if err != nil {
					return fmt.Errorf("week %d day %d: steps: %w", w.N, di+1, err)
				}
				fitNames = append(fitNames, fitNamed{
					Day: fmt.Sprintf("week %d day %d", w.N, di+1), Name: name})
				// Encoding here makes "this day produces a valid .fit" a
				// startup invariant. The identity is a pinned placeholder —
				// d.Rev does not exist yet, and the dry run only proves the
				// bytes assemble.
				serial, created := fitIdentity("dryrun", b.ID, b.DayOf(w.N-1, di))
				if _, err := fitWorkoutFor(name, rs, fitSportFor(sess.Kind), serial, created).Encode(); err != nil {
					return fmt.Errorf("week %d day %d: fit: %w", w.N, di+1, err)
				}
				// Bike days also serve a .zwo, so both dialects must assemble
				// at load — including the FTP the fractions divide by.
				if sess.Kind.IsBike() {
					if _, err := zwoFor(name, rs, d.Athlete.Power["ftp"]); err != nil {
						return fmt.Errorf("week %d day %d: zwo: %w", w.N, di+1, err)
					}
				}
			}
			for i, it := range b.Checklist {
				on, err := sc.isTrue(it.When)
				if err != nil {
					return fmt.Errorf("week %d day %d: checklist[%d] %q: %w", w.N, di+1, i, it.Key, err)
				}
				if !on {
					continue
				}
				if _, err := sc.resolve(it.Label); err != nil {
					return fmt.Errorf("week %d day %d: checklist[%d] %q label: %w", w.N, di+1, i, it.Key, err)
				}
				gid, err := sc.resolve(it.Guide)
				if err != nil {
					return fmt.Errorf("week %d day %d: checklist[%d] %q guide: %w", w.N, di+1, i, it.Key, err)
				}
				if gid != "" && b.guides[gid] == nil {
					return fmt.Errorf("week %d day %d: checklist %q points at guide %q, which is not in the library",
						w.N, di+1, it.Key, gid)
				}
			}
		}
	}

	if err := checkFitNameCollisions(fitNames); err != nil {
		return err
	}

	c := b.ctxFor(d.Athlete, 1)

	// The pre-block and post-block days have no session at all. Those pages
	// still render a checklist, so the predicates have to survive a nil one.
	for i, it := range b.Checklist {
		on, err := c.isTrue(it.When)
		if err != nil {
			return fmt.Errorf("outside the block: checklist[%d] %q: %w", i, it.Key, err)
		}
		if !on {
			continue
		}
		if _, err := c.resolve(it.Label); err != nil {
			return fmt.Errorf("outside the block: checklist[%d] %q label: %w", i, it.Key, err)
		}
		if _, err := c.resolve(it.Guide); err != nil {
			return fmt.Errorf("outside the block: checklist[%d] %q guide: %w", i, it.Key, err)
		}
	}

	// An issue's guide is its protocol, reached from the card. A phase that
	// names an issue nobody declared would render its rehab under no heading
	// at all, which is the sort of thing that only shows up in week 10.
	for _, is := range d.Athlete.Issues {
		if is.Guide != "" && b.guides[is.Guide] == nil {
			return fmt.Errorf("issue %q points at guide %q, which is not in the library", is.Key, is.Guide)
		}
	}
	for _, p := range b.Phases {
		if p.Issue != "" && d.Athlete.Issue(p.Issue) == nil {
			return fmt.Errorf("phase %q belongs to issue %q, which athlete.json does not declare", p.Name, p.Issue)
		}
	}

	for gi, grp := range b.Index.Groups {
		for ri, row := range grp.Rows {
			if b.guides[row.Guide] == nil {
				return fmt.Errorf("index groups[%d].rows[%d] names guide %q, which is not in the library", gi, ri, row.Guide)
			}
			if _, err := c.resolve(row.When); err != nil {
				return fmt.Errorf("index groups[%d].rows[%d] when: %w", gi, ri, err)
			}
			if _, err := c.resolve(row.Detail); err != nil {
				return fmt.Errorf("index groups[%d].rows[%d] detail: %w", gi, ri, err)
			}
		}
	}
	if b.Grading != nil {
		// The rubric measures under an anchor the athlete has to carry.
		// Missing, the grade input silently disappears; loudly here beats
		// discovering it from a grade note in November.
		if key := b.Grading.CapKey(); d.Athlete.HR[key] == 0 {
			return fmt.Errorf("grading measures under athlete HR anchor %q, which athlete.json does not declare", key)
		}
		if _, err := c.resolve(b.Grading.Note); err != nil {
			return fmt.Errorf("grading.note: %w", err)
		}
		if _, err := c.resolve(b.Grading.Footer); err != nil {
			return fmt.Errorf("grading.footer: %w", err)
		}
	}
	if _, err := c.resolve(b.Intro); err != nil {
		return fmt.Errorf("intro: %w", err)
	}
	return nil
}

// ctxFor builds the resolution context for a week, clamping out-of-range weeks
// so pre-block and post-block pages still have something coherent to render.
func (b *Block) ctxFor(a *Athlete, week int) *Ctx {
	if week < 1 {
		week = 1
	}
	if week > len(b.Weeks) {
		week = len(b.Weeks)
	}
	w := b.WeekByN(week)
	c := &Ctx{Athlete: a, Block: b, Week: w}
	if w != nil {
		c.Phase = w.Phase()
		c.Meso = w.Mesocycle()
	}
	return c
}

// ResolveGuides is every popup body for a week, fully rendered.
func (b *Block) ResolveGuides(a *Athlete, week int) (map[string]Guide, error) {
	c := b.ctxFor(a, week)
	out := make(map[string]Guide, len(b.guides))
	for id, g := range b.guides {
		rg, err := g.resolve(c)
		if err != nil {
			return nil, fmt.Errorf("guide %s: %w", id, err)
		}
		out[id] = rg
	}
	return out, nil
}

/* ── change detection ──────────────────────────────────────────────────── */

// fingerprint is a cheap stat-only summary of the data directory. The log is
// excluded: it changes on every checkbox and has nothing to do with the plan.
func fingerprint(dir string) string {
	if dir == "" {
		return ""
	}
	h := sha256.New()
	_ = filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return nil //nolint:nilerr // an unreadable entry is a no-change signal, not a crash
		}
		if !isDataFile(e) {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		fmt.Fprintf(h, "%s\x00%d\x00%d\n", filepath.ToSlash(rel), info.Size(), info.ModTime().UnixNano())
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:16]
}
