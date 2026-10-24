package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// ModelInfo is the OpenAI model entry returned by GET /v1/models.
type ModelInfo struct {
	ID            string   `json:"id"`
	Name          string   `json:"name,omitempty"`
	ContextLength int      `json:"context_length,omitempty"`
	Tools         bool     `json:"tools,omitempty"`
	Vision        bool     `json:"vision,omitempty"`
	Vendor        string   `json:"vendor,omitempty"`
	Multiplier    float64  `json:"multiplier,omitempty"`
	Thinking      []string `json:"thinking,omitempty"`
	Free          bool     `json:"free,omitempty"`
}

type modelCacheFile struct {
	FetchedAt     int64       `json:"fetched_at"`
	Models        []ModelInfo `json:"models"`
	BenefitModels []string    `json:"benefit_models,omitempty"`
}

// fallbackModels mirrors DEFAULT_MODELS from the reference implementation.
var fallbackModelIDs = []string{
	"GLM-5.2",
	"GLM-5.1",
	"GLM-5",
	"glm-5.3-flash",
	"openpangu-2.0-flash",
	"openpangu-2.0-pro",
	"deepseek-v4-flash",
	"deepseek-v4-pro",
	"deepseek-v4.1-flash",
}

var contextWindows = map[string]int{
	"GLM-5.2":             202752,
	"glm-5.3-flash":       1048576,
	"deepseek-v4-flash":   1048576,
	"deepseek-v4-pro":     1048576,
	"deepseek-v4.1-flash": 1000000,
}

var benefitFallback = []string{"glm-5.3-flash", "deepseek-v4.1-flash"}

func defaultModels() []ModelInfo {
	out := make([]ModelInfo, 0, len(fallbackModelIDs))
	for _, id := range fallbackModelIDs {
		out = append(out, ModelInfo{ID: id, Name: id, ContextLength: contextWindows[id], Tools: true})
	}
	return out
}

func defaultBenefitModels() []string { return append([]string(nil), benefitFallback...) }

func loadModelCache(dir string) modelCacheFile {
	var c modelCacheFile
	b, err := os.ReadFile(filepath.Join(dir, modelsCacheFileName))
	if err != nil {
		return c
	}
	_ = json.Unmarshal(b, &c)
	return c
}

func (p *Plugin) saveModelCache(models []ModelInfo, benefit []string) {
	c := modelCacheFile{FetchedAt: p.now().Unix(), Models: models, BenefitModels: benefit}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return
	}
	if err := writeFileAtomic(filepath.Join(p.DataDir, modelsCacheFileName), b); err != nil {
		p.logLine("warn", "save models cache failed", map[string]any{"error": err.Error()})
	}
}

func toBenefitSet(ids []string) map[string]bool {
	if len(ids) == 0 {
		return nil
	}
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

func (p *Plugin) modelsStale(nowUnix int64) bool {
	p.modelsMu.RLock()
	fetched := p.modelsFetched
	p.modelsMu.RUnlock()
	if fetched <= 0 {
		return true
	}
	return nowUnix-fetched >= int64(p.opts.ModelsCacheTTL/time.Second)
}

func (p *Plugin) modelSnapshot() ([]ModelInfo, map[string]bool) {
	p.modelsMu.RLock()
	defer p.modelsMu.RUnlock()
	models := append([]ModelInfo(nil), p.models...)
	benefit := make(map[string]bool, len(p.benefitModels))
	for k, v := range p.benefitModels {
		benefit[k] = v
	}
	return models, benefit
}

func (p *Plugin) isBenefitModel(model string) bool {
	_, set := p.modelSnapshot()
	if set[model] {
		return true
	}
	for _, id := range benefitFallback {
		if id == model {
			return true
		}
	}
	return false
}
