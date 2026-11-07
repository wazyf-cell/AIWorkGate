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

// PresetRegion is one row of a preset's region/plan sub-table (D-36 供应商
// Region/套餐子表): the same vendor behind several regional or plan endpoints.
// An empty BaseURL inherits the preset's own.
type PresetRegion struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	BaseURL    string `json:"base_url,omitempty"`
	ModelsPath string `json:"models_path,omitempty"`
	Notes      string `json:"notes,omitempty"`
}

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

	// D-36 fields (供应商 Region/套餐子表 + Only/NoList/EndpointHint):
	//   - Region is the region/plan sub-table (see PresetRegion).
	//   - Only is the vendor model allow-list: when non-empty, only these model
	//     ids count as vendor-known (a vendor whose /models leaks everything can
	//     still keep the fallback list honest).
	//   - NoList marks providers whose list endpoint must not be trusted
	//     (relays answering with junk /models): the catalog may only come from
	//     the vendor list, manual entries and the snapshot.
	//   - EndpointHint is the "换端点" hint shown when a vendor says the model
	//     is not on this API (D-27 otherAPI semantics), e.g. the Azure
	//     deployment-name rule.
	Region       []PresetRegion `json:"region,omitempty"`
	Only         []string       `json:"only,omitempty"`
	NoList       bool           `json:"no_list,omitempty"`
	EndpointHint string         `json:"endpoint_hint,omitempty"`
}

// Bundle is the embedded preset catalog.
type Bundle struct {
	Presets []Preset `json:"presets"`
	// PresetAliases (D-36 presetAliases 迁移): old/short preset ids keep
	// resolving to their canonical replacement, so renaming a preset id does
	// not orphan stored configs. Direct hits always win over aliases.
	PresetAliases map[string]string `json:"preset_aliases,omitempty"`
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
	if err := b.validateAliases(); err != nil {
		return Bundle{}, err
	}
	return b, nil
}

// Validate checks one preset's invariants, including the D-36 fields.
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
	if err := p.validateRegion(); err != nil {
		return err
	}
	for _, m := range p.Only {
		if strings.TrimSpace(m) == "" {
			return fmt.Errorf("presets: preset %q: only contains an empty model id", p.ID)
		}
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

// validateRegion checks the D-36 region/plan sub-table: ids and names are
// required and unique inside the preset, and an overridden base URL must still
// be http(s).
func (p Preset) validateRegion() error {
	seen := map[string]bool{}
	for _, r := range p.Region {
		id := strings.TrimSpace(r.ID)
		if id == "" {
			return fmt.Errorf("presets: preset %q: a region has no id", p.ID)
		}
		if strings.TrimSpace(r.Name) == "" {
			return fmt.Errorf("presets: preset %q: region %q has no name", p.ID, id)
		}
		if seen[id] {
			return fmt.Errorf("presets: preset %q: region %q is declared twice", p.ID, id)
		}
		seen[id] = true
		if u := strings.TrimSpace(r.BaseURL); u != "" && !strings.HasPrefix(u, "http") {
			return fmt.Errorf("presets: preset %q: region %q base_url %q must start with http(s)", p.ID, id, r.BaseURL)
		}
	}
	return nil
}

// validateAliases checks the D-36 presetAliases migration map: every alias
// must point at a real preset id and must not shadow one.
func (b Bundle) validateAliases() error {
	real := map[string]bool{}
	for _, p := range b.Presets {
		real[p.ID] = true
	}
	for alias, target := range b.PresetAliases {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			return fmt.Errorf("presets: an alias has an empty key")
		}
		if real[alias] {
			return fmt.Errorf("presets: alias %q shadows a real preset id", alias)
		}
		if !real[strings.TrimSpace(target)] {
			return fmt.Errorf("presets: alias %q points at unknown preset %q", alias, target)
		}
	}
	return nil
}

// ExposesModel applies the D-36 Only allow-list: an empty list admits every
// model, a set one only its (case-insensitive) members. It guards the vendor
// source of the fallback chain so a vendor preset cannot vouch for models it
// explicitly excluded.
func (p Preset) ExposesModel(modelID string) bool {
	if len(p.Only) == 0 {
		return true
	}
	id := strings.TrimSpace(modelID)
	for _, m := range p.Only {
		if strings.EqualFold(strings.TrimSpace(m), id) {
			return true
		}
	}
	return false
}

// Lookup finds a preset by id, resolving presetAliases (D-36) when the direct
// hit misses.
func (b Bundle) Lookup(id string) (Preset, bool) {
	id = strings.TrimSpace(id)
	if p, ok := b.lookupDirect(id); ok {
		return p, true
	}
	if target, ok := b.PresetAliases[id]; ok {
		return b.lookupDirect(strings.TrimSpace(target))
	}
	return Preset{}, false
}

func (b Bundle) lookupDirect(id string) (Preset, bool) {
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
