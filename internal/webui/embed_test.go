package webui

import (
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var assetRefPattern = regexp.MustCompile(`assets/[A-Za-z0-9_.-]+\.(?:js|css)`)

// TestEmbeddedAssetsAreComplete guards the //go:embed all:dist detail: a plain
// `dist` pattern skips names starting with "_" (Vite's shared helper chunks),
// and the missing chunk makes lazily loaded pages silently blank.
func TestEmbeddedAssetsAreComplete(t *testing.T) {
	missing := map[string]bool{}
	checked := 0
	err := fs.WalkDir(distFS, "dist", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(p, ".js") && !strings.HasSuffix(p, ".css") && !strings.HasSuffix(p, ".html") {
			return nil
		}
		body, err := fs.ReadFile(distFS, p)
		if err != nil {
			return err
		}
		refs := assetRefPattern.FindAllString(string(body), -1)
		for _, ref := range refs {
			checked++
			if _, err := fs.Stat(distFS, "dist/"+ref); err != nil {
				missing[ref] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded dist: %v", err)
	}
	if checked == 0 {
		t.Fatal("no asset references found; the embed or the build is empty")
	}
	if len(missing) > 0 {
		list := make([]string, 0, len(missing))
		for ref := range missing {
			list = append(list, ref)
		}
		sort.Strings(list)
		t.Fatalf("referenced assets missing from the embedded filesystem: %v", list)
	}
}
