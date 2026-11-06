package main

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// The two CodeArts catalogs (gateway config + snap builtin) carry per-model
// capability records. Only id/name were read before; these helpers pull the
// thinking tiers, multiplier, context window, vision and free flags straight
// out of the upstream record so the resource page never guesses.

var tierRank = map[string]int{
	"off": 0, "none": 1, "disabled": 2, "minimal": 3, "low": 4, "medium": 5,
	"high": 6, "xhigh": 7, "max": 8, "auto": 9, "on": 10,
}

// thinkingTiers reads the tier list in declaration order and returns it sorted
// weakest → strongest, because Go maps lose the upstream key order.
func thinkingTiers(raw map[string]any) []string {
	if tc, ok := asMap(raw["thinking_config"]); ok {
		var tiers []string
		if en, ok := asMap(tc["enabled"]); ok {
			tiers = append(tiers, tierNames(en["efforts"])...)
			if len(tiers) == 0 {
				tiers = append(tiers, "on")
			}
		}
		if _, ok := tc["disabled"]; ok {
			tiers = append(tiers, "off")
		}
		if len(tiers) > 0 {
			return sortTiers(tiers)
		}
	}
	for _, key := range []string{"supported_efforts", "reasoning_efforts", "efforts", "reasoningEfforts", "thinking_levels", "thinkingLevels"} {
		if tiers := tierNames(raw[key]); len(tiers) > 0 {
			return sortTiers(tiers)
		}
	}
	if r, ok := asMap(raw["reasoning"]); ok {
		if tiers := tierNames(r["supportedEfforts"]); len(tiers) > 0 {
			return sortTiers(tiers)
		}
	}
	if caps, ok := asMap(raw["capabilities"]); ok {
		if tiers := tierNames(caps["efforts"]); len(tiers) > 0 {
			return sortTiers(tiers)
		}
	}
	return nil
}

func tierNames(v any) []string {
	switch typed := v.(type) {
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			switch entry := item.(type) {
			case string:
				if s := normalizeTier(entry); s != "" {
					out = append(out, s)
				}
			case map[string]any:
				for _, key := range []string{"id", "name", "key", "value", "effort", "level"} {
					if s, ok := entry[key].(string); ok {
						if n := normalizeTier(s); n != "" {
							out = append(out, n)
						}
						break
					}
				}
			}
		}
		return out
	case map[string]any:
		out := make([]string, 0, len(typed))
		for key := range typed {
			if s := normalizeTier(key); s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		out := make([]string, 0, len(typed))
		for _, s := range typed {
			if n := normalizeTier(s); n != "" {
				out = append(out, n)
			}
		}
		return out
	default:
		return nil
	}
}

func normalizeTier(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "", "true", "false":
		return ""
	}
	return s
}

func sortTiers(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, tier := range in {
		if !seen[tier] {
			seen[tier] = true
			out = append(out, tier)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := tierRank[out[i]], tierRank[out[j]]
		if ri == rj {
			return out[i] < out[j]
		}
		return ri < rj
	})
	return out
}

func numberField(raw map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		switch v := raw[key].(type) {
		case float64:
			if v > 0 {
				return v, true
			}
		case int:
			if v > 0 {
				return float64(v), true
			}
		case json.Number:
			if f, err := v.Float64(); err == nil && f > 0 {
				return f, true
			}
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f > 0 {
				return f, true
			}
		}
	}
	return 0, false
}

// priceFactor also reports whether the catalog explicitly priced the model at
// zero, which is how these catalogs mark a free model.
func priceFactor(raw map[string]any) (float64, bool) {
	value, ok := numberField(raw, "price_factor", "priceFactor", "cost_coefficient", "costCoefficient", "multiplier")
	if ok {
		return value, false
	}
	for _, key := range []string{"price_factor", "priceFactor", "cost_coefficient", "costCoefficient"} {
		if v, present := raw[key]; present {
			if f, err := toFloat(v); err == nil && f == 0 {
				return 0, true
			}
		}
	}
	return 0, false
}

func boolField(raw map[string]any, keys ...string) (bool, bool) {
	for _, key := range keys {
		if v, ok := raw[key].(bool); ok {
			return v, true
		}
	}
	return false, false
}

func contextLength(raw map[string]any) int {
	if v, ok := numberField(raw, "max_input_tokens", "context_window", "contextWindow", "context_length", "contextLength"); ok {
		return int(v)
	}
	if cc, ok := asMap(raw["context_config"]); ok {
		best := 0
		for key, entry := range cc {
			if n := parseTokenLabel(key); n > best {
				best = n
			}
			if m, ok := asMap(entry); ok {
				if v, ok := numberField(m, "tokens", "size", "max_input_tokens", "value"); ok && int(v) > best {
					best = int(v)
				}
			}
		}
		if best > 0 {
			return best
		}
	}
	return 0
}

func parseTokenLabel(s string) int {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" {
		return 0
	}
	unit := 1
	switch s[len(s)-1] {
	case 'M':
		unit = 1024 * 1024
		s = s[:len(s)-1]
	case 'K':
		unit = 1024
		s = s[:len(s)-1]
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return 0
	}
	return n * unit
}

func visionSupported(raw map[string]any) (bool, bool) {
	if v, ok := boolField(raw, "supports_image", "supportsImage", "vision", "vl", "is_vl"); ok {
		return v, true
	}
	if modalities, ok := asMap(raw["modalities"]); ok {
		if inputs, ok := modalities["input"].([]any); ok {
			for _, item := range inputs {
				if s, ok := item.(string); ok && strings.EqualFold(s, "image") {
					return true, true
				}
			}
		}
	}
	if inputs, ok := raw["input_modalities"].([]any); ok {
		for _, item := range inputs {
			if s, ok := item.(string); ok && strings.EqualFold(s, "image") {
				return true, true
			}
		}
	}
	return false, false
}

func asMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func toFloat(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case int:
		return float64(n), nil
	case json.Number:
		return n.Float64()
	default:
		return 0, strconv.ErrSyntax
	}
}
