package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// assets serves the embedded static files under content-hashed URLs.
//
// The previous setup could not work behind a CDN: http.FileServer over an
// embed.FS emits no ETag, and no Last-Modified either because embedded files
// carry a zero ModTime. With max-age and no validator, an intermediary cache
// has nothing to revalidate against and simply serves the stale copy until the
// TTL expires.
//
// So the filename carries the hash — style.css is requested as
// style.<hash>.css. A new build changes the hash, which changes the URL, so a
// cached copy of the old URL can never be served for the new one. That makes
// the assets safely immutable and removes any need to purge.
type assets struct {
	body map[string][]byte
	hash map[string]string
}

func newAssets(root fs.FS) (*assets, error) {
	a := &assets{body: map[string][]byte{}, hash: map[string]string{}}
	err := fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(root, p)
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		sum := sha256.Sum256(b)
		a.body[p] = b
		a.hash[p] = hex.EncodeToString(sum[:])[:12]
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(a.body) == 0 {
		return nil, fmt.Errorf("no static assets embedded")
	}
	return a, nil
}

// Path returns the hashed URL for an asset name, e.g.
// "style.css" -> "/static/style.9f2a1c4b7e08.css".
func (a *assets) Path(name string) string {
	h, ok := a.hash[name]
	if !ok {
		return "/static/" + name // unknown asset: let it 404 visibly
	}
	ext := path.Ext(name)
	return "/static/" + strings.TrimSuffix(name, ext) + "." + h + ext
}

// split turns "style.9f2a1c4b7e08.css" back into ("style.css", "9f2a1c4b7e08").
// A name with no hash segment returns an empty hash.
func split(name string) (base, hash string) {
	ext := path.Ext(name)
	if ext == "" {
		return name, ""
	}
	stem := strings.TrimSuffix(name, ext)
	i := strings.LastIndex(stem, ".")
	if i < 0 {
		return name, ""
	}
	cand := stem[i+1:]
	if len(cand) != 12 || strings.Trim(cand, "0123456789abcdef") != "" {
		return name, "" // not one of our hashes
	}
	return stem[:i] + ext, cand
}

func (a *assets) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/static/")
		base, hash := split(name)
		b, ok := a.body[base]
		if !ok {
			http.NotFound(w, r)
			return
		}
		etag := `"` + a.hash[base] + `"`
		w.Header().Set("ETag", etag)

		if hash == a.hash[base] {
			// The URL pins this exact content, so it can never go stale.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			// Unhashed or outdated URL: allow caching but force revalidation,
			// so an old link picks up new bytes on the next request.
			w.Header().Set("Cache-Control", "public, max-age=0, must-revalidate")
		}

		if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, a.hash[base]) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		// Zero modtime: ServeContent then relies on our ETag rather than
		// emitting a bogus Last-Modified.
		http.ServeContent(w, r, base, time.Time{}, bytes.NewReader(b))
	})
}

// etagFor is used for JSON responses whose body is computed per request.
func etagFor(b []byte) string {
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:])[:12] + `"`
}
