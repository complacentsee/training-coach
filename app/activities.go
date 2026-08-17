package main

// Recorded activity files pulled off the watch land here as opaque bytes under
// <data>/activities/. They are personal health data with the same standing as
// the entries log: server-only, never overwritten, never renamed. The stored
// bytes are canonical and immutable — since 14 Aug 2026 the binary also
// DECODES them (decode.go) into the derived, disposable metrics cache, but
// storage itself still checks the .FIT magic and nothing else, and nothing
// derived can touch the bytes. The directory is invisible to the plan: the
// data Rev hashes only the files loadDataset takes, and fingerprint() counts
// only non-hidden .json files, so neither an activity nor a stranded .tmp can
// perturb a reload or fail `make verify`.

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// activityMaxBytes caps an upload. A recorded run is tens of kilobytes; a
// multi-hour ride with every sensor logging stays under a few megabytes, so
// sixteen is headroom, not a target.
const activityMaxBytes = 16 << 20

// validActivityName admits exactly the names the store will hold: at most 100
// characters, a leading alphanumeric, the rest drawn from [A-Za-z0-9._-],
// ending ".fit" in any case, and never "..". The charset excludes separators
// and a leading dot, so a valid name can neither escape the directory nor
// collide with the deploy pipeline's junk scan for "._*" and ".DS_Store".
func validActivityName(name string) bool {
	if len(name) == 0 || len(name) > 100 ||
		strings.Contains(name, "..") ||
		!strings.HasSuffix(strings.ToLower(name), ".fit") {
		return false
	}
	for i := 0; i < len(name); i++ {
		switch c := name[i]; {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case i > 0 && (c == '.' || c == '_' || c == '-'):
		default:
			return false
		}
	}
	return true
}

// The FIT container's two legal header sizes: 12 bytes, or 14 with the
// header's own CRC in the last two. Both put ".FIT" at 8–11, which is why
// the magic check never had to care which it was reading.
const (
	fitHeaderMin = 12
	fitHeaderMax = 14
)

// fitFramingErr reports why body is not a well-framed FIT container, or nil
// when it is. The header states how many data bytes follow it, so the whole
// file's length is arithmetic: header + data + the two-byte trailing CRC. A
// file that stops short of what its own header promised is a torn read.
//
// This is an INGEST gate, not part of the measurement register — it lives
// here rather than in decode.go deliberately, so it carries no python-mirror
// obligation and no acceptance-gate run. It computes nothing about a
// workout; it reads the container's own arithmetic back to it.
//
// Why it exists now: under MTP the transport was transactional and the page
// compared what it received against the device's own GetObjectInfo, so a
// short read had a second opinion. Under mass storage both numbers come from
// one host read of one FAT directory entry — the comparison becomes
// self-consistent by construction and proves nothing. The oracle has to come
// from inside the bytes.
//
// Chained files (several FIT parts back to back) are walked in full, the same
// shape decode.go walks: every part must be whole and the last must end
// exactly at the final byte, so "header + data + CRC == len(body)" is the
// single-part case of one rule rather than a special one.
func fitFramingErr(body []byte) error {
	if len(body) == 0 {
		return errors.New("empty body")
	}
	for o, part := 0, 1; o < len(body); part++ {
		left := len(body) - o
		if left < fitHeaderMin+2 {
			return fmt.Errorf("part %d: %d bytes left, too short for a header and a CRC", part, left)
		}
		// The magic is asked first on purpose: it is the question "is this a
		// FIT file at all", and a caller who sent something else entirely
		// should hear that rather than a complaint about byte 0 as a header
		// size. Both legal header sizes put the magic in the same place.
		if string(body[o+8:o+12]) != ".FIT" {
			return fmt.Errorf("part %d: no .FIT magic at offset %d", part, o+8)
		}
		hs := int(body[o])
		if hs != fitHeaderMin && hs != fitHeaderMax {
			return fmt.Errorf("part %d: header size %d, want %d or %d", part, hs, fitHeaderMin, fitHeaderMax)
		}
		if left < hs+2 {
			return fmt.Errorf("part %d: %d bytes left, too short for a %d-byte header and a CRC", part, left, hs)
		}
		dsize := int(binary.LittleEndian.Uint32(body[o+4 : o+8]))
		end := o + hs + dsize + 2 // header, data, trailing CRC
		if end > len(body) {
			return fmt.Errorf("part %d: header declares %d data bytes, so the file needs %d and holds %d — truncated by %d",
				part, dsize, end, len(body), end-len(body))
		}
		o = end
	}
	return nil
}

func (s *server) activitiesDir() string {
	return filepath.Join(s.dataDir, "activities")
}

type activityInfo struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	// Sport is filled only for a ?date= listing, and only from the metrics
	// row: an unimported file has none, and guessing one from the bytes here
	// would be a second decode to answer a question the import already did.
	Sport string `json:"sport,omitempty"`
	// Dist and Elapsed come with a ?date= listing so a day of several
	// recordings can be told apart. Sport alone cannot do it: on 51 of this
	// archive's multi-recording days every file is the same sport, which is
	// exactly the warm-up / effort / cool-down shape, and three buttons all
	// reading "running" name nothing.
	Dist    string `json:"dist,omitempty"`
	Elapsed string `json:"elapsed_hms,omitempty"`
}

// isoDatePattern is the shape of a training day everywhere in this app.
var isoDatePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// getActivities lists the stored files, newest first — device names are
// timestamps, so descending name order is descending time. A store that does
// not exist yet is an empty list, never an error and never null.
//
// ?date=YYYY-MM-DD narrows it to one training day and names each file's
// sport, which is what lets a page pick the recording matching the day's
// session out of the 121 archive dates that carry more than one. The
// unfiltered listing is byte-for-byte what it always was — the watch page
// reads it — because a filter nobody asked for is a different endpoint.
func (s *server) getActivities(w http.ResponseWriter, r *http.Request) {
	date := r.URL.Query().Get("date")
	if date != "" && !isoDatePattern.MatchString(date) {
		http.Error(w, "date must be YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	// The training day is the DB's, not the filename's — they agree on this
	// archive, but the import owns that judgement and a page must not make
	// it a second time. A file whose import failed has no row, so the name
	// prefix is the fallback and such a day still lists its recording.
	var sports map[string]string
	var sizes map[string]activityInfo
	if date != "" && s.metrics != nil {
		if rows, err := s.metrics.byDate(date); err != nil {
			log.Printf("activities %s: %v", date, err)
		} else {
			u := s.ds().Athlete.Units
			sports, sizes = map[string]string{}, map[string]activityInfo{}
			for _, a := range rows {
				sports[a.Name] = a.Sport
				i := activityInfo{Elapsed: hms(float64(a.ElapsedS))}
				if a.DistanceM != nil && *a.DistanceM > 0 {
					i.Dist = Distance(*a.DistanceM).InMeasured(u)
				}
				sizes[a.Name] = i
			}
		}
	}

	ents, err := os.ReadDir(s.activitiesDir())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("activities: %v", err)
		http.Error(w, "could not list", http.StatusInternalServerError)
		return
	}
	list := []activityInfo{} // a slice, not nil: an empty store must encode as []
	for _, e := range ents {
		if e.IsDir() || !validActivityName(e.Name()) {
			continue // an in-flight .tmp is not a stored activity
		}
		if date != "" {
			if _, known := sports[e.Name()]; !known && !strings.HasPrefix(e.Name(), date) {
				continue
			}
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		row := activityInfo{Name: e.Name(), Size: info.Size(), Sport: sports[e.Name()]}
		row.Dist, row.Elapsed = sizes[e.Name()].Dist, sizes[e.Name()].Elapsed
		list = append(list, row)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name > list[j].Name })
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(list)
}

// postActivity stores one upload, atomically and at most once. The stat is
// only the polite fast path; publishActivity's hard link is what actually
// holds under a concurrent duplicate.
func (s *server) postActivity(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if !validActivityName(name) {
		http.Error(w, "name must be a plain .fit filename", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, activityMaxBytes))
	if err != nil {
		var over *http.MaxBytesError
		if errors.As(err, &over) {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
		return
	}
	// One gate, not two: the framing walk asks the magic question first and
	// then the one the old check could not — whether all the bytes arrived.
	// Its message names which, because "not a FIT file" and "truncated by 44
	// bytes" want different things done about them.
	if err := fitFramingErr(body); err != nil {
		http.Error(w, "not a well-framed FIT file: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Framing catches a short read; only the CRC catches a wrong one. A
	// flipped byte inside the data leaves every length in the file
	// self-consistent, and this archive is append-only — a damaged recording
	// admitted once is damaged forever. wholeFileFITCRCOK is the decoder's
	// own check, both conventions, unchanged: 485 of the live archive's 1,369
	// files match only the whole-part one, so accepting a single convention
	// here would refuse a third of what the watch and Zwift actually write.
	if !wholeFileFITCRCOK(body) {
		http.Error(w, "FIT CRC mismatch — the file is damaged or was read short", http.StatusBadRequest)
		return
	}
	dir := s.activitiesDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("activity %s: %v", name, err)
		http.Error(w, "could not store", http.StatusInternalServerError)
		return
	}
	if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
		s.retryFailedImport(name)
		http.Error(w, "already stored", http.StatusConflict)
		return
	}
	// The same recording under a second name. The device filename is still
	// the identity — this is a second gate, never a replacement — but a
	// transport that does not name files the way the Epix does can offer the
	// same bytes twice without the name rule noticing, and the archive is
	// append-only, so a double admission is permanent.
	//
	// A derived cache may not refuse a real recording: a DB error here logs
	// and lets the upload through, and a file whose import failed carries no
	// row for this to find.
	sum := sha256.Sum256(body)
	if other, err := s.metrics.nameForSHA256(hex.EncodeToString(sum[:]), name); err != nil {
		log.Printf("activity %s: dedupe: %v", name, err)
	} else if other != "" {
		http.Error(w, "already archived as "+other, http.StatusConflict)
		return
	}
	if err := publishActivity(dir, name, body); err != nil {
		if errors.Is(err, fs.ErrExist) {
			s.retryFailedImport(name)
			http.Error(w, "already stored", http.StatusConflict)
			return
		}
		log.Printf("activity %s: %v", name, err)
		http.Error(w, "could not store", http.StatusInternalServerError)
		return
	}
	// Bytes on disk is the contract; metrics are derived. A decode or DB
	// error lands in the failures table and the log, never in this response
	// — the import already succeeded.
	if m, err := s.metrics.importOne(name, body, s.ds().Loc, s.weather); err != nil {
		log.Printf("metrics %s: %v", name, err)
	} else if s.grader != nil {
		// A fresh import is the grading trigger; the grade never blocks
		// the import, and every skip rule lives with the grader.
		//
		// ?now=1 says this is the LAST file of its training day in this
		// transfer, so there is nothing left to wait for. Only the sender
		// knows that — the server sees one POST at a time and cannot tell a
		// finished day from a pause — which is why the settle window exists
		// at all and why a sender that does know gets to skip it.
		if q := r.URL.Query().Get("now"); q == "1" || q == "true" {
			go s.grader.gradeDay(m, 0, false, "")
		} else {
			go s.grader.maybeGrade(m)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// retryFailedImport gives a stored-but-unmeasured file another decode when
// its name is re-POSTed. The bytes on disk are canonical, so the retry reads
// them rather than trusting the request body; without this, a transient
// import failure (full disk, killed txn) was unrecoverable over HTTP until
// the next restart's reconcile, because the store correctly 409s the name.
func (s *server) retryFailedImport(name string) {
	if msg, _ := s.metrics.failureFor(name); msg == "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(s.activitiesDir(), name))
	if err != nil {
		log.Printf("metrics retry %s: %v", name, err)
		return
	}
	if m, err := s.metrics.importOne(name, data, s.ds().Loc, s.weather); err != nil {
		log.Printf("metrics retry %s: %v", name, err)
	} else {
		log.Printf("metrics retry %s: recovered", name)
		if s.grader != nil {
			go s.grader.maybeGrade(m)
		}
	}
}

// publishActivity lands body at dir/name via a temp file and a hard link.
// The link fails with fs.ErrExist on a taken name — os.Rename would silently
// replace a same-named recording, and these are never overwritten. The .tmp
// suffix keeps a stranded temp out of both the fingerprint walk (which wants
// .json) and the activity listing (which wants a name the store would take).
func publishActivity(dir, name string, body []byte) error {
	tmp, err := os.CreateTemp(dir, name+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.Write(body)
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	return os.Link(tmp.Name(), filepath.Join(dir, name))
}

// getActivityMetrics serves one activity's measured numbers: the stored
// anchor-free row, plus the grade inputs computed against the anchors
// athlete.json declares AT THIS MOMENT — never at import time. The wire
// keys mirror tools/grade_metrics.py so a grade note's vocabulary does not
// depend on which register produced it. Windowed numbers (first-20-min,
// the bike after-warm-up band) re-decode the stored file (~12 ms); the
// run-legend share comes from the histogram, which the tests pin as
// diff-0 against the stream computation.
//
// A name with no row is a 404 either way, but the body says which way:
// not yet imported, or imported and failed (the failures table knows).
func (s *server) getActivityMetrics(w http.ResponseWriter, r *http.Request) {
	out, code, msg := s.activityMetricsPayload(r.URL.Query().Get("name"))
	if code != http.StatusOK {
		http.Error(w, msg, code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

// graderLap is one lap as the grader sees it. Deliberately not the page's
// lapOut: no positions, because a coordinate is noise in a prompt and a
// precise one is worse than noise; no polyline, ever, because 9 KB is about
// 4k tokens of high-entropy nothing per turn.
//
// Laps are READ fields — the device's own totals — rather than arithmetic
// this app performs, so they carry no mirror obligation and no gate run:
// there is no computation here for a second implementation to disagree
// with. They come from detail.go's decode, which already documents the
// conventions, rather than from a second implementation of the same walk.
type graderLap struct {
	N        int     `json:"n"`
	Trigger  string  `json:"trigger"`        // distance | manual | time | session_end
	Step     *int    `json:"step,omitempty"` // the prescribed step, when a pushed workout drove it
	DistM    float64 `json:"dist_m,omitempty"`
	Dist     string  `json:"dist,omitempty"`
	TimerS   float64 `json:"timer_s"`
	ElapsedS float64 `json:"elapsed_s,omitempty"` // only when it differs: a stop inside the lap
	Pace     string  `json:"pace,omitempty"`      // runs only
	AvgHR    *int    `json:"avg_hr,omitempty"`
	AvgPower *int    `json:"avg_power,omitempty"`
}

// maxGraderLaps bounds the list. A session with more laps than this is one
// the count describes better than the enumeration, and the payload has a
// small model'"'"'s context to sit in.
const maxGraderLaps = 40

// activityMetricsPayload builds the response for the handler above and for
// the grader's get_metrics tool — one builder, so the two consumers can
// never disagree about what an activity measured.
func (s *server) activityMetricsPayload(name string) (any, int, string) {
	if !validActivityName(name) {
		return nil, http.StatusBadRequest, "name must be a plain .fit filename"
	}
	row, err := s.metrics.rowByName(name)
	if err != nil {
		log.Printf("activity-metrics %s: %v", name, err)
		return nil, http.StatusInternalServerError, "could not read metrics"
	}
	if row == nil {
		if msg, _ := s.metrics.failureFor(name); msg != "" {
			// One line, bounded: the stored cause can carry decoder or
			// driver internals and this body reaches the network.
			if i := strings.IndexByte(msg, '\n'); i >= 0 {
				msg = msg[:i]
			}
			if len(msg) > 200 {
				msg = msg[:200] + "…"
			}
			return nil, http.StatusNotFound, "import failed: " + msg
		}
		return nil, http.StatusNotFound, "no metrics for that activity"
	}

	type hrOut struct {
		Avg          float64  `json:"avg"`
		Max          *int     `json:"max,omitempty"`
		DropoutShare *float64 `json:"dropout_share,omitempty"`
		Drift        *float64 `json:"drift,omitempty"`
	}
	type powerOut struct {
		Avg     float64 `json:"avg"`
		Max     int     `json:"max,omitempty"`
		Best60s float64 `json:"best_60s,omitempty"`
		WKg     float64 `json:"wkg,omitempty"`
		PctFTP  float64 `json:"pct_ftp,omitempty"`
		Z2BandW []int   `json:"z2_band_w,omitempty"`
	}
	type first20Out struct {
		Avg float64 `json:"avg_bpm"`
		Cap int     `json:"cap_bpm"`
	}
	out := struct {
		Name       string `json:"name"`
		Date       string `json:"date"`
		Sport      string `json:"sport"`
		StartUTC   string `json:"start_utc"`
		ElapsedS   int    `json:"elapsed_s"`
		ElapsedHMS string `json:"elapsed_hms"`
		// Present only when a recording gap exists, so a stop-free file's
		// payload is byte-identical to what it always was — the same
		// omit-when-equal rule the laps' elapsed_s follows. Moving time is
		// what the statistics below are weighted over; against a prescribed
		// duration it is the number that answers "was the work done",
		// because a phone call is not part of a ride.
		MovingS   int      `json:"moving_s,omitempty"`
		MovingHMS string   `json:"moving_hms,omitempty"`
		Records   int      `json:"records"`
		DistanceM *float64 `json:"distance_m,omitempty"`
		// Rendered in the athlete's units beside the raw metres, because
		// these numbers are quoted back to him and because a comparison
		// against the previous session of this kind carries them that way:
		// one side in miles and the other in metres is how a grade came to
		// describe the shorter run as the longer.
		Dist          string         `json:"dist,omitempty"`
		Pace          string         `json:"pace,omitempty"`
		SHA256        string         `json:"sha256"`
		HR            *hrOut         `json:"hr,omitempty"`
		Power         *powerOut      `json:"power,omitempty"`
		DecouplingPct *float64       `json:"decoupling_pct,omitempty"`
		Cadence       *float64       `json:"cadence,omitempty"`
		Weather       *conditions    `json:"weather,omitempty"`
		GradeInput    map[string]any `json:"grade_input,omitempty"`
		First20       *first20Out    `json:"first_20min,omitempty"`
		Profile       []profilePoint `json:"profile,omitempty"`
		// Where the clock stopped, and how far in. A grade note that says
		// "a stop around minutes 82-92" is guessing from the profile's
		// buckets — measured against the file, that stop was 2:34 at 9.34
		// miles, off by eight minutes and a mile and a half.
		Stops    []stop `json:"stops,omitempty"`
		StoppedS int    `json:"stopped_s,omitempty"`
		// The laps the device recorded, WITH their trigger. Without the
		// trigger a 2.4-second button press reads as a catastrophically
		// failed rep — 12 Aug 2026 carries two of them.
		Laps     []graderLap `json:"laps,omitempty"`
		LapCount int         `json:"lap_count,omitempty"`
	}{Name: row.Name, Date: row.Date, Sport: row.Sport, StartUTC: row.StartUTC,
		ElapsedS: row.ElapsedS, Records: row.Records, DistanceM: row.DistanceM,
		SHA256:     row.SHA256,
		ElapsedHMS: fmt.Sprintf("%d:%02d", row.ElapsedS/60, row.ElapsedS%60)}
	if row.MovingS > 0 && row.MovingS < row.ElapsedS {
		out.MovingS = row.MovingS
		out.MovingHMS = fmt.Sprintf("%d:%02d", row.MovingS/60, row.MovingS%60)
	}

	if row.AvgHR != nil {
		h := &hrOut{Avg: pyRound(*row.AvgHR, 1), Max: row.MaxHR}
		if row.DropoutShare != nil {
			v := pyRound(*row.DropoutShare, 4)
			h.DropoutShare = &v
		}
		if row.HRDrift != nil {
			v := pyRound(*row.HRDrift, 1)
			h.Drift = &v
		}
		out.HR = h
	}
	if row.DecouplingPct != nil {
		v := pyRound(*row.DecouplingPct, 2)
		out.DecouplingPct = &v
	}

	d := s.ds()
	a := d.Athlete
	kind := ""
	switch row.Sport {
	case "running":
		kind = "run"
	case "cycling":
		kind = "bike"
	}

	if row.DistanceM != nil && *row.DistanceM > 0 {
		out.Dist = Distance(*row.DistanceM).InMeasured(a.Units)
		if kind == "run" && row.ElapsedS > 0 {
			out.Pace = Pace(float64(row.ElapsedS) / *row.DistanceM).In(a.Units)
		}
	}

	if row.AvgPower != nil {
		p := &powerOut{Avg: pyRound(*row.AvgPower, 1)}
		// FTP is a CYCLING anchor: a run's watts (a Garmin running-power
		// estimate) divided by it is a meaningless ratio, and offering it
		// invites exactly the comparison it looks like — a local model read
		// "273 W against FTP" off a run and graded the day on it. Runs get
		// their watts reported and nothing derived from them.
		if kind == "bike" {
			if kg := float64(a.Weight); kg > 0 {
				p.WKg = pyRound(*row.AvgPower/kg, 2)
			}
			if ftp := a.Power["ftp"]; ftp > 0 {
				p.PctFTP = pyRound(*row.AvgPower/float64(ftp), 3)
				// pyRound, not math.Round: at FTP 214 the band top is
				// exactly 160.5, and the mirror's banker's rounding says 160.
				p.Z2BandW = []int{int(pyRound(0.56*float64(ftp), 0)), int(pyRound(0.75*float64(ftp), 0))}
			}
		}
		out.Power = p
	}
	if row.AvgCadence != nil {
		c := *row.AvgCadence
		if kind == "run" {
			c *= 2 // run records are per-leg; the register doubles at presentation
		}
		c = pyRound(c, 1)
		out.Cadence = &c
	}

	// The windowed numbers re-decode the canonical bytes; a file that has
	// gone missing costs those fields, never the stored row.
	var streams *activityStreams
	if kind != "" {
		if b, err := os.ReadFile(filepath.Join(s.activitiesDir(), name)); err == nil {
			if st, err := decodeActivity(b); err == nil {
				streams = st
			} else {
				log.Printf("activity-metrics %s: re-decode: %v", name, err)
			}
		} else {
			log.Printf("activity-metrics %s: %v", name, err)
		}
	}

	// What it was like out there, as read at import and kept since. Not
	// looked up again here: the conditions of a past moment do not change,
	// and a grade must go on citing the ones it was made against.
	out.Weather = row.Weather

	// How the session was actually ridden or run, minute by minute — capped
	// so an hour and a three-hour ride both cost about the same to read.
	if streams != nil {
		out.Profile = sessionProfile(streams, 60, a.Units)
		out.Stops, out.StoppedS = stopsIn(streams, a.Units)
	}

	// The laps, from the same bytes. A second decode costs about the time
	// one LLM turn spends on its first token, and it buys the grader the
	// difference between a rep and a button press.
	if kind != "" {
		if b, err := os.ReadFile(filepath.Join(s.activitiesDir(), name)); err == nil {
			if d, err := decodeDetail(b); err == nil {
				out.LapCount = len(d.Laps)
				for i, l := range d.Laps {
					if i >= maxGraderLaps {
						break
					}
					gl := graderLap{N: i + 1, Trigger: l.Trigger, Step: l.Step,
						DistM: pyRound(l.DistM, 1), TimerS: l.TimerS,
						AvgHR: l.AvgHR, AvgPower: l.AvgPower}
					if l.DistM > 0 {
						gl.Dist = Distance(l.DistM).InMeasured(a.Units)
						if kind == "run" && l.TimerS >= lapPaceFloorS && l.DistM >= lapPaceFloorM {
							gl.Pace = Pace(l.TimerS / l.DistM).In(a.Units)
						}
					}
					// Elapsed only where it differs from the timer, which is
					// the whole tell: a stop inside the lap.
					if l.ElapsedS-l.TimerS > 2 {
						gl.ElapsedS = l.ElapsedS
					}
					out.Laps = append(out.Laps, gl)
				}
			} else {
				log.Printf("activity-metrics %s: laps: %v", name, err)
			}
		}
	}

	// The peak an average hides. A ramp test's whole result is its best
	// minute — FTP is derived from it — and a ride that climbed to failure
	// reads as a soft steady effort until these are on the page.
	if out.Power != nil && streams != nil && streams.HaveWatts {
		mx := 0
		for _, w := range streams.Watts {
			if w > mx {
				mx = w
			}
		}
		out.Power.Max = mx
		if kind == "bike" {
			if b := bestRolling(streams.Time, intsToFloats(streams.Watts), 60); b != nil {
				out.Power.Best60s = pyRound(*b, 1)
			}
		}
	}

	// Which anchor the run rubric measures under is the block's declaration,
	// not this file's assumption.
	capKey := "gradeCap"
	if blk := d.Current(s.day(d)); blk != nil {
		capKey = blk.Grading.CapKey()
	}

	switch kind {
	case "run":
		if cap, ok := a.HR[capKey]; ok {
			if share, err := s.metrics.underCapShareSQL(name, cap); err != nil {
				log.Printf("activity-metrics %s: share: %v", name, err)
			} else if share != nil {
				out.GradeInput = map[string]any{
					"under_grade_cap_share": pyRound(*share, 4),
					"grade_cap_bpm":         cap,
				}
			}
		}
		if capF, ok := a.HR["firstMin"]; ok && streams != nil {
			if m := runFirst20Mean(streams); m != nil {
				out.First20 = &first20Out{Avg: pyRound(*m, 1), Cap: capF}
			}
		}
	case "bike":
		lo, okLo := a.HR["bikeLo"]
		hi, okHi := a.HR["bikeHi"]
		cap, okCap := a.HR["bikeCap"]
		if okLo && okHi && okCap && streams != nil {
			inBand, secsOver := bikeGradeInput(streams, lo, hi, cap)
			if inBand != nil {
				// Named in their units: these sit beside watts, and an
				// unlabelled "cap: 145" beside "avg: 119.2 W" came back out
				// of a grade note as "145 W" when it is a heart rate.
				gi := map[string]any{
					"in_band_share_after_warmup": pyRound(*inBand, 4),
					"hr_band_bpm":                []int{lo, hi},
					"hr_cap_bpm":                 cap,
				}
				if secsOver != nil {
					gi["secs_over_hr_cap"] = *secsOver
				}
				out.GradeInput = gi
			}
		}
	}

	return out, http.StatusOK, ""
}

// getActivity serves one stored file back, byte for byte. Health data, so
// no-store — the fit route's public caching pattern does not apply here.
func (s *server) getActivity(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if !validActivityName(name) {
		http.Error(w, "name must be a plain .fit filename", http.StatusBadRequest)
		return
	}
	b, err := os.ReadFile(filepath.Join(s.activitiesDir(), name))
	if errors.Is(err, fs.ErrNotExist) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		log.Printf("activity %s: %v", name, err)
		http.Error(w, "could not read", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}
