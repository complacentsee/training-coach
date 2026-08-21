package main

// A missed strength session is a checklist fact, not a session fact: the
// strength tasks ride session kinds via the checklist's `when` templates,
// so they follow a moved session automatically — and vanish silently when
// an amendment removes their kind from the week. Both halves of that need
// the same ledger: which tracked tasks a week offered, and which were
// logged. An issue DECLARES which tasks carry its rehab (Issue.Tracks);
// nothing here hardcodes "strength" or "calf" — the same lesson as the
// injury that used to be one hardcoded calf.
//
// The assessment states facts and quotes the data — counts, the issue's
// own phase line, its own current band — never an invented threshold. A
// day is "not logged", not "missed": timestamps record ticking, not doing,
// and he logs in batches. Tone styles the text; the words carry it.

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
)

// trackDay is one offering of a tracked task: a day the checklist showed
// it, and whether it has been logged.
type trackDay struct {
	Date  string `json:"date"`
	Day   string `json:"day"` // "Tue 18"
	Key   string `json:"key"`
	Label string `json:"label"`
	Guide string `json:"guide,omitempty"` // the popup that says what the work is
	Done  bool   `json:"done"`
	Past  bool   `json:"past"` // strictly before today: old enough to read
	Today bool   `json:"today"`
}

// trackWeek replays one week's checklist for an issue's tracked tasks,
// against whatever block it is given — handlers pass the effective one, so
// a moved easy day moves its Strength B offering with it.
func (s *server) trackWeek(d *dataset, blk *Block, is *Issue, wk *Week, todayISO string) []trackDay {
	if len(is.Tracks) == 0 {
		return nil
	}
	tracked := map[string]bool{}
	for _, k := range is.Tracks {
		tracked[k] = true
	}
	var out []trackDay
	for di := range wk.Days {
		sess := wk.Days[di]
		date := blk.DayOf(wk.N-1, di)
		iso := date.Format("2006-01-02")
		ctx := blk.ctxFor(d.Athlete, wk.N)
		ctx.InBlock = true
		ctx.Session = &sess
		done := s.store.TasksFor(iso)
		for _, t := range checklist(ctx, blk) {
			if !tracked[t.key] {
				continue
			}
			out = append(out, trackDay{
				Date: iso, Day: date.Format("Mon 2"), Key: t.key,
				Label: stripEmph(t.label), Guide: t.guide,
				Done: done[t.key], Past: iso < todayISO, Today: iso == todayISO,
			})
		}
	}
	return out
}

// trackView is the assessment a card renders. Quiet when nothing needs
// saying: it exists only while a past offering is unlogged.
type trackView struct {
	Lines []string `json:"lines"`
	Tone  string   `json:"tone"` // caution, or stop when the issue's own band says stop
}

func (s *server) trackAssessment(d *dataset, blk *Block, is *Issue, wk *Week, todayISO string) *trackView {
	days := s.trackWeek(d, blk, is, wk, todayISO)
	if len(days) == 0 {
		return nil
	}
	var missed, remaining []trackDay
	var done int
	for _, td := range days {
		switch {
		case td.Done:
			done++
		case td.Past:
			missed = append(missed, td)
		default:
			remaining = append(remaining, td)
		}
	}
	if len(missed) == 0 {
		return nil
	}
	v := &trackView{Tone: "caution"}
	for _, m := range missed {
		v.Lines = append(v.Lines, m.Label+" ("+m.Day+") not logged")
	}
	line := fmt.Sprintf("This week: %d of %d done", done, len(days))
	switch {
	case len(remaining) == 0:
		line += " · no slots left"
	case len(remaining) == 1:
		line += " · 1 slot left: " + remaining[0].Label + slotWhen(remaining[0])
	default:
		line += fmt.Sprintf(" · %d slots left, next: %s%s", len(remaining), remaining[0].Label, slotWhen(remaining[0]))
	}
	v.Lines = append(v.Lines, line)
	// The phase is NOT restated here: the card already wears its phase line,
	// and the API carries it as its own field for readers without the card.
	if rs := s.store.Ratings(is.Key); len(rs) > 0 {
		last := rs[len(rs)-1]
		if n, err := strconv.Atoi(last.Val); err == nil {
			if b := is.BandFor(n); b != nil {
				v.Lines = append(v.Lines, is.Heading()+" last rated "+last.Val+" · "+b.Label)
				if b.Tone == "stop" {
					v.Tone = "stop"
				}
			}
		}
	}
	return v
}

// missedItem is one unlogged past thing, as its own card row: the work by
// name, when it was planned or offered, the guide that says what it is,
// and the key/date a late tick posts against. Session rows also carry the
// rework trigger — a past-due session's other honest exit.
type missedItem struct {
	Date   string
	Day    string
	Key    string
	Label  string
	Guide  string
	Meta   string // "planned Tue 18" / "offered Tue 18 · Calf"
	Rework bool
}

// missedView is the today page's "Not logged" card: rows the athlete can
// act on — open the plan, log the work done late, or rework the day —
// plus one summary line per issue with tracked work outstanding. Nil when
// nothing needs saying.
type missedView struct {
	Items []missedItem
	Lines []string
	Tone  string
}

func (s *server) missedWork(d *dataset, blk *Block, wk *Week, todayISO string) *missedView {
	var v missedView
	v.Tone = "caution"

	// Past-due sessions: a planned day with no evidence at all — no grade,
	// no recording of its sport, no skip, no session checkoff, and not
	// amended away (a vacated day is rest in the effective week this reads).
	// "session" is the checklist's own key for the day's session row, the
	// same one the checkbox posts — a literal by convention, like "week" on
	// a week grade.
	grades := s.store.Grades()
	for di := range wk.Days {
		sess := wk.Days[di]
		date := blk.DayOf(wk.N-1, di)
		iso := date.Format("2006-01-02")
		if iso >= todayISO || sess.Kind == KindRest {
			continue
		}
		if _, graded := grades[iso]; graded {
			continue
		}
		if _, skipped := s.store.SkipOn(iso); skipped {
			continue
		}
		if s.store.TasksFor(iso)["session"] {
			continue
		}
		if s.sessionRecorded(iso, sess.Kind) {
			continue
		}
		day := date.Format("Mon 2")
		v.Items = append(v.Items, missedItem{
			Date: iso, Day: day, Key: "session",
			Label: stripEmph(sess.Label), Guide: sess.GuideID(),
			Meta: "planned " + day, Rework: true,
		})
	}

	for ii := range d.Athlete.Issues {
		is := &d.Athlete.Issues[ii]
		days := s.trackWeek(d, blk, is, wk, todayISO)
		var missed int
		var done int
		var rem []trackDay
		for _, td := range days {
			switch {
			case td.Done:
				done++
			case td.Past:
				missed++
				v.Items = append(v.Items, missedItem{
					Date: td.Date, Day: td.Day, Key: td.Key,
					Label: td.Label, Guide: td.Guide,
					Meta: "offered " + td.Day + " · " + is.Heading(),
				})
			default:
				rem = append(rem, td)
			}
		}
		if missed == 0 {
			continue
		}
		// The slot is named, not counted: "1 slot left" answers nothing —
		// WHEN it is and WHAT it is are the whole question. The rating is
		// not restated here; its card is directly above.
		line := fmt.Sprintf("%s: %d of %d done this week", is.Heading(), done, len(days))
		switch {
		case len(rem) == 0:
			line += " · no slots left"
		case len(rem) == 1:
			line += " · 1 slot left: " + rem[0].Label + slotWhen(rem[0])
		default:
			line += fmt.Sprintf(" · %d slots left, next: %s%s", len(rem), rem[0].Label, slotWhen(rem[0]))
		}
		v.Lines = append(v.Lines, line)
	}
	if len(v.Items) == 0 {
		return nil
	}
	sort.Slice(v.Items, func(i, j int) bool {
		if v.Items[i].Date != v.Items[j].Date {
			return v.Items[i].Date < v.Items[j].Date
		}
		return v.Items[i].Key < v.Items[j].Key
	})
	return &v
}

// slotWhen names a coming slot's day, with "today" earning its word.
func slotWhen(td trackDay) string {
	if td.Today {
		return " (today)"
	}
	return " (" + td.Day + ")"
}

// trackOffered reports whether a session's day would offer the tracked
// task, and the label it wears there.
func trackOffered(d *dataset, blk *Block, wk *Week, sess Session, key string) (string, bool) {
	ctx := blk.ctxFor(d.Athlete, wk.N)
	ctx.InBlock = true
	s := sess
	ctx.Session = &s
	for _, t := range checklist(ctx, blk) {
		if t.key == key {
			return stripEmph(t.label), true
		}
	}
	return "", false
}

// trackedLoss names the tracked tasks an op would remove from its week
// entirely — the quality day dying takes Strength A with it, and whoever
// decides that should see the cost by name. A task that merely follows its
// session to another day is not a loss and says nothing here.
func (s *server) trackedLoss(d *dataset, blk *Block, wk *Week, replaced map[int]Session) []string {
	var out []string
	for ii := range d.Athlete.Issues {
		is := &d.Athlete.Issues[ii]
		for _, key := range is.Tracks {
			pre, post := 0, 0
			var label string
			for di := range wk.Days {
				preSess := wk.Days[di]
				postSess := preSess
				if r, ok := replaced[di]; ok {
					postSess = r
				}
				if l, on := trackOffered(d, blk, wk, preSess, key); on {
					pre++
					if label == "" {
						label = l
					}
				}
				if _, on := trackOffered(d, blk, wk, postSess, key); on {
					post++
				}
			}
			if pre > 0 && post == 0 {
				out = append(out, label+" ("+is.Heading()+" tracks it)")
			}
		}
	}
	return out
}

// getIssueAdherence is GET /api/issue-adherence?key=: the card's ledger
// and assessment as data, for the rework skill and a future chat to read
// rather than re-derive.
func (s *server) getIssueAdherence(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	e := s.effective()
	d := e.d
	if d == nil {
		http.Error(w, "no data loaded", http.StatusServiceUnavailable)
		return
	}
	var is *Issue
	for i := range d.Athlete.Issues {
		if d.Athlete.Issues[i].Key == key {
			is = &d.Athlete.Issues[i]
		}
	}
	if is == nil {
		http.Error(w, "no issue "+strconv.Quote(key), http.StatusNotFound)
		return
	}
	day := s.day(d)
	blk := d.Current(day)
	if blk == nil {
		http.Error(w, "no training block loaded", http.StatusServiceUnavailable)
		return
	}
	wk, _, ok := blk.Locate(day)
	if !ok {
		http.Error(w, "today is outside the block", http.StatusNotFound)
		return
	}
	iso := day.Format("2006-01-02")
	phase := ""
	if p := blk.PhaseForIssue(is.Key, wk.N); p != nil {
		phase = p.Name + " — " + stripEmph(p.Detail)
	}
	s.writeJSON(w, struct {
		Issue      string     `json:"issue"`
		Week       int        `json:"week"`
		Phase      string     `json:"phase,omitempty"`
		Days       []trackDay `json:"days"`
		Assessment *trackView `json:"assessment,omitempty"`
	}{is.Key, wk.N, phase, s.trackWeek(d, blk, is, wk, iso), s.trackAssessment(d, blk, is, wk, iso)})
}
