package main

import (
	"encoding/json"
	"strings"
)

// Model catalog for Cline, ported from cline-models.ts and cline-product.ts.
//
// Two remote sources feed it, and both matter for different reasons:
//   - /api/v1/ai/cline/recommended-models is anonymous and is the ONLY place the
//     `free` list comes from, so it decides what a user without credits can run.
//   - /api/v1/models is authenticated and is the authoritative id list, including
//     paid models the recommended list never mentions.
//
// models.dev's `cline-pass` block fills in context windows and image support that
// neither endpoint reports.

// freeIDPrefix and freeIDSuffix are the two naming conventions that mark a free
// model (cline-models.ts FREE_ID_PREFIX / FREE_ID_SUFFIX).
const (
	freeIDPrefix = "cline-free/"
	freeIDSuffix = ":free"
)

// fallbackModels are the free models the reference measured on 2026-10-05. They
// are used only when both remote lists are unreachable, so a cold network never
// leaves the model list empty.
var fallbackModels = []ModelInfo{
	{ID: "stealth/space-bunny-alpha", Name: "Space Bunny Alpha", ContextLength: 1000000, MaxTokens: 524288, SupportsImage: true, Free: true, Description: "Blazing-fast inference with 1M context"},
	{ID: "cline-free/mimo-v2.6-flash", Name: "MiMo-V2.6-Flash", ContextLength: 1048576, MaxTokens: 131072, SupportsImage: true, Free: true, Description: "Mixture-of-Experts architecture with 309B total parameters"},
	{ID: "cline-free/muse-spark-1.3-contributor", Name: "Muse Spark 1.3 Contributor", ContextLength: 1048576, MaxTokens: 943718, SupportsImage: true, Free: true, Description: "Meta's multimodal reasoning model for experimentation, learning, and early-stage agentic, multi-agent, and coding workflows."},
}

// ModelInfo is one catalog row in the shape the host's /v1/models parser reads.
type ModelInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextLength int    `json:"context_length,omitempty"`
	MaxTokens     int    `json:"max_tokens,omitempty"`
	SupportsImage bool   `json:"supports_image,omitempty"`
	Free          bool   `json:"free,omitempty"`
	Description   string `json:"description,omitempty"`
}

// recommendedModels is the parsed free/recommended/clinePass envelope.
type recommendedModels struct {
	Free        []recommendedEntry
	Recommended []recommendedEntry
	ClinePass   []recommendedEntry
}

// recommendedEntry is one row of a recommended-models list.
type recommendedEntry struct {
	ID          string
	Name        string
	Description string
}

// parseRecommendedModels projects the anonymous catalog envelope.
func parseRecommendedModels(body []byte) *recommendedModels {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return &recommendedModels{}
	}
	return &recommendedModels{
		Free:        parseEntryList(payload["free"]),
		Recommended: parseEntryList(payload["recommended"]),
		ClinePass:   parseEntryList(payload["clinePass"]),
	}
}

func parseEntryList(value any) []recommendedEntry {
	list, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]recommendedEntry, 0, len(list))
	for _, raw := range list {
		record, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id := stringField(record, "id")
		if id == "" {
			continue
		}
		out = append(out, recommendedEntry{
			ID:          id,
			Name:        stringField(record, "name"),
			Description: stringField(record, "description"),
		})
	}
	return out
}

// parseModelIDs extracts ids from the OpenAI-shaped authenticated list.
func parseModelIDs(body []byte) []string {
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return nil
	}
	ids := make([]string, 0, len(payload.Data))
	for _, d := range payload.Data {
		if id := strings.TrimSpace(d.ID); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// isFreeModel decides whether an id is free. Any of three signals is enough: it
// appeared in the remote free list, it carries the `:free` suffix, or it lives
// under the `cline-free/` namespace.
func isFreeModel(id string, remoteFree map[string]bool) bool {
	if remoteFree[id] {
		return true
	}
	if strings.HasSuffix(id, freeIDSuffix) || strings.HasPrefix(id, freeIDPrefix) {
		return true
	}
	for _, m := range fallbackModels {
		if m.ID == id && m.Free {
			return true
		}
	}
	return false
}

// nameFromID builds a readable name from an id, matching nameFromId.
func nameFromID(id string) string {
	tail := id
	if i := strings.Index(id, "/"); i >= 0 {
		tail = id[i+1:]
	}
	tail = strings.ReplaceAll(tail, freeIDSuffix, "")
	tail = strings.ReplaceAll(tail, "-", " ")
	words := strings.Fields(tail)
	for i, w := range words {
		if w == "" {
			continue
		}
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

// mergeModels combines the remote lists with the fallback table and the models.dev
// metadata. Precedence, as in the reference:
//   - free ids first (they are what an account without credits can actually use),
//   - then the fallback entries that the remote list still recognises,
//   - then the rest of the remote ids.
//
// A model that the remote list does not mention is dropped when the remote list is
// non-empty: offering it would promise a model the account cannot reach.
func mergeModels(remote *recommendedModels, remoteIDs []string, dev map[string]ModelInfo) []ModelInfo {
	fallbackIndex := map[string]ModelInfo{}
	for _, m := range fallbackModels {
		fallbackIndex[m.ID] = m
	}
	entries := map[string]recommendedEntry{}
	var order []string
	add := func(entry recommendedEntry) {
		if _, ok := entries[entry.ID]; !ok {
			order = append(order, entry.ID)
		}
		entries[entry.ID] = entry
	}
	freeIDs := map[string]bool{}
	for _, e := range remote.Free {
		freeIDs[e.ID] = true
		add(e)
	}
	all := append(append(append([]recommendedEntry{}, remote.Free...), remote.Recommended...), remote.ClinePass...)
	for _, e := range all {
		add(e)
	}
	for _, id := range remoteIDs {
		add(recommendedEntry{ID: id})
	}

	known := len(remoteIDs) > 0 || len(order) > 0
	out := make([]ModelInfo, 0, len(order)+len(fallbackModels))
	seen := map[string]bool{}
	build := func(id string) ModelInfo {
		entry := entries[id]
		fb := fallbackIndex[id]
		info := ModelInfo{
			ID:   id,
			Name: firstNonEmpty(fb.Name, entry.Name, nameFromID(id)),
			Free: isFreeModel(id, freeIDs),
		}
		if fb.ContextLength > 0 {
			info.ContextLength = fb.ContextLength
		}
		if fb.MaxTokens > 0 {
			info.MaxTokens = fb.MaxTokens
		}
		if fb.SupportsImage {
			info.SupportsImage = true
		}
		if info.Description = firstNonEmpty(entry.Description, fb.Description); info.Description == "" {
			info.Description = ""
		}
		// models.dev fills only what is still missing; an explicit local value wins.
		if devInfo, ok := dev[id]; ok {
			if info.ContextLength == 0 {
				info.ContextLength = devInfo.ContextLength
			}
			if info.Name == id && devInfo.Name != "" {
				info.Name = devInfo.Name
			}
			if devInfo.SupportsImage {
				info.SupportsImage = true
			}
		}
		return info
	}
	for _, id := range order {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, build(id))
	}
	for _, m := range fallbackModels {
		if seen[m.ID] {
			continue
		}
		if known && !freeIDs[m.ID] && entries[m.ID].ID == "" {
			continue
		}
		seen[m.ID] = true
		out = append(out, build(m.ID))
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
