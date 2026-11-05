package market

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// tarEntry describes one entry of a test tarball. dir=true creates a directory
// entry; link != "" creates a symlink entry (used only to assert rejection).
type tarEntry struct {
	name    string
	content string
	dir     bool
	link    string
}

// buildTarball packs entries into a tar.gz byte slice.
func buildTarball(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for _, e := range entries {
		var hdr *tar.Header
		switch {
		case e.dir:
			hdr = &tar.Header{Name: e.name, Mode: 0o755, Typeflag: tar.TypeDir}
		case e.link != "":
			hdr = &tar.Header{Name: e.name, Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: e.link}
		default:
			hdr = &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.content)), Typeflag: tar.TypeReg}
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header %q: %v", e.name, err)
		}
		if !e.dir && e.link == "" {
			if _, err := tw.Write([]byte(e.content)); err != nil {
				t.Fatalf("write tar body %q: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

// hitList records request paths of a fake registry, read back after the calls
// complete.
type hitList struct {
	mu   sync.Mutex
	hits []string
}

func (h *hitList) snapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.hits...)
}

// fakeNPMRegistry serves the metadata of one package and one tarball stream on
// any .tgz path. The dist tarball URLs inside the metadata are rewritten to the
// httptest server so the pipeline downloads from the fake registry.
func fakeNPMRegistry(t *testing.T, meta PackageMetadata, tarball []byte) (*Registry, *hitList) {
	t.Helper()
	hits := &hitList{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.mu.Lock()
		hits.hits = append(hits.hits, r.URL.Path)
		hits.mu.Unlock()
		if strings.HasSuffix(r.URL.Path, ".tgz") {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(tarball)
			return
		}
		if r.URL.Path == "/unknown" {
			http.NotFound(w, r)
			return
		}
		data, err := json.Marshal(meta)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))
	// Point every dist tarball at this fake server, and make it absolute so
	// the pipeline's Fetch can dial it without a proxy rule.
	rewritten := make(map[string]PackageVersion, len(meta.Versions))
	for v, pv := range meta.Versions {
		pv.Dist.Tarball = srv.URL + "/" + meta.Name + "-" + v + ".tgz"
		rewritten[v] = pv
	}
	meta.Versions = rewritten
	t.Cleanup(srv.Close)
	reg, err := NewRegistry(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg, hits
}

// fakeBridge writes a dummy magpie-bridge.exe and returns its path.
func fakeBridge(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "magpie-bridge.exe")
	if err := os.WriteFile(path, []byte("MZ fake bridge"), 0o755); err != nil {
		t.Fatalf("write fake bridge: %v", err)
	}
	return path
}

// encodeName mirrors the registry path shape used by the package client so a
// fake server can match it without globbing.
func encodeName(pkg string) string {
	return "/" + url.PathEscape(pkg)
}
