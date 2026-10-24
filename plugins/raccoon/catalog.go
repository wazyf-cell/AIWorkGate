package main

import (
	"bytes"
	"fmt"
	"strings"
)

// raccoonModel is one published catalog entry, reduced to the fields the host
// understands. Ported from parseRaccoonModelCatalog + RACCOON_FALLBACK_MODELS.
type raccoonModel struct {
	ID            string   `json:"id"`
	Name          string   `json:"name,omitempty"`
	ContextLength int      `json:"context_length,omitempty"`
	Tools         bool     `json:"tools,omitempty"`
	Vision        bool     `json:"vision,omitempty"`
	Thinking      []string `json:"thinking,omitempty"`
}

// raccoonEfforts is RACCOON_REASONING_EFFORTS: the thinking switch is on/off
// only, not the usual low/medium/high ladder.
var raccoonEfforts = []string{"on", "off"}

// catalogEntry is one raw row of the model catalog. The multipliers are pointers
// so an absent field stays distinguishable from an explicit 0 (see
// raccoonDisplayName).
type catalogEntry struct {
	ID                  string   `json:"id"`
	Description         string   `json:"description"`
	Name                string   `json:"name"`
	ContextWindow       int      `json:"context_window"`
	ContextLength       int      `json:"context_length"`
	EffectiveMultiplier *float64 `json:"effectiveMultiplier"`
	BaseMultiplier      *float64 `json:"baseMultiplier"`
	InputModalities     []string `json:"input_modalities"`
	Capabilities        struct {
		InputModalities []string `json:"input_modalities"`
		Reasoning       bool     `json:"reasoning"`
	} `json:"capabilities"`
}

// parseCatalog converts the envelope-wrapped catalog into the host's list.
//
// Shape ported from parseRaccoonModelCatalog: the payload is
// {code:0, data:{...}} and the entries live under data. Only ids without "/"
// are publishable (the host drops those).
func parseCatalog(body []byte) ([]raccoonModel, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, nil
	}
	// Try the envelope first, then a bare object/array, so a shape change upstream
	// degrades to a parse error instead of a panic or a silent empty list.
	var env struct {
		Code int `json:"code"`
		Data struct {
			List   []catalogEntry `json:"list"`
			Models []catalogEntry `json:"models"`
		} `json:"data"`
	}
	var list []catalogEntry
	if err := jsonUnmarshalObject(trimmed, &env); err == nil {
		list = env.Data.List
		if len(list) == 0 {
			list = env.Data.Models
		}
	}
	if len(list) == 0 {
		var flat struct {
			List   []catalogEntry `json:"list"`
			Models []catalogEntry `json:"models"`
		}
		if err := jsonUnmarshalObject(trimmed, &flat); err == nil {
			list = flat.List
			if len(list) == 0 {
				list = flat.Models
			}
		}
	}
	if len(list) == 0 {
		if err := jsonUnmarshalArray(trimmed, &list); err != nil {
			return nil, fmt.Errorf("raccoon: catalog is not a model list: %w", err)
		}
	}

	out := make([]raccoonModel, 0, len(list))
	for _, e := range list {
		id := strings.TrimSpace(e.ID)
		if id == "" || strings.Contains(id, "/") {
			continue
		}
		window := e.ContextWindow
		if window == 0 {
			window = e.ContextLength
		}
		name := strings.TrimSpace(e.Description)
		if name == "" {
			name = strings.TrimSpace(e.Name)
		}
		if name == "" {
			name = id
		}
		out = append(out, raccoonModel{
			ID:            id,
			Name:          raccoonDisplayName(name, e.BaseMultiplier, e.EffectiveMultiplier),
			ContextLength: window,
			Tools:         true,
			Vision:        raccoonVision(e),
			Thinking:      raccoonEfforts,
		})
	}
	return out, nil
}

func raccoonVision(e catalogEntry) bool {
	mods := e.Capabilities.InputModalities
	if len(mods) == 0 {
		mods = e.InputModalities
	}
	for _, m := range mods {
		if strings.EqualFold(strings.TrimSpace(m), "image") {
			return true
		}
	}
	return false
}

// raccoonDisplayName ports raccoonDisplayName from raccoon.ts exactly:
//
//	if effective is absent (not a number) -> the raw name, untouched
//	if effective == 0                     -> name + " · 免费"
//	if base > effective                   -> name + " · xBase→xEffective"
//	otherwise                             -> name + " · xEffective"
//
// Absent must be distinguishable from an explicit 0, which is why the multipliers
// arrive as pointers: a missing JSON field decodes to 0, and treating that as
// "free" would mislabel every model that simply omits the field.
func raccoonDisplayName(name string, base, effective *float64) string {
	name = strings.TrimSpace(name)
	if effective == nil {
		return name
	}
	if *effective == 0 {
		return name + " · 免费"
	}
	if base != nil && *base > *effective {
		return fmt.Sprintf("%s · x%s→x%s", name, formatMultiplier(*base), formatMultiplier(*effective))
	}
	return fmt.Sprintf("%s · x%s", name, formatMultiplier(*effective))
}

// formatMultiplier ports formatMultiplier: 4 decimals, trailing zeros trimmed.
func formatMultiplier(v float64) string {
	s := fmt.Sprintf("%.4f", v)
	s = strings.TrimRight(s, "0")
	return strings.TrimRight(s, ".")
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
