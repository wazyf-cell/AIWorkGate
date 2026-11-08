package market

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Registry endpoints.
const (
	// DefaultRegistry is the official npm registry.
	DefaultRegistry = "https://registry.npmjs.org"
	// RegistryMirror is the domestic npm mirror the plan names first
	// (registry.npmmirror.com). NewRegistry("") defaults to DefaultRegistry;
	// pass RegistryMirror (or any other base) to switch.
	RegistryMirror = "https://registry.npmmirror.com"

	defaultRegistryTimeout = 30 * time.Second
)

// ErrNotFound is returned when the registry answers 404 (unknown package,
// unknown search term, stale tarball URL).
var ErrNotFound = errors.New("market: registry: not found")

// PackageMetadata is the npm registry metadata document of one package
// (GET /<name>).
type PackageMetadata struct {
	Name     string                    `json:"name"`
	DistTags map[string]string         `json:"dist-tags"`
	Versions map[string]PackageVersion `json:"versions"`
}

// PackageVersion is one entry of metadata.versions.
type PackageVersion struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Dist    Dist   `json:"dist"`
}

// Dist points at the tarball of a version. Shasum is the npm historical sha1;
// Integrity is the sha512 base64 form. Neither is the sha256 this package
// records, which is computed over the downloaded bytes instead.
type Dist struct {
	Tarball   string `json:"tarball"`
	Shasum    string `json:"shasum,omitempty"`
	Integrity string `json:"integrity,omitempty"`
}

// SearchResponse is the npm search envelope (GET /-/v1/search).
type SearchResponse struct {
	Objects []SearchObject `json:"objects"`
	Total   int            `json:"total"`
}

// SearchObject is one hit of the search envelope. Score carries npm's search
// ranking; Detail.Popularity is the 0..1 popularity signal the market list
// sorts by (task-50). Either nesting level may be missing or null — a missing
// score reads as 0, and Search keeps the hit rather than dropping it.
type SearchObject struct {
	Package SearchPackage `json:"package"`
	Score   *SearchScore  `json:"score"`
}

// SearchScore is the npm search score envelope; Detail may be absent or null.
type SearchScore struct {
	Detail *SearchScoreDetail `json:"detail"`
}

// SearchScoreDetail holds the three 0..1 sub-scores; only Popularity is used.
type SearchScoreDetail struct {
	Popularity float64 `json:"popularity"`
}

// SearchPackage carries the display fields of one npm search hit.
type SearchPackage struct {
	Name        string            `json:"name"`
	Version     string            `json:"version"`
	Description string            `json:"description,omitempty"`
	Links       map[string]string `json:"links,omitempty"`
	// Popularity is npm search's score.detail.popularity (0..1), copied from
	// the enclosing SearchObject by Search (task-50). omitempty keeps the
	// wire clean when the registry returned no score; 0 is never shown as a
	// download count anywhere.
	Popularity float64 `json:"popularity,omitempty"`
}

// Registry is the injected npm HTTP client. All network access of the market
// package goes through it: the base URL and the *http.Client are constructor
// arguments (tests hand it an httptest server), and the default client honours
// the environment proxy via http.ProxyFromEnvironment.
type Registry struct {
	base *url.URL
	http *http.Client
	ua   string
}

// NewRegistry builds a Registry. An empty base selects DefaultRegistry; pass
// RegistryMirror or any mirror URL to switch. A nil client gets a default with
// a 30s timeout and the system proxy.
func NewRegistry(base string, client *http.Client) (*Registry, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		base = DefaultRegistry
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("market: registry base %q: %w", base, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("market: registry base %q must be http(s)", base)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("market: registry base %q has no host", base)
	}
	if client == nil {
		client = &http.Client{
			Timeout: defaultRegistryTimeout,
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				IdleConnTimeout:     90 * time.Second,
				MaxIdleConnsPerHost: 8,
			},
		}
	}
	return &Registry{base: u, http: client, ua: "aiworkgate-market/0.1"}, nil
}

// Base returns the configured registry base URL (string form), mainly for
// diagnostics.
func (r *Registry) Base() string { return r.base.String() }

// Metadata fetches GET /<name> on the registry.
func (r *Registry) Metadata(ctx context.Context, pkg string) (PackageMetadata, error) {
	var out PackageMetadata
	if err := r.getJSON(ctx, pkg, nil, &out); err != nil {
		return out, err
	}
	if out.Name == "" && len(out.Versions) == 0 {
		return out, fmt.Errorf("market: registry metadata for %q is empty", pkg)
	}
	return out, nil
}

// Search runs the npm search API, returning at most limit hits ("", 0, 10 are
// the npm /-/v1/search text=size values; a negative size defaults to 10).
func (r *Registry) Search(ctx context.Context, query string, limit int) ([]SearchPackage, error) {
	q := url.Values{}
	q.Set("text", strings.TrimSpace(query))
	if limit <= 0 {
		limit = 10
	}
	q.Set("size", strconv.Itoa(limit))
	var out SearchResponse
	if err := r.getJSON(ctx, "/-/v1/search", q, &out); err != nil {
		return nil, err
	}
	hits := make([]SearchPackage, 0, len(out.Objects))
	for _, o := range out.Objects {
		pkg := o.Package
		// 缺 score / detail / popularity 一律按 0（指针为零值即未命中）。
		if o.Score != nil && o.Score.Detail != nil {
			pkg.Popularity = o.Score.Detail.Popularity
		}
		hits = append(hits, pkg)
	}
	return hits, nil
}

// Fetch downloads url (any host the registry package may point to) with a hard
// cap of maxBytes, returning the raw bytes. It is used for tarballs whose URL
// the registry hands out.
func (r *Registry) Fetch(ctx context.Context, rawURL string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = MaxTarballBytes
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("market: fetch %q: %w", rawURL, err)
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("User-Agent", r.ua)
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("market: fetch %q: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, rawURL)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("market: fetch %q: %s", rawURL, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("market: fetch %q: read: %w", rawURL, err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("market: fetch %q exceeds %d bytes", rawURL, maxBytes)
	}
	return data, nil
}

// getJSON runs one GET on the registry and decodes a 2xx body as JSON.
func (r *Registry) getJSON(ctx context.Context, path string, query url.Values, out any) error {
	u := *r.base
	u.Path = strings.TrimRight(u.Path, "/") + "/" + strings.TrimLeft(path, "/")
	if query != nil {
		u.RawQuery = query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("market: registry request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", r.ua)
	resp, err := r.http.Do(req)
	if err != nil {
		return fmt.Errorf("market: registry %s: %w", u.String(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s", ErrNotFound, u.String())
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("market: registry %s: %s", u.String(), resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("market: registry %s: read: %w", u.String(), err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("market: registry %s: decode: %w", u.String(), err)
	}
	return nil
}
