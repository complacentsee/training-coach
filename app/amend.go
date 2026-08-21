package main

// A reschedule is an event-sourced overlay, never a plan edit. The authored
// block JSON is shipped local→server by push-data and any server-side edit
// to it would silently diverge (and be clobbered by the next push); the log
// is the one server-owned store push-data never touches. So an amendment is
// an entries.jsonl fact — "Thursday's session moved to Saturday, agreed
// 20 Aug" — and every handler serves an EFFECTIVE dataset: the authored one
// with the standing amendments replayed over it, materialised here and
// substituted at s.ds(), the single point every handler already reads.
//
// The load-time guarantee ("a template typo is a startup failure") cannot
// cover entries that arrive at runtime, so it is relocated: an amendment is
// validated whole at POST (the postActivity validate-at-ingest precedent)
// and re-checked at every materialisation — a push-data that invalidates a
// standing amendment VOIDS it loudly rather than failing the build, because
// the log must never take serving hostage.
//
// v1 rules, decided 20 Aug 2026: same-week only; a session may move onto
// a rest day, or a run may take an easy bike day's slot (swap);
// graded/recorded days and down/taper/race weeks are immutable; a
// benchmark tag may be stripped ("plain") — a missed DEC is dead until the
// next cycle.
//
// Structured sessions TRAVEL WITH their steps. The identity law forbids a
// standing serial serving changed bytes, and amendments are invisible to
// the data Rev — but a date that never served a workout may start to, and
// one that stops serving tells no lie, so a session's steps follow it onto
// a rest day or through a run↔bike swap. What stays refused is two
// structured days trading places: both dates' bytes would change under
// unchanged serials. That case waits for fitIdentity to fold amendment
// state in. The proofs dryRun runs at load — the on-watch name set and the
// day's encode — relocate to the POST gate for the slots an op touches.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"
)

// amendOp is one amend entry, decoded: Key is the op, Val the argument.
type amendOp struct {
	Date string // source ISO date
	Op   string // "move" | "swap" | "cancel" | "plain" (| "revoke" at POST)
	Arg  string // destination ISO for move/swap
	Note string
}

func amendOpOf(e Entry) amendOp {
	return amendOp{Date: e.Date, Op: e.Key, Arg: e.Val, Note: e.Note}
}

// amendInfo is what the pages say about an amended date. It lives beside
// the effective block rather than in it, so the block stays a pure plan.
type amendInfo struct {
	Role  string // "vacated" | "landed" | "swapped" | "cancelled" | "plained"
	Other string // the other end's ISO date, "" for cancel/plain
	Label string // the session label that used to sit here, for the ghost
	Note  string
}

// effectiveSet is one materialisation: the dataset handlers actually serve.
// Cached on (data Rev, log Seq) — the only two things that can change it.
type effectiveSet struct {
	rev    string
	seq    int
	d      *dataset
	info   map[string]amendInfo // ISO date → what to say about it
	voided []string             // amendments that no longer apply, for the log
}

// locateISO finds the block, week and day index an ISO date falls in. A
// live block wins over an archived one: an abandoned block may overlap its
// replacement's dates, and an amendment belongs to the block being trained
// — the same preference dataset.Current applies.
func locateISO(blocks []*Block, loc *time.Location, iso string) (bi int, wk *Week, di int, ok bool) {
	t, err := time.ParseInLocation("2006-01-02", iso, loc)
	if err != nil {
		return 0, nil, 0, false
	}
	for pass := 0; pass < 2; pass++ {
		for i, b := range blocks {
			if b.Archived == (pass == 1) {
				if w, d, ok := b.Locate(t); ok {
					return i, w, d, true
				}
			}
		}
	}
	return 0, nil, 0, false
}

// weekImmutable names the tag that makes a week off-limits, or "".
func weekImmutable(wk *Week) string {
	for _, t := range wk.Tags {
		switch t {
		case "down", "taper", "race":
			return t
		}
	}
	return ""
}

// amendCheck is the structural validator, shared by POST and by the
// materialiser: rules that depend only on the plan and the standing set.
// Facts that arrive later (a grade, a recording) are POST-time gates only —
// they refuse a NEW amendment but never void an agreed one. Returns "" when
// the op is applicable, else the refusal reason.
func amendCheck(blocks []*Block, loc *time.Location, taken map[string]amendInfo, op amendOp) string {
	bi, wk, di, ok := locateISO(blocks, loc, op.Date)
	if !ok {
		return "outside any block"
	}
	if _, dup := taken[op.Date]; dup {
		return "already amended"
	}
	if tag := weekImmutable(wk); tag != "" {
		return "the week is tagged " + tag
	}
	src := wk.Days[di]
	if src.Kind == KindRest {
		return "a rest day has nothing to rework"
	}
	switch op.Op {
	case "cancel":
		if op.Arg != "" {
			return "cancel takes no destination"
		}
	case "plain":
		if src.Tag == "" {
			return "no benchmark tag to strip"
		}
	case "move", "swap", "displace":
		dbi, dwk, ddi, ok := locateISO(blocks, loc, op.Arg)
		if !ok {
			return "destination outside any block"
		}
		if op.Arg == op.Date {
			return "destination is the source"
		}
		if dbi != bi || dwk.N != wk.N {
			return "destination is outside the week"
		}
		if _, dup := taken[op.Arg]; dup {
			return "destination already amended"
		}
		dst := dwk.Days[ddi]
		if op.Op == "move" && dst.Kind != KindRest {
			return "destination is not a rest day"
		}
		if op.Op == "swap" && !(src.Kind.IsRun() && dst.Kind == KindBikeEasy) {
			return "a swap is a run taking an easy bike day"
		}
		// Displace is the swap's harder sibling: the run takes the bike
		// day's slot and the bike comes off the week instead of trading
		// places — the primary sport outranks cross-training when both
		// cannot happen.
		if op.Op == "displace" {
			if dst.Kind == KindRest {
				return "the destination is a rest day — move instead"
			}
			if !(src.Kind.IsRun() && dst.Kind == KindBikeEasy) {
				return "a displace is a run taking an easy bike day's slot"
			}
		}
		// The identity rule, precisely: gaining or losing a structured
		// workout is safe, two structured days trading places is not.
		if len(src.Steps) > 0 && len(dst.Steps) > 0 {
			return "both days carry structured workouts — the watch identity cannot follow two trading places"
		}
		// A structured session in a new slot takes a new on-watch name;
		// the block's 15-character dedupe set must stay collision-free.
		replaced := map[int]Session{di: {Kind: KindRest}, ddi: src}
		if op.Op == "swap" {
			replaced[di] = dst
		}
		if reason := amendNameProof(blocks[bi], wk.N, replaced); reason != "" {
			return reason
		}
	default:
		return "unknown op " + strings.TrimSpace(op.Op)
	}
	return ""
}

// amendNameProof re-runs the loader's on-watch name uniqueness check over
// the block as the op would leave it. Relocated from dryRun, not replaced
// by an argument — the check is cheap and the name scheme may change.
func amendNameProof(b *Block, wkN int, replaced map[int]Session) string {
	seen := map[string]string{}
	for _, w := range b.Weeks {
		for di, sess := range w.Days {
			if w.N == wkN {
				if r, ok := replaced[di]; ok {
					sess = r
				}
			}
			if len(sess.Steps) == 0 {
				continue
			}
			name, err := fitName(w.N, di, sess.Label)
			if err != nil {
				return "the moved workout cannot be named for the watch: " + err.Error()
			}
			p := name
			if len(p) > 15 {
				p = p[:15]
			}
			if other, dup := seen[p]; dup {
				return "the moved workout's watch name collides with " + other
			}
			seen[p] = name
		}
	}
	return ""
}

// amendEncodeProof relocates dryRun's per-day encode to the gate: a
// session arriving in a new slot must still assemble a valid .fit (and
// .zwo for a bike) exactly as the loader would have proved at startup.
// Same-week ops resolve in the same variable context, so in practice this
// re-proves what load proved — which is the point: prove, never argue.
func amendEncodeProof(d *dataset, op amendOp) string {
	if op.Op != "move" && op.Op != "swap" && op.Op != "displace" {
		return ""
	}
	bi, wk, di, ok := locateISO(d.Blocks, d.Loc, op.Date)
	_, dwk, ddi, ok2 := locateISO(d.Blocks, d.Loc, op.Arg)
	if !ok || !ok2 {
		return ""
	}
	blk := d.Blocks[bi]
	type placement struct {
		di   int
		sess Session
	}
	var arriving []placement
	if s := wk.Days[di]; len(s.Steps) > 0 {
		arriving = append(arriving, placement{ddi, s})
	}
	if s := dwk.Days[ddi]; op.Op == "swap" && len(s.Steps) > 0 {
		arriving = append(arriving, placement{di, s})
	}
	for _, p := range arriving {
		sess := p.sess
		sc := blk.ctxFor(d.Athlete, wk.N).forSession(&sess)
		rs, err := resolveSteps(sc, sess)
		if err != nil {
			return "the steps no longer resolve in the new slot: " + err.Error()
		}
		name, err := fitName(wk.N, p.di, sess.Label)
		if err != nil {
			return "the moved workout cannot be named for the watch: " + err.Error()
		}
		serial, created := fitIdentity("dryrun", blk.ID, blk.DayOf(wk.N-1, p.di))
		if _, err := fitWorkoutFor(name, rs, fitSportFor(sess.Kind), serial, created).Encode(); err != nil {
			return "the moved workout does not assemble: " + err.Error()
		}
		if sess.Kind.IsBike() {
			if _, err := zwoFor(name, rs, d.Athlete.Power["ftp"]); err != nil {
				return "the moved workout's zwo does not assemble: " + err.Error()
			}
		}
	}
	return ""
}

// cloneBlock deep-copies the parts an amendment mutates — the weeks and
// their day slots — and shares everything else. NOT a JSON round-trip: the
// block carries unexported state (location, merged guides) a round-trip
// would silently drop.
func cloneBlock(b *Block) *Block {
	nb := *b
	nb.Weeks = make([]*Week, len(b.Weeks))
	for i, w := range b.Weeks {
		nw := *w
		nw.Days = append([]Session(nil), w.Days...)
		nw.block = &nb
		nb.Weeks[i] = &nw
	}
	return &nb
}

// buildEffective replays the standing amendments over a copy of the
// dataset. Only touched blocks are cloned; an amendment the current data
// refuses is voided, never fatal.
func buildEffective(d *dataset, entries []Entry) *effectiveSet {
	es := &effectiveSet{d: d, info: map[string]amendInfo{}}
	if len(entries) == 0 {
		return es
	}
	nd := *d
	nd.Blocks = append([]*Block(nil), d.Blocks...)
	cloned := map[int]bool{}
	for _, e := range entries {
		op := amendOpOf(e)
		if reason := amendCheck(nd.Blocks, nd.Loc, es.info, op); reason != "" {
			es.voided = append(es.voided, fmt.Sprintf("%s %s %s: %s", op.Date, op.Op, op.Arg, reason))
			continue
		}
		bi, _, di, _ := locateISO(nd.Blocks, nd.Loc, op.Date)
		if !cloned[bi] {
			nd.Blocks[bi] = cloneBlock(nd.Blocks[bi])
			cloned[bi] = true
		}
		_, wk, _, _ := locateISO(nd.Blocks, nd.Loc, op.Date)
		src := wk.Days[di]
		switch op.Op {
		case "move":
			_, dwk, ddi, _ := locateISO(nd.Blocks, nd.Loc, op.Arg)
			dwk.Days[ddi] = src
			wk.Days[di] = Session{Kind: KindRest, Label: "Rest"}
			es.info[op.Date] = amendInfo{Role: "vacated", Other: op.Arg, Label: src.Label, Note: op.Note}
			es.info[op.Arg] = amendInfo{Role: "landed", Other: op.Date, Note: op.Note}
		case "swap":
			_, dwk, ddi, _ := locateISO(nd.Blocks, nd.Loc, op.Arg)
			dst := dwk.Days[ddi]
			wk.Days[di], dwk.Days[ddi] = dst, src
			es.info[op.Date] = amendInfo{Role: "swapped", Other: op.Arg, Label: src.Label, Note: op.Note}
			es.info[op.Arg] = amendInfo{Role: "swapped", Other: op.Date, Label: dst.Label, Note: op.Note}
		case "displace":
			_, dwk, ddi, _ := locateISO(nd.Blocks, nd.Loc, op.Arg)
			dropped := dwk.Days[ddi]
			dwk.Days[ddi] = src
			wk.Days[di] = Session{Kind: KindRest, Label: "Rest"}
			es.info[op.Date] = amendInfo{Role: "vacated", Other: op.Arg, Label: src.Label, Note: op.Note}
			// The landed side remembers what it cost: the dropped session's
			// label rides in Label, and the provenance line says so.
			es.info[op.Arg] = amendInfo{Role: "landed", Other: op.Date, Label: dropped.Label, Note: op.Note}
		case "cancel":
			wk.Days[di] = Session{Kind: KindRest, Label: "Rest"}
			es.info[op.Date] = amendInfo{Role: "cancelled", Label: src.Label, Note: op.Note}
		case "plain":
			src.Tag = ""
			wk.Days[di] = src
			es.info[op.Date] = amendInfo{Role: "plained", Note: op.Note}
		}
	}
	es.d = &nd
	return es
}

// effective returns the cached materialisation for the current (Rev, Seq),
// rebuilding when either moved. Voidings are logged once per rebuild — a
// build happens only when the data or the log changed.
func (s *server) effective() *effectiveSet {
	d := s.data.Load()
	if d == nil || s.store == nil {
		return &effectiveSet{d: d, info: map[string]amendInfo{}}
	}
	seq := s.store.Seq()
	if e := s.eff.Load(); e != nil && e.rev == d.Rev && e.seq == seq {
		return e
	}
	s.effMu.Lock()
	defer s.effMu.Unlock()
	if e := s.eff.Load(); e != nil && e.rev == d.Rev && e.seq == seq {
		return e
	}
	e := buildEffective(d, s.store.Amendments())
	e.rev, e.seq = d.Rev, seq
	// Voidings log when the SET changes, not on every rebuild — a rebuild
	// happens on every log append, and a line repeated on each checkoff
	// buries the one occurrence that mattered. The pages carry the banner.
	if prev := s.eff.Load(); prev == nil || !slices.Equal(prev.voided, e.voided) {
		for _, v := range e.voided {
			log.Printf("amend: VOIDED, serving the authored day instead: %s", v)
		}
	}
	s.eff.Store(e)
	return e
}

// amendInfoFor is what the pages ask: is this date amended, and what to say.
func (s *server) amendInfoFor(iso string) (amendInfo, bool) {
	e := s.effective()
	i, ok := e.info[iso]
	return i, ok
}

// amendDayName renders an ISO date as the short form the provenance lines
// use: "Thu 20".
func amendDayName(iso string, loc *time.Location) string {
	t, err := time.ParseInLocation("2006-01-02", iso, loc)
	if err != nil {
		return iso
	}
	return t.Format("Mon 2")
}

// amendLine is the one-sentence provenance a card or popover shows.
func amendLine(info amendInfo, loc *time.Location) string {
	suffix := ""
	if info.Note != "" {
		suffix = " — " + info.Note
	}
	switch info.Role {
	case "vacated":
		return "Moved to " + amendDayName(info.Other, loc) + suffix
	case "landed":
		if info.Label != "" {
			return "Moved from " + amendDayName(info.Other, loc) +
				", dropping " + stripEmph(info.Label) + suffix
		}
		return "Moved from " + amendDayName(info.Other, loc) + suffix
	case "swapped":
		return "Swapped with " + amendDayName(info.Other, loc) + suffix
	case "cancelled":
		return "Dropped" + suffix
	case "plained":
		return "Run plain — the measurement waits for the next cycle" + suffix
	}
	return ""
}

/* ── the HTTP surface ─────────────────────────────────────────────────── */

const amendNoteMax = 2000

// postAmend is POST /api/amend {date, op, arg, note}: the apply gate. The
// whole op is validated against the authored plan plus the standing set
// before anything is appended — refusal writes nothing, the postActivity
// law. Facts gate here and only here: a graded or recorded day refuses a
// NEW amendment, but never voids a standing one.
func (s *server) postAmend(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Date string `json:"date"`
		Op   string `json:"op"`
		Arg  string `json:"arg"`
		Note string `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Note) > amendNoteMax {
		http.Error(w, "note too long", http.StatusRequestEntityTooLarge)
		return
	}
	d := s.data.Load()
	if d == nil {
		http.Error(w, "no data loaded", http.StatusServiceUnavailable)
		return
	}
	e := s.effective()

	if req.Op == "revoke" {
		// Resolve to the standing ENTRY, not a role: either end of a move
		// or a swap names it, and the revoke must land on the entry's own
		// source date or Amendments()' last-write-wins never sees it.
		var target *Entry
		for _, a := range s.store.Amendments() {
			if a.Date == req.Date || (a.Val != "" && a.Val == req.Date) {
				a := a
				target = &a
				break
			}
		}
		if target == nil {
			http.Error(w, "nothing to revoke on "+req.Date, http.StatusBadRequest)
			return
		}
		// The record gates a revoke exactly as it gates an amendment: once
		// either end carries a grade or a recording, un-moving the session
		// would strand that fact against a day claiming something else.
		grades := s.store.Grades()
		for _, iso := range []string{target.Date, target.Val} {
			if iso == "" {
				continue
			}
			if _, graded := grades[iso]; graded {
				http.Error(w, iso+" is graded — the record does not move", http.StatusBadRequest)
				return
			}
			if s.anyRecorded(iso) {
				http.Error(w, iso+" has a recording — the record does not move", http.StatusBadRequest)
				return
			}
		}
		if err := s.store.Append(Entry{Kind: kindAmend, Date: target.Date, Key: "revoke", Note: req.Note}); err != nil {
			http.Error(w, "append: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if _, still := s.amendInfoFor(target.Date); still {
			http.Error(w, "appended but still standing — check the server log", http.StatusInternalServerError)
			return
		}
		s.jsonOK(w)
		return
	}

	op := amendOp{Date: req.Date, Op: req.Op, Arg: req.Arg, Note: req.Note}
	if reason := amendCheck(d.Blocks, d.Loc, e.info, op); reason != "" {
		http.Error(w, reason, http.StatusBadRequest)
		return
	}
	if reason := amendEncodeProof(d, op); reason != "" {
		http.Error(w, reason, http.StatusBadRequest)
		return
	}
	// Only the block being trained amends: an archived block's calendar is
	// a record. A standing amendment survives its block archiving — the
	// gate is on posting, the way graded/recorded gates are.
	if bi, _, _, ok := locateISO(d.Blocks, d.Loc, op.Date); ok && !d.IsCurrent(d.Blocks[bi], s.day(d)) {
		http.Error(w, "not the current block", http.StatusBadRequest)
		return
	}
	// POST-only gates: the record is immutable, and work never moves into
	// the past.
	today := s.day(d).Format("2006-01-02")
	grades := s.store.Grades()
	for _, iso := range []string{op.Date, op.Arg} {
		if iso == "" {
			continue
		}
		if _, graded := grades[iso]; graded {
			http.Error(w, iso+" is graded — the record does not move", http.StatusBadRequest)
			return
		}
		if s.anyRecorded(iso) {
			http.Error(w, iso+" has a recording — the record does not move", http.StatusBadRequest)
			return
		}
	}
	if (op.Op == "move" || op.Op == "swap" || op.Op == "displace") && op.Arg < today {
		http.Error(w, "the destination is in the past", http.StatusBadRequest)
		return
	}

	if err := s.store.Append(Entry{Kind: kindAmend, Date: op.Date, Key: op.Op, Val: op.Arg, Note: op.Note}); err != nil {
		http.Error(w, "append: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Read back: the amendment must be standing in the fresh materialisation
	// or the caller must not be told it applied.
	if _, ok := s.amendInfoFor(op.Date); !ok {
		http.Error(w, "appended but not standing — check the server log", http.StatusInternalServerError)
		return
	}
	s.jsonOK(w)
}

func (s *server) jsonOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"ok":true}`)
}

// reworkCandidate is one thing the athlete could do about a session.
type reworkCandidate struct {
	Op     string `json:"op"` // "" for the absorb row: informational, no write
	Arg    string `json:"arg,omitempty"`
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
}

// getRework is GET /api/rework?date=: the deterministic candidate list, no
// model anywhere. Every number and label is server-resolved; the client
// renders strings.
func (s *server) getRework(w http.ResponseWriter, r *http.Request) {
	iso := r.URL.Query().Get("date")
	d := s.data.Load()
	if d == nil {
		http.Error(w, "no data loaded", http.StatusServiceUnavailable)
		return
	}
	e := s.effective()
	out := struct {
		Date       string            `json:"date"`
		Day        string            `json:"day"`
		Label      string            `json:"label"`
		Amt        string            `json:"amt"`
		Can        bool              `json:"can"`
		Reason     string            `json:"reason,omitempty"`
		Standing   string            `json:"standing,omitempty"`
		SkipNote   string            `json:"skip_note,omitempty"`
		Candidates []reworkCandidate `json:"candidates,omitempty"`
	}{Date: iso, Day: amendDayName(iso, d.Loc)}

	if info, standing := e.info[iso]; standing {
		out.Standing = amendLine(info, d.Loc)
		out.Can = true // the only offered action is revoke, client-side
		s.writeJSON(w, out)
		return
	}

	bi, wk, di, ok := locateISO(d.Blocks, d.Loc, iso)
	if !ok {
		http.Error(w, "outside any block", http.StatusNotFound)
		return
	}
	src := wk.Days[di]
	out.Label = src.Label
	out.Amt = src.Amount(s.units())
	if sk, skipped := s.store.SkipOn(iso); skipped {
		out.SkipNote = sk.Note
	}

	// Probe with a self-move to surface the source-side refusals in one
	// place; the op itself is never valid (destination is the source).
	probe := amendCheck(d.Blocks, d.Loc, e.info, amendOp{Date: iso, Op: "cancel"})
	grades := s.store.Grades()
	if _, graded := grades[iso]; graded {
		probe = "already graded — the record does not move"
	} else if s.anyRecorded(iso) {
		probe = "a recording exists — the record does not move"
	}
	if probe != "" {
		out.Can, out.Reason = false, probe
		s.writeJSON(w, out)
		return
	}
	out.Can = true

	today := s.day(d).Format("2006-01-02")
	blk := d.Blocks[bi]
	for i, ds := range wk.Days {
		if i == di {
			continue
		}
		dISO := blk.DayOf(wk.N-1, i).Format("2006-01-02")
		if dISO < today {
			continue
		}
		if _, taken := e.info[dISO]; taken {
			continue
		}
		if _, graded := grades[dISO]; graded {
			continue
		}
		if s.anyRecorded(dISO) {
			continue
		}
		day := amendDayName(dISO, d.Loc)
		var cands []reworkCandidate
		switch {
		case ds.Kind == KindRest:
			cands = append(cands, reworkCandidate{
				Op: "move", Arg: dISO,
				Title:  "Move to " + day,
				Detail: "a rest day — nothing is displaced",
			})
		case src.Kind.IsRun() && ds.Kind == KindBikeEasy:
			// Both readings of run-outranks-bike, side by side: trade
			// places, or take the slot and let the bike go.
			cands = append(cands,
				reworkCandidate{
					Op: "swap", Arg: dISO,
					Title:  "Swap with " + day + "'s bike",
					Detail: stripEmph(ds.Label) + " takes " + out.Day + " instead",
				},
				reworkCandidate{
					Op: "displace", Arg: dISO,
					Title:  "Move to " + day + ", dropping its bike",
					Detail: stripEmph(ds.Label) + " comes off the week; " + out.Day + " becomes rest",
				})
		default:
			continue
		}
		// Offered only where the gate would accept it — the candidate list
		// probes the real validator rather than restating its rules — and a
		// candidate that would take tracked rehab work off the week says so
		// by name.
		for _, cand := range cands {
			if amendCheck(d.Blocks, d.Loc, e.info, amendOp{Date: iso, Op: cand.Op, Arg: cand.Arg}) != "" {
				continue
			}
			replaced := map[int]Session{di: {Kind: KindRest}, i: src}
			if cand.Op == "swap" {
				replaced[di] = ds
			}
			if loss := s.trackedLoss(d, blk, wk, replaced); len(loss) > 0 {
				cand.Detail += "; also drops " + strings.Join(loss, ", ")
			}
			out.Candidates = append(out.Candidates, cand)
		}
	}
	if src.Tag != "" {
		out.Candidates = append(out.Candidates, reworkCandidate{
			Op:     "plain",
			Title:  "Run it plain",
			Detail: "the " + src.Tag + " measurement waits for its next cycle",
		})
	}
	cancelDetail := "the day becomes rest; the week's volume drops by " + src.Amount(s.units())
	if loss := s.trackedLoss(d, blk, wk, map[int]Session{di: {Kind: KindRest}}); len(loss) > 0 {
		cancelDetail += "; also drops " + strings.Join(loss, ", ")
	}
	out.Candidates = append(out.Candidates,
		reworkCandidate{Op: "cancel", Title: "Drop it", Detail: cancelDetail},
		reworkCandidate{Title: "Absorb it — change nothing",
			Detail: "the plan stands; a skip already tells the story"})
	s.writeJSON(w, out)
}

func (s *server) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("rework: encode: %v", err)
	}
}
