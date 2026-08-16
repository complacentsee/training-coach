package main

// The tile proxy and the policy that binds it. The browser must reach no
// third party, the cache must be invisible to the plan, and the fetching
// must stay inside what OSM's usage policy permits — a unique User-Agent, no
// second request for a tile already held, and nothing fetched that a viewer
// did not ask to see.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// upstreamStub is a fake tile server: what it was asked for, and how the
// caller identified itself. No test touches the real tile servers.
type upstreamStub struct {
	hits  int32
	mu    sync.Mutex
	agent string
}

func (u *upstreamStub) userAgent() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.agent
}

func stubTiles(t *testing.T, dir string) (*tileService, *upstreamStub) {
	t.Helper()
	stub := &upstreamStub{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&stub.hits, 1)
		stub.mu.Lock()
		stub.agent = r.Header.Get("User-Agent")
		stub.mu.Unlock()
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\nfake"))
	}))
	t.Cleanup(up.Close)
	return &tileService{
		dir: filepath.Join(dir, "tiles"), url: up.URL + "/{z}/{x}/{y}.png",
		agent:    "running-coach/testbuild (self-hosted training log)",
		enabled:  true,
		sem:      make(chan struct{}, tileFetchers),
		inFlight: map[string]chan struct{}{},
		http:     &http.Client{Timeout: 5 * time.Second},
	}, stub
}

// TestTileProxyFetchesOnceAndIdentifiesItself: a tile already on the volume
// is never asked for again — the policy's caching requirement and the thing
// that keeps a self-hosted map from looking like a scan — and every request
// upstream names this app rather than a library or a browser.
func TestTileProxyFetchesOnceAndIdentifiesItself(t *testing.T) {
	dir := t.TempDir()
	svc, stub := stubTiles(t, dir)
	srv := fitTestMuxServer(t, "")
	srv.s.tiles = svc

	for i := 0; i < 3; i++ {
		rec := get(srv.mux, "/tiles/13/2016/2984.png", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d = %d: %s", i, rec.Code, rec.Body)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
			t.Errorf("content type %q", ct)
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age=604800") {
			t.Errorf("cache-control %q — the policy asks for a week at least", cc)
		}
	}
	if n := atomic.LoadInt32(&stub.hits); n != 1 {
		t.Errorf("%d upstream fetches for one tile, want exactly 1", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "tiles", "13", "2016", "2984.png")); err != nil {
		t.Errorf("the tile was not cached on the volume: %v", err)
	}

	ua := stub.userAgent()
	if !strings.HasPrefix(ua, "running-coach/") {
		t.Errorf("upstream saw User-Agent %q, which does not name this app", ua)
	}
	for _, forbidden := range []string{"Mozilla", "okhttp", "python-requests", "nginx", "Go-http-client"} {
		if strings.Contains(ua, forbidden) {
			t.Errorf("User-Agent %q masquerades as %s, which the policy forbids", ua, forbidden)
		}
	}
}

// TestTileBounds: the zoom range is bounded and the grid is checked, so a
// path is never built from a number nobody validated — and z18 upward is
// where the policy's own examples of scraping begin.
func TestTileBounds(t *testing.T) {
	dir := t.TempDir()
	svc, stub := stubTiles(t, dir)
	srv := fitTestMuxServer(t, "")
	srv.s.tiles = svc

	for _, bad := range []string{
		"/tiles/2/1/1.png",      // under the floor
		"/tiles/18/1/1.png",     // over the ceiling
		"/tiles/13/99999/1.png", // off the grid
		"/tiles/13/1/-1.png",    // off the grid
		"/tiles/13/x/1.png",     // not a number
	} {
		if rec := get(srv.mux, bad, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", bad, rec.Code)
		}
	}
	if n := atomic.LoadInt32(&stub.hits); n != 0 {
		t.Errorf("%d upstream fetches for requests that never qualified", n)
	}

	// Tiles off is a 404 and no fetch, so an operator can run the app with
	// no outbound requests at all.
	svc.enabled = false
	if rec := get(srv.mux, "/tiles/13/2016/2984.png", nil); rec.Code != http.StatusNotFound {
		t.Errorf("tiles off = %d, want 404", rec.Code)
	}
	if n := atomic.LoadInt32(&stub.hits); n != 0 {
		t.Errorf("a fetch happened with tiles off")
	}
}

// TestTilesAreInvisibleToTheDataRev: the cache lives on the same volume as
// the plan and must not perturb it. A tile write that moved the Rev would
// rotate every workout's identity; one that moved the fingerprint would make
// the reload poller churn on every pan of a map.
func TestTilesAreInvisibleToTheDataRev(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{"blocks", "library"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	requireAthleteData(t)
	copyFile(t, "./data/athlete.json", filepath.Join(dir, "athlete.json"))
	for _, n := range []string{"movements.json", "sessions.json", "tasks.json", "index.json"} {
		copyFile(t, filepath.Join("./data/library", n), filepath.Join(dir, "library", n))
	}
	copyFile(t, "./data/blocks/2026-08-16-week-build.json", filepath.Join(dir, "blocks", "b.json"))

	before, err := loadDataset(dir, time.UTC)
	if err != nil {
		t.Fatalf("load without tiles: %v", err)
	}
	fpBefore := fingerprint(dir)

	for _, p := range []string{"13/2016/2984.png", "17/33001/48123.png"} {
		if err := storeTile(filepath.Join(dir, "tiles", filepath.FromSlash(p)), []byte("\x89PNG")); err != nil {
			t.Fatal(err)
		}
	}
	after, err := loadDataset(dir, time.UTC)
	if err != nil {
		t.Fatalf("the tile cache broke the load: %v", err)
	}
	if after.Rev != before.Rev {
		t.Errorf("data Rev moved: %s -> %s — a cached tile must be invisible to it", before.Rev, after.Rev)
	}
	if fp := fingerprint(dir); fp != fpBefore {
		t.Errorf("fingerprint moved: %s -> %s — the poller would reload on every map pan", fpBefore, fp)
	}
}

// TestContentPolicyIsServed: the browser's zero-third-party property was
// true by construction and enforced by nothing until a map arrived. Now it
// is a header, on every response, naming no host but this one.
func TestContentPolicyIsServed(t *testing.T) {
	h := secured(fitTestMux(t, t.TempDir()))
	for _, path := range []string{"/", "/calendar", "/api/activities"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		csp := rec.Header().Get("Content-Security-Policy")
		if csp == "" {
			t.Fatalf("%s carries no content security policy", path)
		}
		for _, want := range []string{"default-src 'self'", "script-src 'self'", "object-src 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: policy lacks %q: %s", path, want, csp)
			}
		}
		// Anything that would let a page reach off this origin.
		for _, bad := range []string{"http://", "https://", "*", "unsafe-eval"} {
			if strings.Contains(csp, bad) {
				t.Errorf("%s: policy admits %q: %s", path, bad, csp)
			}
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: no nosniff", path)
		}
	}
}
