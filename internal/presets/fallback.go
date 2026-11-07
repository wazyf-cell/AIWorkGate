package presets

import (
	"context"
	"fmt"
	"strings"

	"aiworkgate/internal/model"
)

// CatalogEntry is one model resolved through the fallback chain (D-27⑤).
// Source records where the entry came from; it is what the UI needs to label
// "vendor 列表 / models.dev / 插件自带 / 本地快照" per D-49.
type CatalogEntry struct {
	ModelID       string
	DisplayName   string
	Vendor        string
	ContextLength int
	Vision        bool
	Efforts       []string
	Free          bool
	Source        Source
}

// ModelsDev is the injectable models.dev client. The real transport is a
// later card; the interface keeps the fallback chain testable offline.
type ModelsDev interface {
	Lookup(ctx context.Context, query string) ([]CatalogEntry, error)
}

// FallbackChain resolves one model id by walking the chain in order:
// vendor 列表 → models.dev → 插件自带 → 本地快照/缓存.
//
// The walk stops at the first source that knows the model; every source that
// was skipped because it did not know the model (or because it failed) is
// returned in Failed so the caller can log it. A vendor preset whose Models
// list names the model counts as a hit; a models.dev client that returns an
// error is recorded as failed and the walk continues.
type FallbackChain struct {
	// Vendor is the preset catalog (vendor sources).
	Vendor Bundle
	// ModelsDev is the models.dev client; nil skips that source.
	ModelsDev ModelsDev
	// Plugin is the model list a plugin reported (built-in catalog).
	Plugin []model.ModelInfo
	// Snapshot is the local snapshot/cache written by an earlier sync.
	Snapshot []model.ModelInfo
}

// Resolve walks the chain for one model id.
func (fc FallbackChain) Resolve(ctx context.Context, modelID string) (CatalogEntry, []Source, error) {
	id := strings.TrimSpace(modelID)
	if id == "" {
		return CatalogEntry{}, nil, fmt.Errorf("presets: resolve: model id is required")
	}

	// 1. vendor 列表.
	if hit, ok := fc.lookupVendor(id); ok {
		return hit, nil, nil
	}
	failed := []Source{}

	// 2. models.dev.
	if fc.ModelsDev != nil {
		entries, err := fc.ModelsDev.Lookup(ctx, id)
		switch {
		case err != nil:
			failed = append(failed, SourceModelsDev)
		case len(entries) > 0:
			for _, e := range entries {
				if strings.EqualFold(strings.TrimSpace(e.ModelID), id) {
					e.Source = SourceModelsDev
					return e, nil, nil
				}
			}
		}
	} else {
		failed = append(failed, SourceModelsDev)
	}

	// 3. 插件自带.
	if entry, ok := fc.lookupList(fc.Plugin, id); ok {
		entry.Source = SourcePlugin
		return entry, failed, nil
	}

	// 4. 本地快照/缓存.
	if entry, ok := fc.lookupList(fc.Snapshot, id); ok {
		entry.Source = SourceSnapshot
		return entry, failed, nil
	}

	return CatalogEntry{}, failed, fmt.Errorf(
		"presets: model %q not found in any source (failed: %s)", id, formatSources(failed))
}

func (fc FallbackChain) lookupVendor(id string) (CatalogEntry, bool) {
	for _, p := range fc.Vendor.ByKind(KindVendor) {
		for _, m := range p.Models {
			// D-36 Only: an allow-listed preset only vouches for its members.
			if !p.ExposesModel(m) {
				continue
			}
			if strings.EqualFold(strings.TrimSpace(m), id) {
				return CatalogEntry{
					ModelID:     id,
					DisplayName: FormatModelName("", p.Name, id),
					Vendor:      p.Name,
					Source:      SourceVendor,
				}, true
			}
		}
	}
	return CatalogEntry{}, false
}

func (fc FallbackChain) lookupList(list []model.ModelInfo, id string) (CatalogEntry, bool) {
	for _, m := range list {
		if strings.EqualFold(strings.TrimSpace(m.ID), id) {
			return CatalogEntry{
				ModelID:       m.ID,
				DisplayName:   FormatModelName("", m.Vendor, m.ID),
				Vendor:        m.Vendor,
				ContextLength: m.ContextLength,
				Vision:        m.Vision,
				Efforts:       append([]string(nil), m.Thinking...),
				Free:          m.Free,
			}, true
		}
	}
	return CatalogEntry{}, false
}

func formatSources(list []Source) string {
	if len(list) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(list))
	for _, s := range list {
		parts = append(parts, string(s))
	}
	return strings.Join(parts, ", ")
}
