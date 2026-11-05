package market

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewRegistryBases(t *testing.T) {
	reg, err := NewRegistry("", nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if reg.Base() != DefaultRegistry {
		t.Fatalf("default base = %q, want %q", reg.Base(), DefaultRegistry)
	}
	reg, err = NewRegistry(RegistryMirror, nil)
	if err != nil {
		t.Fatalf("NewRegistry(mirror): %v", err)
	}
	if reg.Base() != RegistryMirror {
		t.Fatalf("mirror base = %q, want %q", reg.Base(), RegistryMirror)
	}
	if DefaultRegistry != "https://registry.npmjs.org" {
		t.Fatalf("DefaultRegistry = %q, want the official URL", DefaultRegistry)
	}
	if RegistryMirror != "https://registry.npmmirror.com" {
		t.Fatalf("RegistryMirror = %q, want the domestic mirror", RegistryMirror)
	}
	if _, err := NewRegistry("ftp://registry", nil); err == nil {
		t.Fatal("an ftp base must be rejected")
	}
	if _, err := NewRegistry("not a url", nil); err == nil {
		t.Fatal("a bare base must be rejected")
	}
}

func TestRegistryMirrorSwitchHitsConfiguredBase(t *testing.T) {
	// 镜像切换：同一个 client 换 base，请求必须打到配置的 registry 而不是
	// 硬编码的官方地址——用 httptest 假 registry 证明请求确实到了那里。
	meta := PackageMetadata{Name: "pkg", Versions: map[string]PackageVersion{}}
	reg, hits := fakeNPMRegistry(t, meta, nil)
	if _, err := reg.Metadata(context.Background(), "pkg"); err != nil {
		t.Fatalf("Metadata: %v", err)
	}
	got := hits.snapshot()
	if len(got) != 1 || got[0] != "/pkg" {
		t.Fatalf("request paths = %v, want exactly [/pkg] on the configured base", got)
	}
}

func TestRegistryMetadataDecodes(t *testing.T) {
	pkg := "@magpie-community/opencode-trae-auth"
	meta := PackageMetadata{
		Name:     pkg,
		DistTags: map[string]string{"latest": "1.2.3"},
		Versions: map[string]PackageVersion{
			"1.2.3": {Name: pkg, Version: "1.2.3", Dist: Dist{Shasum: "aaa", Integrity: "sha512-bbb"}},
		},
	}
	reg, hits := fakeNPMRegistry(t, meta, nil)
	got, err := reg.Metadata(context.Background(), pkg)
	if err != nil {
		t.Fatalf("Metadata: %v", err)
	}
	if got.Name != pkg || got.DistTags["latest"] != "1.2.3" {
		t.Fatalf("metadata = %+v", got)
	}
	if got.Versions["1.2.3"].Dist.Integrity != "sha512-bbb" {
		t.Fatalf("dist = %+v", got.Versions["1.2.3"].Dist)
	}
	paths := hits.snapshot()
	if len(paths) != 1 || !strings.Contains(paths[0], pkg) {
		t.Fatalf("metadata request paths = %v, want one hit containing the package", paths)
	}
}

func TestRegistrySearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/-/v1/search" {
			t.Errorf("search path = %q, want /-/v1/search", r.URL.Path)
		}
		if got := r.URL.Query().Get("text"); got != "trae" {
			t.Errorf("text = %q, want trae", got)
		}
		if got := r.URL.Query().Get("size"); got != "3" {
			t.Errorf("size = %q, want 3", got)
		}
		_ = json.NewEncoder(w).Encode(SearchResponse{
			Total: 1,
			Objects: []SearchObject{{Package: SearchPackage{
				Name: "opencode-trae-auth", Version: "1.0.0", Description: "trae auth plugin",
			}}},
		})
	}))
	t.Cleanup(srv.Close)
	reg, err := NewRegistry(srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	hits, err := reg.Search(context.Background(), "trae", 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].Name != "opencode-trae-auth" || hits[0].Version != "1.0.0" {
		t.Fatalf("search hits = %+v", hits)
	}
}

func TestRegistryNotFound(t *testing.T) {
	meta := PackageMetadata{Name: "whatever", Versions: map[string]PackageVersion{}}
	reg, _ := fakeNPMRegistry(t, meta, nil)
	_, err := reg.Metadata(context.Background(), "unknown")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown package error = %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error text = %q, want it to say not found", err)
	}
}
