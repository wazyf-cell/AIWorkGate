package plugins

import (
	"strings"

	"aiworkgate/internal/model"
)

// Capability names introduced by contract v0.4 that select an endpoint family
// rather than a scheduling feature. The v0.3 names (upstream, accounts,
// checkin, ...) stay in internal/model; these two are reserved here so a
// manifest that already declares them is understood once the gateway routes
// those endpoints. Until then declaring them changes nothing.
const (
	// CapEmbeddings marks a plugin that can serve /v1/embeddings pass-through.
	CapEmbeddings = "embeddings"
	// CapImages marks a plugin that can serve /v1/images/* pass-through.
	CapImages = "images"
)

// KnownCapabilities is the vocabulary this host understands. Anything else in
// a manifest is accepted and simply never matches, which is indistinguishable
// from "the feature is off" — hence UnknownCapabilities.
var KnownCapabilities = []string{
	model.CapUpstream,
	model.CapAccounts,
	model.CapCheckin,
	model.CapQuota,
	model.CapLoginAPIKey,
	model.CapLoginOAuth,
	model.CapLoginPasteCode,
	model.CapLoginIDEPlugin,
	model.CapLoginLocalState,
	model.CapLoginCookie,
	model.CapRefresh,
	model.CapEgress,
	model.CapIdentityRequest,
	model.CapIdentityOS,
	CapEmbeddings,
	CapImages,
}

// UnknownCapabilities lists declared names outside KnownCapabilities, in
// declaration order and without duplicates. Matching follows HasCapability:
// surrounding whitespace is ignored, case is not.
func UnknownCapabilities(mf model.Manifest) []string {
	known := make(map[string]struct{}, len(KnownCapabilities))
	for _, name := range KnownCapabilities {
		known[name] = struct{}{}
	}
	var out []string
	seen := make(map[string]struct{}, len(mf.Capabilities))
	for _, raw := range mf.Capabilities {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if _, ok := known[name]; ok {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// HasCapability reports whether mf declares capability. Matching is
// case-sensitive and whitespace is ignored, because the manifest is written by
// hand and a stray space must not silently disable an endpoint family. An
// empty list means the plugin declares nothing (the v0.3 behaviour), so every
// lookup against it is false.
func HasCapability(mf model.Manifest, capability string) bool {
	want := strings.TrimSpace(capability)
	if want == "" {
		return false
	}
	for _, c := range mf.Capabilities {
		if strings.TrimSpace(c) == want {
			return true
		}
	}
	return false
}

// PluginCapabilities returns a copy of the capabilities declared by an
// installed plugin, or nil when the id is unknown. The copy keeps callers from
// aliasing the manifest slice held under the manager lock.
func (m *Manager) PluginCapabilities(id string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.plugins[id]
	if !ok {
		return nil
	}
	return append([]string(nil), p.manifest.Capabilities...)
}

// HasCapability reports whether the installed plugin id declares capability.
// An unknown plugin declares nothing.
func (m *Manager) HasCapability(id, capability string) bool {
	m.mu.Lock()
	p, ok := m.plugins[id]
	var mf model.Manifest
	if ok {
		mf = p.manifest
	}
	m.mu.Unlock()
	if !ok {
		return false
	}
	return HasCapability(mf, capability)
}
