package main

// Recorded activity files pulled off the watch land here as opaque bytes under
// <data>/activities/. They are personal health data with the same standing as
// the entries log: server-only, never overwritten, never decoded — the server
// checks the .FIT magic and nothing else. The directory is invisible to the
// plan: the data Rev hashes only the files loadDataset takes, and
// fingerprint() counts only non-hidden .json files, so neither an activity
// nor a stranded .tmp can perturb a reload or fail `make verify`.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
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

func (s *server) activitiesDir() string {
	return filepath.Join(s.dataDir, "activities")
}

type activityInfo struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// getActivities lists the stored files, newest first — device names are
// timestamps, so descending name order is descending time. A store that does
// not exist yet is an empty list, never an error and never null.
func (s *server) getActivities(w http.ResponseWriter, r *http.Request) {
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
		info, err := e.Info()
		if err != nil {
			continue
		}
		list = append(list, activityInfo{Name: e.Name(), Size: info.Size()})
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
	if len(body) < 12 || string(body[8:12]) != ".FIT" {
		http.Error(w, "not a FIT file", http.StatusBadRequest)
		return
	}
	dir := s.activitiesDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("activity %s: %v", name, err)
		http.Error(w, "could not store", http.StatusInternalServerError)
		return
	}
	if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
		http.Error(w, "already stored", http.StatusConflict)
		return
	}
	if err := publishActivity(dir, name, body); err != nil {
		if errors.Is(err, fs.ErrExist) {
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
	if err := s.metrics.importOne(name, body, s.ds().Loc); err != nil {
		log.Printf("metrics %s: %v", name, err)
	}
	w.WriteHeader(http.StatusNoContent)
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
	name := r.URL.Query().Get("name")
	if !validActivityName(name) {
		http.Error(w, "name must be a plain .fit filename", http.StatusBadRequest)
		return
	}
	row, err := s.metrics.rowByName(name)
	if err != nil {
		log.Printf("activity-metrics %s: %v", name, err)
		http.Error(w, "could not read metrics", http.StatusInternalServerError)
		return
	}
	if row == nil {
		if msg, _ := s.metrics.failureFor(name); msg != "" {
			http.Error(w, "import failed: "+msg, http.StatusNotFound)
			return
		}
		http.Error(w, "no metrics for that activity", http.StatusNotFound)
		return
	}

	type hrOut struct {
		Avg          float64  `json:"avg"`
		Max          *int     `json:"max,omitempty"`
		DropoutShare *float64 `json:"dropout_share,omitempty"`
		Drift        *float64 `json:"drift,omitempty"`
	}
	type powerOut struct {
		Avg     float64 `json:"avg"`
		WKg     float64 `json:"wkg,omitempty"`
		PctFTP  float64 `json:"pct_ftp,omitempty"`
		Z2BandW []int   `json:"z2_band_w,omitempty"`
	}
	type first20Out struct {
		Avg float64 `json:"avg"`
		Cap int     `json:"cap"`
	}
	out := struct {
		Name          string         `json:"name"`
		Date          string         `json:"date"`
		Sport         string         `json:"sport"`
		StartUTC      string         `json:"start_utc"`
		ElapsedS      int            `json:"elapsed_s"`
		ElapsedHMS    string         `json:"elapsed_hms"`
		Records       int            `json:"records"`
		DistanceM     *float64       `json:"distance_m,omitempty"`
		SHA256        string         `json:"sha256"`
		HR            *hrOut         `json:"hr,omitempty"`
		Power         *powerOut      `json:"power,omitempty"`
		DecouplingPct *float64       `json:"decoupling_pct,omitempty"`
		Cadence       *float64       `json:"cadence,omitempty"`
		GradeInput    map[string]any `json:"grade_input,omitempty"`
		First20       *first20Out    `json:"first_20min,omitempty"`
	}{Name: row.Name, Date: row.Date, Sport: row.Sport, StartUTC: row.StartUTC,
		ElapsedS: row.ElapsedS, Records: row.Records, DistanceM: row.DistanceM,
		SHA256:     row.SHA256,
		ElapsedHMS: fmt.Sprintf("%d:%02d", row.ElapsedS/60, row.ElapsedS%60)}

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

	if row.AvgPower != nil {
		p := &powerOut{Avg: pyRound(*row.AvgPower, 1)}
		if kg := float64(a.Weight); kg > 0 {
			p.WKg = pyRound(*row.AvgPower/kg, 2)
		}
		if ftp := a.Power["ftp"]; ftp > 0 {
			p.PctFTP = pyRound(*row.AvgPower/float64(ftp), 3)
			// pyRound, not math.Round: at FTP 214 the band top is exactly
			// 160.5, and the mirror's banker's rounding says 160.
			p.Z2BandW = []int{int(pyRound(0.56*float64(ftp), 0)), int(pyRound(0.75*float64(ftp), 0))}
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

	switch kind {
	case "run":
		if cap, ok := a.HR["gradeCap"]; ok {
			if share, err := s.metrics.underCapShareSQL(name, cap); err != nil {
				log.Printf("activity-metrics %s: share: %v", name, err)
			} else if share != nil {
				out.GradeInput = map[string]any{
					"under_grade_cap_share": pyRound(*share, 4),
					"grade_cap":             cap,
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
				gi := map[string]any{
					"in_band_share_after_warmup": pyRound(*inBand, 4),
					"band":                       []int{lo, hi},
					"cap":                        cap,
				}
				if secsOver != nil {
					gi["secs_over_cap"] = *secsOver
				}
				out.GradeInput = gi
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
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
