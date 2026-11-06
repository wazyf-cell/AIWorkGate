// Package presets holds the provider presets (P12-2, D-27②③) and the model
// naming / fallback-chain semantics they feed.
//
// Ported semantics (from magpie provider/presets.go, D-27②):
//   - presets are split into four kinds (vendor / relay / local / remote);
//   - remote presets carry a definition only — connecting to another magpie
//     is out of scope for this card (Q2-A kept the definition);
//   - a vendor preset carries the base URL template, the default models path
//     and the key label, so adding a vendor is "fill in the key" —
//     the preset list itself lives in presets.json, not in the host config.
//
// The naming rule (Q1-A / D-27③): a user-set name is stored separately from the
// vendor list and rendered as "自定义名 · 供应商" — FormatModelName.

package presets

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Kind is the preset family a provider falls into (D-27②).
type Kind string

const (
	// KindVendor is a model factory (openai, anthropic, deepseek, …).
	KindVendor Kind = "vendor"
	// KindRelay is a relay/aggregator the plan names second (a third-party
	// gateway fronting several vendors).
	KindRelay Kind = "relay"
	// KindLocal is the host's own loopback provider.
	KindLocal Kind = "local"
	// KindRemote is another magpie instance. Defined, not connected (Q2-A).
	KindRemote Kind = "remote"
)

// Valid reports whether the kind is one of the four.
func (k Kind) Valid() bool {
	switch k {
	case KindVendor, KindRelay, KindLocal, KindRemote:
		return true
	}
	return false
}

// Source is where a resolved catalog entry came from (D-27⑤ fallback chain).
type Source string

const (
	// SourceVendor is the preset list (offline).
	SourceVendor Source = "vendor"
	// SourceModelsDev is the public model catalog at models.dev.
	SourceModelsDev Source = "models_dev"
	// SourcePlugin is the model the plugin itself reported.
	SourcePlugin Source = "plugin"
	// SourceSnapshot is the local snapshot/cache written by a previous sync.
	SourceSnapshot Source = "snapshot"
)

// Preset is one provider definition.
type Preset struct {
	Kind       Kind   `json:"kind"`
	ID         string `json:"id"`
	Name       string `json:"name"`
	BaseURL    string `json:"base_url,omitempty"`
	ModelsPath string `json:"models_path,omitempty"`
	KeyLabel   string `json:"key_label,omitempty"`
	// Models is the default model id list a vendor preset declares. It feeds the
	// fallback chain's vendor source (D-27⑤); empty means "vendor declares no
	// default list" and the chain starts at models.dev.
	Models []string `json:"models,omitempty"`
	Notes  string   `json:"notes,omitempty"`
}

// Bundle is the embedded preset catalog.
type Bundle struct {
	Presets []Preset `json:"presets"`
}

// FormatModelName renders the "自定义名 · 供应商" display name (D-27③):
// the display name wins when set, the vendor label fills the right half.
// An empty display falls back to the model id.
func FormatModelName(display, vendor, modelID string) string {
	name := strings.TrimSpace(display)
	if name == "" {
		name = strings.TrimSpace(modelID)
	}
	if name == "" {
		return ""
	}
	vendor = strings.TrimSpace(vendor)
	if vendor == "" {
		return name
	}
	return fmt.Sprintf("%s · %s", name, vendor)
}

// Load decodes the embedded preset bundle and validates it.
func Load() (Bundle, error) {
	var b Bundle
	if err := json.Unmarshal(embedded, &b); err != nil {
		return Bundle{}, fmt.Errorf("presets: decode embedded bundle: %w", err)
	}
	for i := range b.Presets {
		if err := b.Presets[i].Validate(); err != nil {
			return Bundle{}, err
		}
	}
	return b, nil
}

// Validate checks one preset's invariants.
func (p Preset) Validate() error {
	if !p.Kind.Valid() {
		return fmt.Errorf("presets: preset %q: unknown kind %q", p.ID, p.Kind)
	}
	if strings.TrimSpace(p.ID) == "" {
		return fmt.Errorf("presets: preset %q: id is required", p.ID)
	}
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("presets: preset %q: name is required", p.ID)
	}
	if p.Kind == KindRemote {
		// remote is defined, not connected: no base URL is required.
		return nil
	}
	if strings.TrimSpace(p.BaseURL) == "" {
		return fmt.Errorf("presets: preset %q: base_url is required for %s", p.ID, p.Kind)
	}
	if !strings.HasPrefix(strings.TrimSpace(p.BaseURL), "http") {
		return fmt.Errorf("presets: preset %q: base_url %q must start with http(s)", p.ID, p.BaseURL)
	}
	return nil
}

// Lookup finds a preset by id.
func (b Bundle) Lookup(id string) (Preset, bool) {
	id = strings.TrimSpace(id)
	for _, p := range b.Presets {
		if p.ID == id {
			return p, true
		}
	}
	return Preset{}, false
}

// ByKind returns every preset of one kind, sorted by id.
func (b Bundle) ByKind(k Kind) []Preset {
	out := make([]Preset, 0, len(b.Presets))
	for _, p := range b.Presets {
		if p.Kind == k {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// DefaultModelsPath is the models path when a preset does not name one.
func (p Preset) DefaultModelsPath() string {
	if p.ModelsPath != "" {
		return p.ModelsPath
	}
	return "/v1/models"
}
