package market

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGitHub serves branch archive tarballs for /<owner>/<repo>/archive/refs/
// heads/<branch>.tar.gz. archives maps "owner/repo/branch" → body; a missing
// key answers 404. hits records the requested paths.
func fakeGitHub(t *testing.T, archives map[string][]byte) (*GitHubSource, *hitList) {
	t.Helper()
	hits := &hitList{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.mu.Lock()
		hits.hits = append(hits.hits, r.URL.Path)
		hits.mu.Unlock()
		// /<owner>/<repo>/archive/refs/heads/<branch>.tar.gz
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) == 6 && parts[2] == "archive" && parts[3] == "refs" && parts[4] == "heads" {
			key := parts[0] + "/" + parts[1] + "/" + strings.TrimSuffix(parts[5], ".tar.gz")
			if body, ok := archives[key]; ok {
				w.Header().Set("Content-Type", "application/x-gzip")
				_, _ = w.Write(body)
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	src, err := NewGitHubSource(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("NewGitHubSource: %v", err)
	}
	return src, hits
}

func TestPrepareGitHubArchiveInstalls(t *testing.T) {
	tarball := buildTarball(t, []tarEntry{
		{name: "widget-main/plugin.mjs", content: "export const plugin = {};\n"},
		{name: "widget-main/package.json", content: `{"name":"widget","version":"0.0.0"}`},
	})
	src, hits := fakeGitHub(t, map[string][]byte{"acme/widget/main": tarball})

	base := t.TempDir()
	bridge := fakeBridge(t)
	inst := NewInstaller(nil, filepath.Join(base, "scratch"))
	p, err := inst.Prepare(ctx, "github:acme/widget", Options{
		GitHub:           src,
		FinalPluginDir:   filepath.Join(base, "plugins", "widget"),
		BridgeExecutable: bridge,
	})
	if err != nil {
		t.Fatalf("Prepare github: %v", err)
	}
	defer inst.Cleanup(p)

	// 请求打到了 main 分支 archive。
	if got := hits.snapshot(); len(got) != 1 || !strings.Contains(got[0], "/acme/widget/archive/refs/heads/main.tar.gz") {
		t.Fatalf("archive requests = %v", got)
	}

	// 文件落位：payload 解到了 prepared root（tarball 的 widget-main/ 顶层被收敛）。
	for _, rel := range []string{"plugin.mjs", "package.json", "manifest.json"} {
		if _, err := os.Stat(filepath.Join(p.Root, rel)); err != nil {
			t.Fatalf("payload %s missing: %v", rel, err)
		}
	}

	// 记录：kind=github, package=owner/repo, version=branch, tarball URL + sha。
	rec := p.Record
	if rec.Kind != "github" {
		t.Fatalf("kind = %q, want github", rec.Kind)
	}
	if rec.Package != "acme/widget" {
		t.Fatalf("package = %q, want acme/widget", rec.Package)
	}
	if rec.Version != "main" {
		t.Fatalf("version = %q, want main (branch builds carry the branch)", rec.Version)
	}
	if !strings.Contains(rec.TarballURL, "/archive/refs/heads/main.tar.gz") {
		t.Fatalf("tarball_url = %q", rec.TarballURL)
	}
	if len(rec.SHA256) != 64 {
		t.Fatalf("sha256 = %q", rec.SHA256)
	}
	if rec.Enabled {
		t.Fatal("market installs must stay disabled (T3)")
	}
	if p.Manifest.ID != "widget" {
		t.Fatalf("manifest id = %q, want widget", p.Manifest.ID)
	}

	// SHA256 pin：不一致时拒绝。
	_, err = inst.Prepare(ctx, "github:acme/widget", Options{
		GitHub: src, ExpectedSHA256: strings.Repeat("0", 64),
		FinalPluginDir: filepath.Join(base, "plugins", "widget"), BridgeExecutable: bridge,
	})
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("pinned sha error = %v", err)
	}
}

func TestPrepareGitHubExplicitBranch(t *testing.T) {
	tarball := buildTarball(t, []tarEntry{{name: "widget-dev/plugin.mjs", content: "export {}"}})
	src, hits := fakeGitHub(t, map[string][]byte{"acme/widget/dev": tarball})
	inst := NewInstaller(nil, t.TempDir())
	p, err := inst.Prepare(ctx, "github:acme/widget@dev", Options{
		GitHub:           src,
		FinalPluginDir:   filepath.Join(t.TempDir(), "plugins", "widget"),
		BridgeExecutable: fakeBridge(t),
	})
	if err != nil {
		t.Fatalf("Prepare github@branch: %v", err)
	}
	defer inst.Cleanup(p)
	if got := hits.snapshot(); len(got) != 1 || !strings.Contains(got[0], "heads/dev.tar.gz") {
		t.Fatalf("branch requests = %v, want heads/dev.tar.gz", got)
	}
	if p.Record.Version != "dev" {
		t.Fatalf("version = %q, want dev", p.Record.Version)
	}
}

func TestPrepareGitHubMasterFallback(t *testing.T) {
	tarball := buildTarball(t, []tarEntry{{name: "old-master/plugin.mjs", content: "export {}"}})
	// 只有 master 有包：main 404 后必须回落。
	src, hits := fakeGitHub(t, map[string][]byte{"acme/old/master": tarball})
	inst := NewInstaller(nil, t.TempDir())
	p, err := inst.Prepare(ctx, "github:acme/old", Options{
		GitHub:           src,
		FinalPluginDir:   filepath.Join(t.TempDir(), "plugins", "old"),
		BridgeExecutable: fakeBridge(t),
	})
	if err != nil {
		t.Fatalf("Prepare with master fallback: %v", err)
	}
	defer inst.Cleanup(p)
	got := hits.snapshot()
	if len(got) != 2 || !strings.Contains(got[0], "heads/main.tar.gz") || !strings.Contains(got[1], "heads/master.tar.gz") {
		t.Fatalf("requests = %v, want main then master", got)
	}
	if p.Record.Version != "master" {
		t.Fatalf("version = %q, want master", p.Record.Version)
	}
}

func TestPrepareGitHubNotFoundIsDistinct(t *testing.T) {
	src, _ := fakeGitHub(t, nil) // 一律 404
	_, err := NewInstaller(nil, t.TempDir()).Prepare(ctx, "github:acme/ghost", Options{GitHub: src})
	if err == nil {
		t.Fatal("404 on every branch must fail")
	}
	if !errors.Is(err, ErrGitHubNotFound) {
		t.Fatalf("error = %v, want ErrGitHubNotFound", err)
	}
	if !strings.Contains(err.Error(), "main, master") {
		t.Fatalf("error = %v, want the tried branches", err)
	}
	// 显式分支只试一次，404 同样明确。
	_, err = NewInstaller(nil, t.TempDir()).Prepare(ctx, "github:acme/ghost@dev", Options{GitHub: src})
	if !errors.Is(err, ErrGitHubNotFound) || strings.Contains(err.Error(), "tried") {
		t.Fatalf("explicit branch 404 = %v, want a single-branch not-found", err)
	}
}

func TestPrepareGitHubUnreachableIsDistinct(t *testing.T) {
	// 服务已关 = 网络层失败 → ErrGitHubUnreachable（可配代理的提示保留）。
	src, _ := fakeGitHub(t, nil)
	url := "http://127.0.0.1:1" // 立即拒绝的端口
	dead, err := NewGitHubSource(url, nil)
	if err != nil {
		t.Fatalf("NewGitHubSource: %v", err)
	}
	_ = src
	inst := NewInstaller(nil, t.TempDir())
	_, err = inst.Prepare(ctx, "github:acme/widget", Options{GitHub: dead})
	if err == nil {
		t.Fatal("unreachable GitHub must fail")
	}
	if !errors.Is(err, ErrGitHubUnreachable) {
		t.Fatalf("error = %v, want ErrGitHubUnreachable", err)
	}
	text := err.Error()
	if !strings.Contains(text, "unreachable") || !strings.Contains(text, "proxy") {
		t.Fatalf("error text = %q, want the unreachable/proxy hint", text)
	}
}

func TestPrepareGitHubBadTarIsExtractionError(t *testing.T) {
	src, _ := fakeGitHub(t, map[string][]byte{"acme/bad/main": []byte("this is not a tar.gz")})
	_, err := NewInstaller(nil, t.TempDir()).Prepare(ctx, "github:acme/bad", Options{GitHub: src})
	if err == nil {
		t.Fatal("a corrupt archive must fail")
	}
	// 与 404 / 不可达区分：这是解包错误。
	if errors.Is(err, ErrGitHubNotFound) || errors.Is(err, ErrGitHubUnreachable) {
		t.Fatalf("bad tar misclassified: %v", err)
	}
	if !strings.Contains(err.Error(), "tarball") {
		t.Fatalf("error = %v, want a tarball extraction error", err)
	}
}

func TestPrepareGitHubTraversalStillRejected(t *testing.T) {
	tarball := buildTarball(t, []tarEntry{{name: "../escape.mjs", content: "x"}})
	src, _ := fakeGitHub(t, map[string][]byte{"acme/evil/main": tarball})
	_, err := NewInstaller(nil, t.TempDir()).Prepare(ctx, "github:acme/evil", Options{GitHub: src})
	if err == nil || !strings.Contains(err.Error(), "escape") {
		t.Fatalf("traversal error = %v", err)
	}
}

func TestPrepareGitHubInvalidOwnerOrRepo(t *testing.T) {
	src, _ := fakeGitHub(t, nil)
	// ParseSpec 只查形状；owner/repo 的 URL 注入防护在 Archive 里。
	for _, spec := range []string{"github:../etc/passwd/x", "github:acme/..%2f..%2fx", "github:acme/wi dget"} {
		if _, err := ParseSpec(spec); err != nil {
			continue // 形状层就拒了也算通过
		}
		inst := NewInstaller(nil, t.TempDir())
		if _, err := inst.Prepare(ctx, spec, Options{GitHub: src}); err == nil {
			t.Fatalf("spec %q must be rejected", spec)
		}
	}
}

func TestNewGitHubSourceValidation(t *testing.T) {
	if _, err := NewGitHubSource("", nil); err != nil {
		t.Fatalf("default base: %v", err)
	}
	if _, err := NewGitHubSource("ftp://x", nil); err == nil {
		t.Fatal("non-http base must be rejected")
	}
	if _, err := NewGitHubSource("https://", nil); err == nil {
		t.Fatal("hostless base must be rejected")
	}
}
