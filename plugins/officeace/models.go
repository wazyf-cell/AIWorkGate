package main

import (
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	modelsCacheFileName = "models-cache.json"
	modelsCacheTTL      = 6 * time.Hour
)

var modelIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)

// Model is a normalized catalog entry. Capability fields are pointers so
// unknown capabilities are omitted instead of being invented.
type Model struct {
	ID            string   `json:"id"`
	Name          string   `json:"name,omitempty"`
	Vendor        string   `json:"vendor,omitempty"`
	ContextLength int      `json:"context_length,omitempty"`
	Tools         *bool    `json:"tools,omitempty"`
	Vision        *bool    `json:"vision,omitempty"`
	Multiplier    float64  `json:"multiplier,omitempty"`
	Thinking      []string `json:"thinking,omitempty"`
	Free          bool     `json:"free,omitempty"`
	BaseURL       string   `json:"base_url,omitempty"`
}

type modelCache struct {
	FetchedAt int64   `json:"fetched_at"`
	Source    string  `json:"source"`
	Models    []Model `json:"models"`
}

func boolPtr(v bool) *bool { return &v }

func validModelID(id string) bool {
	return id != "" && len(id) <= 128 && modelIDPattern.MatchString(id)
}

// nonTextModelID is a conservative denylist for ids that are obviously not
// chat/text-generation models. Capability flags are never synthesized from it.
func nonTextModelID(id string) bool {
	s := strings.ToLower(id)
	for _, needle := range []string{"embedding", "embed-", "-embed", "rerank", "bge-", "m3e-", "text-to-image", "stable-diffusion", "tts-", "-tts", "whisper", "asr-"} {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// modelFromLocal converts a live /api/maas-models entry.
func modelFromLocal(entry localModel) (Model, bool) {
	if entry.Enabled != nil && !*entry.Enabled {
		return Model{}, false
	}
	kind := strings.ToLower(entry.Kind)
	if kind == "embedding" || kind == "rerank" || kind == "asr" || kind == "tts" {
		return Model{}, false
	}
	id := strings.TrimSpace(entry.Model)
	if id == "" {
		id = strings.TrimSpace(entry.ID)
	}
	if !validModelID(id) || nonTextModelID(id) {
		return Model{}, false
	}
	m := Model{
		ID:            id,
		Name:          firstNonEmpty(entry.DisplayName, entry.Name, id),
		Tools:         entry.ToolCallingSupported,
		BaseURL:       endpointForModelBase(entry.BaseURL),
		ContextLength: 0,
	}
	for _, p := range []*float64{entry.ContextWindow, entry.ContextLength, entry.ContextLengthAlt} {
		if p != nil && *p > 0 {
			m.ContextLength = int(*p)
			break
		}
	}
	if entry.Capabilities != nil {
		if m.Tools == nil {
			for _, key := range []string{"toolCalling", "tool_calling", "tools"} {
				if v, ok := entry.Capabilities[key].(bool); ok {
					m.Tools = boolPtr(v)
					break
				}
			}
		}
		if v, ok := entry.Capabilities["vision"].(bool); ok {
			m.Vision = boolPtr(v)
		}
	}
	for _, modality := range entry.InputModalities {
		switch strings.ToLower(modality) {
		case "image", "images", "vision":
			m.Vision = boolPtr(true)
		}
	}
	raw := map[string]any{
		"thinking_config":  entry.ThinkingConfig,
		"efforts":          entry.Efforts,
		"supportedEfforts": entry.SupportedEfforts,
		"reasoningEfforts": entry.ReasoningEfforts,
		"isReasoning":      entry.IsReasoning,
		"isFree":           entry.IsFree,
		"priceFactor":      entry.PriceFactor,
		"costCoefficient":  entry.CostCoefficient,
		"modelSeries":      entry.ModelSeries,
	}
	if caps := entry.Capabilities; caps != nil {
		for key, value := range caps {
			if _, ok := raw[key]; !ok {
				raw[key] = value
			}
		}
		m.Thinking = append(m.Thinking, officeThinkingTiers(caps)...)
	}
	if tiers := officeThinkingTiers(raw); len(tiers) > 0 {
		m.Thinking = tiers
	} else if len(m.Thinking) > 0 {
		m.Thinking = officeSortTiers(m.Thinking)
	}
	if entry.IsFree != nil {
		m.Free = *entry.IsFree
	}
	if price := entry.PriceFactor; price != nil && m.Multiplier == 0 {
		m.Multiplier = *price
		if *price == 0 && entry.IsFree == nil {
			m.Free = true
		}
	}
	if entry.ModelSeries != "" {
		m.Vendor = entry.ModelSeries
	}
	return m, true
}

// modelsFromProfiles derives a catalog from /api/model-config-profiles when
// /api/maas-models has no entries.
func modelsFromProfiles(p *localProfiles) []Model {
	if p == nil {
		return nil
	}
	var out []Model
	for _, provider := range p.Providers {
		if provider.Enabled != nil && !*provider.Enabled {
			continue
		}
		ids := append([]string(nil), provider.Models...)
		if len(ids) == 0 {
			for id := range provider.ModelBaseURLs {
				ids = append(ids, id)
			}
			sort.Strings(ids)
		}
		for _, raw := range ids {
			id := strings.TrimSpace(raw)
			if !validModelID(id) || nonTextModelID(id) {
				continue
			}
			name := id
			if n := strings.TrimSpace(provider.ModelDisplayName[id]); n != "" {
				name = n
			}
			out = append(out, Model{
				ID:      id,
				Name:    name,
				BaseURL: endpointForModelBase(provider.ModelBaseURLs[id]),
			})
		}
	}
	return dedupeModels(out)
}

func dedupeModels(in []Model) []Model {
	seen := map[string]int{}
	out := make([]Model, 0, len(in))
	for _, m := range in {
		if idx, ok := seen[m.ID]; ok {
			// Prefer the richer duplicate.
			if out[idx].BaseURL == "" && m.BaseURL != "" {
				out[idx] = m
			}
			continue
		}
		seen[m.ID] = len(out)
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// mergeModels unions by id, keeping base entries and filling only fields the
// base does not know yet. Capability pointers stay nil when every source is
// silent, so the host never shows invented values.
func mergeModels(base, extra []Model) []Model {
	index := make(map[string]int, len(base))
	out := append([]Model(nil), base...)
	for i, m := range out {
		index[m.ID] = i
	}
	for _, m := range extra {
		if m.ID == "" {
			continue
		}
		if i, ok := index[m.ID]; ok {
			if out[i].Name == "" || out[i].Name == out[i].ID {
				if m.Name != "" {
					out[i].Name = m.Name
				}
			}
			if out[i].BaseURL == "" {
				out[i].BaseURL = m.BaseURL
			}
			if out[i].ContextLength == 0 {
				out[i].ContextLength = m.ContextLength
			}
			if out[i].Tools == nil {
				out[i].Tools = m.Tools
			}
			if out[i].Vision == nil {
				out[i].Vision = m.Vision
			}
			if out[i].Multiplier == 0 {
				out[i].Multiplier = m.Multiplier
			}
			if out[i].Vendor == "" {
				out[i].Vendor = m.Vendor
			}
			if len(out[i].Thinking) == 0 {
				out[i].Thinking = m.Thinking
			}
			if !out[i].Free {
				out[i].Free = m.Free
			}
			continue
		}
		index[m.ID] = len(out)
		out = append(out, m)
	}
	return out
}

// applyMultipliers copies OfficeAce's published credit multipliers onto the
// catalog. Missing rates stay zero so the host renders "—".
func applyMultipliers(models []Model, rates map[string]float64) []Model {
	if len(rates) == 0 {
		return models
	}
	for i := range models {
		if rate, ok := rates[models[i].ID]; ok {
			models[i].Multiplier = rate
		}
	}
	return models
}
