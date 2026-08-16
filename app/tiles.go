package main

// Map tiles, fetched by the SERVER and cached on the volume. The browser
// never talks to a tile provider — it asks this app, exactly as it asks for
// everything else, which is what keeps the zero-third-party property a
// property rather than a habit. It is the weather pattern applied again:
// fetch outside data server-side, keep what comes back, never fetch it
// twice.
//
// OSM's tile usage policy binds THIS SERVER now, not the browser. Read
// 15 Aug 2026 from operations.osmfoundation.org/policies/tiles, and what it
// requires is built in here rather than left to the operator:
//
//   - A clear, unique User-Agent naming the app. A generic library or proxy
//     default ("okhttp", "nginx") is blocked without notice, and
//     impersonating a browser is forbidden outright. TILE_CONTACT appends a
//     contact to it, which the policy asks for and cannot be defaulted
//     because this app does not know who is running it.
//   - HTTPS, and only the documented URL shape.
//   - Cache for at least seven days. This cache is indefinite: a tile of a
//     neighbourhood does not change between two runs down the same street.
//   - NO BULK DOWNLOADING. "Pre-emptive fetching of tiles other than those
//     a user is actively viewing" is the policy's own definition of
//     scraping, so there is no warm-up pass, no pre-seed, and no
//     archive-wide sweep here — the feasibility note's "30-80 MB for the
//     whole archive" is a thing this deliberately does not do. A tile is
//     fetched when a viewer's map asks for it and never otherwise.
//
// The cache is derived and disposable, like metrics.db: safe to delete, it
// refills from use. It is invisible to the data Rev and the reload
// fingerprint because both count non-hidden .json only, pinned by a test.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// tileMinZoom/tileMaxZoom bound what a viewer can ask for. Seventeen is
	// a street with house numbers on it, which is as close as a route needs;
	// past it the policy's own examples of scraping start.
	tileMinZoom = 3
	tileMaxZoom = 17
	// tileMaxBytes is a sanity bound on one tile — a 256px PNG is tens of
	// kilobytes and nothing legitimate approaches this.
	tileMaxBytes = 512 << 10
	// tileFetchers is how many upstream fetches may be in flight at once.
	// A viewport is a dozen tiles; four at a time fills it promptly without
	// ever looking like a scan.
	tileFetchers = 4
)

// tileService proxies and caches one tile source.
type tileService struct {
	dir     string // <data>/tiles
	url     string // template with {z} {x} {y}
	agent   string
	http    *http.Client
	enabled bool

	sem chan struct{}

	mu       sync.Mutex
	inFlight map[string]chan struct{}
	fetched  int
	bytes    int64
	logged   int
}

// newTileService reads its configuration from the environment. Tiles are ON
// by default with OSM as the source: an app that draws a map with no map is
// a worse default than one that fetches politely.
func newTileService(dataDir string) *tileService {
	t := &tileService{
		dir:      filepath.Join(dataDir, "tiles"),
		url:      envOr("TILE_URL", "https://tile.openstreetmap.org/{z}/{x}/{y}.png"),
		enabled:  envOr("TILES", "on") != "off",
		sem:      make(chan struct{}, tileFetchers),
		inFlight: map[string]chan struct{}{},
		http:     &http.Client{Timeout: 20 * time.Second},
	}
	// Names the app and its version, and carries a contact when the operator
	// gives one. Never a browser's, never a library's default.
	t.agent = "running-coach/" + buildHash + " (self-hosted training log)"
	if c := strings.TrimSpace(os.Getenv("TILE_CONTACT")); c != "" {
		t.agent = "running-coach/" + buildHash + " (self-hosted training log; " + c + ")"
	}
	if t.enabled && !strings.HasPrefix(t.url, "https://") {
		log.Printf("tiles:    OFF — TILE_URL must be https (policy), got %q", t.url)
		t.enabled = false
	}
	if t.enabled {
		log.Printf("tiles:    on, %s, cached in %s", t.url, t.dir)
	}
	return t
}

// tilePath is the cache location, and the only place z/x/y become a path.
func (t *tileService) tilePath(z, x, y int) string {
	return filepath.Join(t.dir, strconv.Itoa(z), strconv.Itoa(x), strconv.Itoa(y)+".png")
}

// validTile refuses anything outside the bounded zooms or off the grid, so a
// path can never be built from a number this did not check.
func validTile(z, x, y int) bool {
	if z < tileMinZoom || z > tileMaxZoom {
		return false
	}
	n := 1 << uint(z)
	return x >= 0 && x < n && y >= 0 && y < n
}

func (t *tileService) upstream(z, x, y int) string {
	r := strings.NewReplacer("{z}", strconv.Itoa(z), "{x}", strconv.Itoa(x), "{y}", strconv.Itoa(y))
	return r.Replace(t.url)
}

// fetch pulls one tile and stores it. Two callers wanting the same tile at
// the same moment make one request: a map opening cold asks for a dozen
// neighbours, and asking upstream twice for the same one is exactly the
// pattern the policy watches for.
func (t *tileService) fetch(ctx context.Context, z, x, y int) ([]byte, error) {
	key := fmt.Sprintf("%d/%d/%d", z, x, y)
	t.mu.Lock()
	if wait, ok := t.inFlight[key]; ok {
		t.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return os.ReadFile(t.tilePath(z, x, y))
	}
	done := make(chan struct{})
	t.inFlight[key] = done
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.inFlight, key)
		t.mu.Unlock()
		close(done)
	}()

	select {
	case t.sem <- struct{}{}:
		defer func() { <-t.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	req, err := http.NewRequestWithContext(ctx, "GET", t.upstream(z, x, y), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", t.agent)
	resp, err := t.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tile %s: upstream %d", key, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, tileMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > tileMaxBytes {
		return nil, fmt.Errorf("tile %s: over %d bytes", key, tileMaxBytes)
	}
	if err := storeTile(t.tilePath(z, x, y), body); err != nil {
		// A cache that cannot write still serves the viewer; it just pays
		// again next time, and says so once.
		log.Printf("tiles:    could not cache %s: %v", key, err)
	}
	t.mu.Lock()
	t.fetched++
	t.bytes += int64(len(body))
	// Log what it grows to, on a widening scale so a long session does not
	// fill the log with it.
	if t.fetched >= t.logged*2 || t.logged == 0 {
		t.logged = t.fetched
		log.Printf("tiles:    %d fetched, %.1f MB cached", t.fetched, float64(t.bytes)/(1<<20))
	}
	t.mu.Unlock()
	return body, nil
}

// storeTile writes atomically, so a killed process leaves no half tile that
// would be served forever after.
func storeTile(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tile-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// getTile serves one tile from the cache, fetching it once if this viewer is
// the first to look there.
func (s *server) getTile(w http.ResponseWriter, r *http.Request) {
	t := s.tiles
	if t == nil || !t.enabled {
		http.Error(w, "tiles are off", http.StatusNotFound)
		return
	}
	z, errZ := strconv.Atoi(r.PathValue("z"))
	x, errX := strconv.Atoi(r.PathValue("x"))
	y, errY := strconv.Atoi(strings.TrimSuffix(r.PathValue("y"), ".png"))
	if errZ != nil || errX != nil || errY != nil || !validTile(z, x, y) {
		http.Error(w, "no such tile", http.StatusBadRequest)
		return
	}

	body, err := os.ReadFile(t.tilePath(z, x, y))
	if errors.Is(err, fs.ErrNotExist) {
		body, err = t.fetch(r.Context(), z, x, y)
	}
	if err != nil {
		if r.Context().Err() != nil {
			return // the viewer panned away; not an error worth a body
		}
		log.Printf("tiles:    %d/%d/%d: %v", z, x, y, err)
		http.Error(w, "tile unavailable", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	// A week is the policy's floor and a tile of a street is stable for far
	// longer; the browser holding it is one fewer request upstream.
	w.Header().Set("Cache-Control", "public, max-age=604800")
	_, _ = w.Write(body)
}
