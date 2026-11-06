package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Capability is one model's entry in the models.dev opencode catalog.
//
// Why a second source exists at all: /v1/models on the inference host returns
// only id/object/created/owned_by for every one of its ~85 rows (measured
// 2026-10-21), so it says nothing about context window, image input or
// reasoning levels. models.dev is the same catalog the official CLI reads
// (packages/core/src/models-dev.ts), which is why the reference points here too.
type Capability struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Modalities []string `json:"modalities"`
	Context    int      `json:"context_window"`
	MaxOutput  int      `json:"max_output_tokens"`
	Reasoning  bool     `json:"reasoning"`
	Efforts    []string `json:"efforts"`
	ToolCall   bool     `json:"tool_call"`
	IsFree     bool     `json:"is_free"`
}

// SupportsImage reports whether the model accepts image input.
func (c Capability) SupportsImage() bool {
	for _, m := range c.Modalities {
		if m == "image" {
			return true
		}
	}
	return false
}

const (
	// modelsDevURL is the capability source. Ported from opencode-product.ts:30.
	modelsDevURL = "https://models.dev/api.json"
	// modelsDevTTL matches the official CLI's 60 minutes (opencode-product.ts:32).
	modelsDevTTL = 60 * time.Minute
	// modelsDevTimeout bounds the whole download; the reference uses 20s.
	modelsDevTimeout = 20 * time.Second
	// capabilitiesCacheFile keeps the last good table across restarts, so a
	// models.dev outage degrades to a slightly stale table instead of nothing.
	capabilitiesCacheFile = "capabilities.json"
)

// measuredImageOverrides records what real requests actually did, which the
// upstream catalog gets wrong in both directions (ported from the reference's
// MEASURED_IMAGE_OVERRIDES, measured 2026-10-02).
var measuredImageOverrides = map[string]bool{
	"big-pickle":               true,
	"longcat-2.5-preview-free": false,
}

// capabilityCache holds the models.dev table with a TTL and a disk fallback.
type capabilityCache struct {
	mu      sync.RWMutex
	entries []Capability
	at      time.Time
	path    string
	client  *http.Client
	now     func() time.Time
}

func newCapabilityCache(dataDir string) *capabilityCache {
	return &capabilityCache{
		path: dataDir + stringPathSeparator() + capabilitiesCacheFile,
		client: &http.Client{
			Timeout: modelsDevTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		now: time.Now,
	}
}

// Get returns the capability table, refreshing it when the TTL has lapsed.
// A refresh failure is not an error: the stale in-memory (or on-disk) table is
// returned instead, because "could not reach models.dev" must not mean "model
// has no capabilities".
func (c *capabilityCache) Get() []Capability {
	c.mu.RLock()
	entries, at := c.entries, c.at
	c.mu.RUnlock()
	if len(entries) > 0 && c.now().Sub(at) < modelsDevTTL {
		return entries
	}
	if len(entries) == 0 {
		if disk := c.loadDisk(); len(disk) > 0 {
			c.mu.Lock()
			c.entries = disk
			c.mu.Unlock()
			entries = disk
		}
	}
	fresh, err := c.fetch()
	if err != nil || len(fresh) == 0 {
		return entries
	}
	c.mu.Lock()
	c.entries, c.at = fresh, c.now()
	c.mu.Unlock()
	c.saveDisk(fresh)
	return fresh
}

// Set replaces the table, used by tests and by the response to a manual reload.
func (c *capabilityCache) Set(entries []Capability) {
	c.mu.Lock()
	c.entries, c.at = entries, c.now()
	c.mu.Unlock()
}

// Find returns the capability for one model id.
func (c *capabilityCache) Find(model string) (Capability, bool) {
	for _, e := range c.Get() {
		if e.ID == model {
			return e, true
		}
	}
	return Capability{}, false
}

// fetch downloads and projects the opencode slice of models.dev.
func (c *capabilityCache) fetch() ([]Capability, error) {
	req, err := http.NewRequest(http.MethodGet, modelsDevURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("models.dev HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	return parseModelsDev(body)
}

// parseModelsDev projects {"opencode":{"models":{...}}} into capabilities.
func parseModelsDev(body []byte) ([]Capability, error) {
	var root struct {
		OpenCode struct {
			Models map[string]struct {
				Name       string `json:"name"`
				Modalities struct {
					Input []string `json:"input"`
				} `json:"modalities"`
				Limit struct {
					Context int `json:"context"`
					Output  int `json:"output"`
				} `json:"limit"`
				Reasoning        bool `json:"reasoning"`
				ReasoningOptions []struct {
					Type   string   `json:"type"`
					Values []string `json:"values"`
				} `json:"reasoning_options"`
				ToolCall bool `json:"tool_call"`
				Cost     struct {
					Input  *float64 `json:"input"`
					Output *float64 `json:"output"`
				} `json:"cost"`
			} `json:"models"`
		} `json:"opencode"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, err
	}
	if len(root.OpenCode.Models) == 0 {
		return nil, fmt.Errorf("models.dev has no opencode entry")
	}
	out := make([]Capability, 0, len(root.OpenCode.Models))
	for id, m := range root.OpenCode.Models {
		modalities := []string{"text"}
		for _, in := range m.Modalities.Input {
			if in == "image" {
				modalities = []string{"text", "image"}
				break
			}
		}
		if override, ok := measuredImageOverrides[id]; ok {
			if override {
				modalities = []string{"text", "image"}
			} else {
				modalities = []string{"text"}
			}
		}
		var efforts []string
		for _, opt := range m.ReasoningOptions {
			if opt.Type != "effort" {
				continue
			}
			for _, v := range opt.Values {
				if v != "" && !containsString(efforts, v) {
					efforts = append(efforts, v)
				}
			}
		}
		free := m.Cost.Input != nil && m.Cost.Output != nil && *m.Cost.Input == 0 && *m.Cost.Output == 0
		out = append(out, Capability{
			ID:         id,
			Name:       firstNonEmpty(m.Name, id),
			Modalities: modalities,
			Context:    m.Limit.Context,
			MaxOutput:  m.Limit.Output,
			// reasoning must agree with efforts: claiming true with no levels
			// would put an empty selector in the UI.
			Reasoning: m.Reasoning && len(efforts) > 0,
			Efforts:   efforts,
			ToolCall:  m.ToolCall,
			IsFree:    free,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (c *capabilityCache) loadDisk() []Capability {
	if strings.TrimSpace(c.path) == "" {
		return nil
	}
	data, err := readFileLimited(c.path, 8<<20)
	if err != nil {
		return nil
	}
	var payload struct {
		Version int          `json:"version"`
		At      int64        `json:"at"`
		Entries []Capability `json:"entries"`
	}
	if json.Unmarshal(data, &payload) != nil || payload.Version != 1 || len(payload.Entries) == 0 {
		return nil
	}
	c.mu.Lock()
	c.at = time.Unix(payload.At, 0)
	c.mu.Unlock()
	return payload.Entries
}

func (c *capabilityCache) saveDisk(entries []Capability) {
	if strings.TrimSpace(c.path) == "" {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"version": 1,
		"at":      c.now().Unix(),
		"entries": entries,
	})
	if err != nil {
		return
	}
	_ = writeFileAtomic(c.path, payload, 0o600)
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
