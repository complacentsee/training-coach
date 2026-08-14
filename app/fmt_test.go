package main

// Formatting is checked by the suite rather than by remembering to run
// gofmt: drift here is invisible in review and shows up as noise in the
// next unrelated diff. Covers this package and the tools beside it.

import (
	"bytes"
	"go/format"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSourcesAreFormatted(t *testing.T) {
	var unformatted []string
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") {
			return nil //nolint:nilerr // an unreadable tree is not this test's business
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		want, err := format.Source(src)
		if err != nil {
			t.Errorf("%s: %v", p, err)
			return nil
		}
		if !bytes.Equal(src, want) {
			unformatted = append(unformatted, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(unformatted) > 0 {
		t.Errorf("not gofmt-clean: %s\n\trun: gofmt -w %s",
			strings.Join(unformatted, " "), strings.Join(unformatted, " "))
	}
}
