package main

import (
	"sort"
	"strings"
)

// The OfficeAce loopback API currently reports thinking support as a boolean;
// these helpers read a tier list too, in case the account's model square starts
// carrying them, so no level is ever invented by the plugin.

var officeTierRank = map[string]int{
	"off": 0, "none": 1, "disabled": 2, "minimal": 3, "low": 4, "medium": 5,
	"high": 6, "xhigh": 7, "max": 8, "auto": 9, "on": 10,
}

func officeThinkingTiers(raw map[string]any) []string {
	if tc, ok := raw["thinking_config"].(map[string]any); ok {
		var tiers []string
		if en, ok := tc["enabled"].(map[string]any); ok {
			tiers = append(tiers, officeTierNames(en["efforts"])...)
			if len(tiers) == 0 {
				tiers = append(tiers, "on")
			}
		}
		if _, ok := tc["disabled"]; ok {
			tiers = append(tiers, "off")
		}
		if len(tiers) > 0 {
			return officeSortTiers(tiers)
		}
	}
	for _, key := range []string{"efforts", "supportedEfforts", "supported_efforts", "reasoningEfforts", "reasoning_efforts"} {
		if tiers := officeTierNames(raw[key]); len(tiers) > 0 {
			return officeSortTiers(tiers)
		}
	}
	return nil
}

func officeTierNames(v any) []string {
	var out []string
	switch typed := v.(type) {
	case []any:
		for _, item := range typed {
			switch entry := item.(type) {
			case string:
				out = append(out, entry)
			case map[string]any:
				for _, key := range []string{"id", "name", "key", "value", "effort", "level"} {
					if s, ok := entry[key].(string); ok {
						out = append(out, s)
						break
					}
				}
			}
		}
	case map[string]any:
		for key := range typed {
			out = append(out, key)
		}
	}
	filtered := out[:0]
	for _, s := range out {
		if s = normalizeOfficeTier(s); s != "" {
			filtered = append(filtered, s)
		}
	}
	return filtered
}

func normalizeOfficeTier(s string) string {
	switch s = strings.ToLower(s); s {
	case "", "true", "false":
		return ""
	default:
		return s
	}
}

func officeSortTiers(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, tier := range in {
		if !seen[tier] {
			seen[tier] = true
			out = append(out, tier)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := officeTierRank[out[i]], officeTierRank[out[j]]
		if ri == rj {
			return out[i] < out[j]
		}
		return ri < rj
	})
	return out
}
