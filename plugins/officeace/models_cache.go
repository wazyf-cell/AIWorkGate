package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type modelJSONEntry struct {
	ID      string `json:"id"`
	BaseURL string `json:"baseUrl"`
	Name    string `json:"name"`
}

// modelsFromModelJSON reads %USERPROFILE%\.office-claw\users\<id>\model.json.
func modelsFromModelJSON(path string) ([]Model, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var root map[string][]modelJSONEntry
	if err := json.Unmarshal(b, &root); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	entries, ok := root["huawei-maas"]
	if !ok {
		for _, list := range root {
			entries = append(entries, list...)
		}
	}
	var out []Model
	for _, e := range entries {
		id := strings.TrimSpace(e.ID)
		if !validModelID(id) || nonTextModelID(id) {
			continue
		}
		out = append(out, Model{
			ID:      id,
			Name:    firstNonEmpty(e.Name, id),
			BaseURL: endpointForModelBase(e.BaseURL),
		})
	}
	return dedupeModels(out), nil
}

func loadModelCache(dir string) (modelCache, error) {
	var mc modelCache
	b, err := os.ReadFile(filepath.Join(dir, modelsCacheFileName))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return mc, nil
		}
		return mc, err
	}
	if err := json.Unmarshal(b, &mc); err != nil {
		return modelCache{}, fmt.Errorf("parse %s: %w", modelsCacheFileName, err)
	}
	return mc, nil
}

func saveModelCache(dir string, mc modelCache) error {
	b, err := json.MarshalIndent(mc, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, modelsCacheFileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func cacheFresh(mc modelCache, now time.Time) bool {
	if mc.FetchedAt <= 0 || len(mc.Models) == 0 {
		return false
	}
	return now.Sub(time.Unix(mc.FetchedAt, 0)) < modelsCacheTTL
}

// modelsSnapshotFunc is the catalog provider used by handleModels. It is a
// variable only so tests can stub the whole snapshot, including the
// error-plus-stale-cache combination.
var modelsSnapshotFunc = func(p *Plugin, ctx context.Context, force bool) ([]Model, string, error) {
	return p.modelsSnapshot(ctx, force)
}

// modelsSnapshot returns the catalog, refreshing it at most every 6h. On any
// refresh failure it serves the last successful cache rather than failing.
func (p *Plugin) modelsSnapshot(ctx context.Context, force bool) ([]Model, string, error) {
	now := p.now()
	p.modelsMu.RLock()
	cached := p.models
	cachedAt := p.modelsFetchedAt
	cachedSource := p.modelsSource
	p.modelsMu.RUnlock()
	if !force && len(cached) > 0 && !cachedAt.IsZero() && now.Sub(cachedAt) < modelsCacheTTL {
		return cached, cachedSource, nil
	}

	p.modelsFetchMu.Lock()
	defer p.modelsFetchMu.Unlock()
	if !force {
		// Another request may have refreshed while we waited for the lock.
		p.modelsMu.RLock()
		cached = p.models
		cachedAt = p.modelsFetchedAt
		cachedSource = p.modelsSource
		p.modelsMu.RUnlock()
		if len(cached) > 0 && !cachedAt.IsZero() && now.Sub(cachedAt) < modelsCacheTTL {
			return cached, cachedSource, nil
		}
	}

	var errs []string
	disc := p.discovery()
	cred, credErr := p.liveCredential(ctx)
	if credErr == nil && cred != nil && cred.SessionID != "" {
		live, err := p.local.listModels(ctx, cred.SessionID)
		if err == nil && len(live) > 0 {
			var out []Model
			for _, entry := range live {
				if m, ok := modelFromLocal(entry); ok {
					out = append(out, m)
				}
			}
			out = dedupeModels(out)
			if len(out) > 0 {
				// The live list only carries the enabled subset and often no
				// capability flags; merge the profile and model.json catalogs
				// so the resource page keeps every selectable model while
				// live metadata wins field by field.
				merged := out
				if profiles, perr := p.local.getProfiles(ctx, cred.SessionID); perr == nil {
					merged = mergeModels(merged, modelsFromProfiles(profiles))
				}
				for _, path := range userModelJSONPaths(disc, cred) {
					extra, eerr := modelsFromModelJSON(path)
					if eerr == nil {
						merged = mergeModels(merged, extra)
					}
				}
				merged = dedupeModels(merged)
				if rates, rerr := p.local.getTokenRates(ctx, cred.SessionID); rerr == nil {
					merged = applyMultipliers(merged, rates)
				}
				merged = applyStaticMetadata(merged, loadStaticCatalog(disc.Home))
				p.storeModels("merged", merged)
				return merged, "merged", nil
			}
			errs = append(errs, "本机模型列表为空")
		} else if err != nil {
			errs = append(errs, err.Error())
		}
		if profiles, err := p.local.getProfiles(ctx, cred.SessionID); err == nil {
			if out := modelsFromProfiles(profiles); len(out) > 0 {
				if rates, rerr := p.local.getTokenRates(ctx, cred.SessionID); rerr == nil {
					out = applyMultipliers(out, rates)
				}
				out = applyStaticMetadata(out, loadStaticCatalog(disc.Home))
				p.storeModels("local_profiles", out)
				return out, "local_profiles", nil
			}
		} else {
			errs = append(errs, err.Error())
		}
	} else if credErr != nil {
		errs = append(errs, credErr.Error())
	}

	for _, path := range userModelJSONPaths(disc, cred) {
		out, err := modelsFromModelJSON(path)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		if len(out) > 0 {
			out = applyStaticMetadata(out, loadStaticCatalog(disc.Home))
			p.logLine("info", "model catalog from model.json", map[string]any{"path": path, "count": len(out)})
			p.storeModels("model_json", out)
			return out, "model_json", nil
		}
	}

	if len(cached) > 0 {
		p.logLine("warn", "serving stale model cache", map[string]any{"count": len(cached), "errors": errs})
		return cached, "cache", nil
	}
	return nil, "", fmt.Errorf("模型目录不可用：%s", strings.Join(errs, "；"))
}

func (p *Plugin) storeModels(source string, models []Model) {
	now := p.now()
	p.modelsMu.Lock()
	p.models = models
	p.modelsSource = source
	p.modelsFetchedAt = now
	p.modelsMu.Unlock()
	if err := saveModelCache(p.DataDir, modelCache{FetchedAt: now.Unix(), Source: source, Models: models}); err != nil {
		p.logLine("warn", "save models cache failed", map[string]any{"error": err.Error()})
	}
}
