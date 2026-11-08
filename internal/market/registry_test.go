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

// TestRegistrySearchPopularity 钉住 task-50：npm search 的
// score.detail.popularity 进 SearchPackage.Popularity；三种形态——
// 正常数值 / 整个 score 缺失 / score 为 null——缺的按 0，命中不丢。
func TestRegistrySearchPopularity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 用原始 JSON 而不是 struct 编码：struct 表达不出「score 键存在但为 null」。
		body := `{"total":3,"objects":[` +
			`{"package":{"name":"with-score","version":"1.0.0"},"score":{"detail":{"popularity":0.9,"final":1}}},` +
			`{"package":{"name":"no-score","version":"1.1.0"}},` +
			`{"package":{"name":"null-score","version":"1.2.0"},"score":null}]}`
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	reg, err := NewRegistry(srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	hits, err := reg.Search(context.Background(), "opencode", 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("hits = %d, want 3", len(hits))
	}
	want := []struct {
		name       string
		popularity float64
	}{
		{"with-score", 0.9},
		{"no-score", 0},
		{"null-score", 0},
	}
	for i, tc := range want {
		if hits[i].Name != tc.name {
			t.Fatalf("hits[%d].name = %q, want %q", i, hits[i].Name, tc.name)
		}
		if hits[i].Popularity != tc.popularity {
			t.Fatalf("hits[%d] (%s) popularity = %v, want %v", i, tc.name, hits[i].Popularity, tc.popularity)
		}
	}
}
