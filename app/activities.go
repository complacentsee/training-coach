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
