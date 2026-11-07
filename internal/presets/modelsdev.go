package presets

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The models.dev catalog (D-27⑤ source 2). models.dev publishes its whole
// directory as one JSON document; the client below only fetches, parses and
// indexes it by vendor/model id — it never evaluates anything from the payload.
const (
	// DefaultModelsDevURL is the public catalog document.
	DefaultModelsDevURL = "https://models.dev/api.json"
	// DefaultModelsDevTimeout bounds one catalog fetch. The plan pins 15s: a
	// dead models.dev must slow the fallback chain down briefly, not hang it.
	DefaultModelsDevTimeout = 15 * time.Second
	// maxModelsDevBytes caps the response body. The real document is a few MB;
	// the cap only stops a hostile mirror from streaming forever.
	maxModelsDevBytes = 32 << 20
)

// ModelsDevDoc is the parsed shape of the models.dev document. Parsing is
// tolerant: a provider without models and a model without metadata both decode
// as empty structures and are skipped, so shape drift upstream degrades the
// directory instead of failing it.
type ModelsDevDoc map[string]ModelsDevProvider

// ModelsDevProvider is one top-level vendor entry of the document.
type ModelsDevProvider struct {
	ID     string                    `json:"id"`
	Name   string                    `json:"name"`
	Models map[string]ModelsDevModel `json:"models"`
}

// ModelsDevModel is one model entry of one provider.
type ModelsDevModel struct {
	ID         string              `json:"id"`
	Name       string              `json:"name"`
	Modalities ModelsDevModalities `json:"modalities"`
	Limit      ModelsDevLimit      `json:"limit"`
}

// ModelsDevModalities mirrors the doc's modalities object; a nil Input means
// "the doc did not say", which is not the same as text-only.
type ModelsDevModalities struct {
	Input []string `json:"input"`
}

// ModelsDevLimit mirrors the doc's limit object.
type ModelsDevLimit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

// ModelsDevClient is the real transport behind the ModelsDev interface of
// fallback.go. The base URL and the *http.Client are constructor arguments, so
// unit tests point it at an httptest server and never touch the network; the
// default client honours the environment proxy (http.ProxyFromEnvironment).
type ModelsDevClient struct {
	base string
	http *http.Client
	ua   string
}

// NewModelsDev builds a client. An empty base selects DefaultModelsDevURL; a
// nil client gets a default with the 15s timeout and the system proxy.
func NewModelsDev(base string, client *http.Client) (*ModelsDevClient, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		base = DefaultModelsDevURL
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("presets: models.dev base %q: %w", base, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("presets: models.dev base %q must be http(s)", base)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("presets: models.dev base %q has no host", base)
	}
	if client == nil {
		client = &http.Client{
			Timeout: DefaultModelsDevTimeout,
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				IdleConnTimeout:     90 * time.Second,
				MaxIdleConnsPerHost: 4,
			},
		}
	}
	return &ModelsDevClient{base: u.String(), http: client, ua: "aiworkgate-presets/0.1"}, nil
}

// Fetch downloads and decodes the catalog document. A transport failure, a
// non-2xx answer, an oversized body and undecodable JSON all return an error —
// the caller (FallbackChain) treats that as "source failed" and moves on; it is
// never fatal and never panics.
func (c *ModelsDevClient) Fetch(ctx context.Context) (ModelsDevDoc, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base, nil)
	if err != nil {
		return nil, fmt.Errorf("presets: models.dev request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.ua)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("presets: models.dev fetch %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("presets: models.dev fetch %s: %s", c.base, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelsDevBytes+1))
	if err != nil {
		return nil, fmt.Errorf("presets: models.dev fetch %s: read: %w", c.base, err)
	}
	if int64(len(body)) > maxModelsDevBytes {
		return nil, fmt.Errorf("presets: models.dev document exceeds %d bytes", maxModelsDevBytes)
	}
	var doc ModelsDevDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("presets: models.dev decode: %w", err)
	}
	if len(doc) == 0 {
		return nil, fmt.Errorf("presets: models.dev document is empty")
	}
	return doc, nil
}

// Lookup resolves one model id (D-27⑤ source 2). The query is a bare model id
// or a "vendor/model" pair; the match on both parts is case-insensitive and
// exact. Every vendor that knows the id is returned so the caller can pick; an
// unknown id returns an empty slice with no error (a clean miss, distinct from
// a fetch failure).
func (c *ModelsDevClient) Lookup(ctx context.Context, query string) ([]CatalogEntry, error) {
	doc, err := c.Fetch(ctx)
	if err != nil {
		return nil, err
	}
	return doc.Lookup(query), nil
}

// Lookup resolves query against an already fetched document.
func (d ModelsDevDoc) Lookup(query string) []CatalogEntry {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil
	}
	wantVendor := ""
	if i := strings.IndexByte(q, '/'); i > 0 {
		wantVendor, q = q[:i], q[i+1:]
	}
	var out []CatalogEntry
	for provID, prov := range d {
		vendor := strings.TrimSpace(prov.Name)
		if vendor == "" {
			vendor = strings.TrimSpace(prov.ID)
		}
		if vendor == "" {
			vendor = provID
		}
		if wantVendor != "" && !strings.EqualFold(vendor, wantVendor) && !strings.EqualFold(provID, wantVendor) {
			continue
		}
		for modelID, m := range prov.Models {
			if !strings.EqualFold(strings.TrimSpace(modelID), q) && !strings.EqualFold(strings.TrimSpace(m.ID), q) {
				continue
			}
			display := strings.TrimSpace(m.Name)
			if display == "" {
				display = strings.TrimSpace(modelID)
			}
			out = append(out, CatalogEntry{
				ModelID:       strings.TrimSpace(modelID),
				DisplayName:   FormatModelName(display, vendor, modelID),
				Vendor:        vendor,
				ContextLength: m.Limit.Context,
				Vision:        hasImageInput(m.Modalities.Input),
			})
		}
	}
	return out
}

func hasImageInput(list []string) bool {
	for _, m := range list {
		if strings.EqualFold(strings.TrimSpace(m), "image") {
			return true
		}
	}
	return false
}
