package market

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// D-04 GitHub source: a github:owner/repo spec installs the branch archive
// tarball (https://github.com/<owner>/<repo>/archive/refs/heads/<branch>.tar.gz
// — the simplest of the two shapes the plan names, no release lookup needed).
// The download reuses the npm pipeline's tar.gz extractor (extract.go carries
// the traversal/symlink/size protection), so a GitHub package goes through
// exactly the same security gates as an npm one.

const (
	// DefaultGitHubBase is the archive host. Override it in tests with an
	// httptest server; a mirror is just another base.
	DefaultGitHubBase = "https://github.com"
	// DefaultGitHubTimeout bounds one archive download.
	DefaultGitHubTimeout = 60 * time.Second

	// defaultGitHubBranches is the default branch order when the spec names
	// none: GitHub's default is main, older repos still use master.
	defaultGitHubBranches = "main,master"
)

// githubRefPattern guards owner/repo/branch before they are concatenated into
// a URL: word chars, dots, dashes, underscores only, no leading dot — this
// rejects path separators, "..", userinfo tricks and spaces.
var githubRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// validGitHubRef reports whether one URL component is safe.
func validGitHubRef(s string) bool { return githubRefPattern.MatchString(s) }

// GitHubSource is the injected GitHub archive client. All network access of
// the GitHub source goes through it: base URL and *http.Client are constructor
// arguments (tests hand it an httptest server), and the default client honours
// the environment proxy via http.ProxyFromEnvironment — the same rule the npm
// Registry applies, so a machine whose proxy reaches github.com just works.
type GitHubSource struct {
	base *url.URL
	http *http.Client
	ua   string
}

// NewGitHubSource builds a GitHub source. An empty base selects
// DefaultGitHubBase; a nil client gets a default with a 60s timeout and the
// system proxy.
func NewGitHubSource(base string, client *http.Client) (*GitHubSource, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		base = DefaultGitHubBase
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("market: github base %q: %w", base, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("market: github base %q must be http(s)", base)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("market: github base %q has no host", base)
	}
	if client == nil {
		client = &http.Client{
			Timeout: DefaultGitHubTimeout,
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				IdleConnTimeout:     90 * time.Second,
				MaxIdleConnsPerHost: 8,
			},
		}
	}
	return &GitHubSource{base: u, http: client, ua: "aiworkgate-market/0.1"}, nil
}

// githubRefs validates one owner/repo/branch component for URL assembly.
func githubRefs(parts ...string) error {
	for _, p := range parts {
		if !githubRefPattern.MatchString(p) {
			return fmt.Errorf("market: github %q is not a valid owner/repo/branch", p)
		}
	}
	return nil
}

// ArchiveURL renders the branch archive URL for owner/repo/branch.
func (g *GitHubSource) ArchiveURL(owner, repo, branch string) string {
	return strings.TrimSuffix(g.base.String(), "/") + "/" + owner + "/" + repo + "/archive/refs/heads/" + branch + ".tar.gz"
}

// Archive downloads the branch tarball with the MaxTarballBytes cap. Transport
// errors wrap ErrGitHubUnreachable, HTTP 404 wraps ErrGitHubNotFound, any other
// non-2xx is a plain status error; a bad body only fails later, at extraction.
func (g *GitHubSource) Archive(ctx context.Context, owner, repo, branch string, maxBytes int64) ([]byte, error) {
	if err := githubRefs(owner, repo, branch); err != nil {
		return nil, err
	}
	if maxBytes <= 0 {
		maxBytes = MaxTarballBytes
	}
	raw := g.ArchiveURL(owner, repo, branch)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrGitHubUnreachable, raw, err)
	}
	req.Header.Set("Accept", "application/x-gtar, application/octet-stream")
	req.Header.Set("User-Agent", g.ua)
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrGitHubUnreachable, raw, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s", ErrGitHubNotFound, raw)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("market: github fetch %s: %s", raw, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("market: github fetch %s: read: %w", raw, err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("market: github fetch %s exceeds %d bytes", raw, maxBytes)
	}
	return data, nil
}

// prepareGitHub runs download -> sha256 -> extract -> manifest for a GitHub
// branch archive. When the spec names no branch, main is tried first and
// master second; an explicit branch is tried exactly once.
func (in *Installer) prepareGitHub(ctx context.Context, spec Spec, opts Options) (*Prepared, error) {
	src, err := in.githubSourceOf(opts)
	if err != nil {
		return nil, err
	}
	branches := []string{spec.Branch}
	if strings.TrimSpace(spec.Branch) == "" {
		branches = strings.Split(defaultGitHubBranches, ",")
	}
	var (
		data     []byte
		branch   string
		notFound error
		tried    []string
	)
	for _, b := range branches {
		data, err = src.Archive(ctx, spec.Owner, spec.Repo, b, MaxTarballBytes)
		if err == nil {
			branch = b
			break
		}
		if errors.Is(err, ErrGitHubNotFound) && len(branches) > 1 {
			notFound = err
			tried = append(tried, b)
			continue
		}
		return nil, err // unreachable / other status: fail with the real cause
	}
	if branch == "" {
		return nil, fmt.Errorf("%w for %s/%s (tried %s)", notFound, spec.Owner, spec.Repo, strings.Join(tried, ", "))
	}
	archiveURL := src.ArchiveURL(spec.Owner, spec.Repo, branch)
	sha := sha256Hex(data)
	if opts.ExpectedSHA256 != "" && !strings.EqualFold(sha, opts.ExpectedSHA256) {
		return nil, fmt.Errorf("market: github %s/%s@%s archive sha256 mismatch: got %s, want %s", spec.Owner, spec.Repo, branch, sha, opts.ExpectedSHA256)
	}

	pluginID, err := PluginIDFor(spec.Repo)
	if err != nil {
		return nil, err
	}
	staging, err := in.newStaging(pluginID, opts)
	if err != nil {
		return nil, err
	}
	okSoFar := false
	defer func() {
		if !okSoFar {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := extractTarball(data, staging); err != nil {
		return nil, err
	}
	payloadRoot, err := findPayloadRoot(staging)
	if err != nil {
		return nil, err
	}
	// The bridge manifest wants a display name; GitHub specs have none, so the
	// canonical owner/repo stands in (the seed's Spec copy only).
	seedSpec := spec
	seedSpec.Name = spec.Owner + "/" + spec.Repo
	manifest, generated, err := in.ensureManifest(payloadRoot, manifestSeed{
		Spec: seedSpec, PluginID: pluginID, Version: branch, Opts: opts,
	})
	if err != nil {
		return nil, err
	}
	if generated {
		if err := deployBridgeExecutable(opts, payloadRoot, manifest); err != nil {
			return nil, err
		}
	}
	if err := writeManifestFile(payloadRoot, manifest); err != nil {
		return nil, err
	}
	record := InstallRecord{
		Source:      spec.String(),
		Kind:        spec.Kind.String(),
		Package:     spec.Owner + "/" + spec.Repo,
		Version:     branch,
		TarballURL:  archiveURL,
		SHA256:      sha,
		PluginID:    manifest.ID,
		Enabled:     false,
		InstalledAt: in.nowUnix(),
	}
	if record.Version == "" {
		record.Version = branch
	}
	okSoFar = true
	return &Prepared{Root: payloadRoot, Manifest: manifest, Record: record}, nil
}
