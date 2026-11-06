package main

import (
	"context"
	"encoding/json"
	"net/http"
)

// handleModels lists the models this plugin can serve.
//
// The inference host's own /v1/models is a name list only (id/object/created/
// owned_by on all ~85 rows, measured 2026-10-21), so metadata comes from the
// models.dev capability table. When that table is unavailable the metadata is
// left out rather than invented.
func (s *server) handleModels(w http.ResponseWriter, r *http.Request) {
	// The model list is fetched upstream when the capability table is cold, so it
	// needs the same fail-closed gate as chat: a blocked binding must not leak a
	// request out the direct route just because it is "only" a listing.
	if err := s.egressError(); err != nil {
		writeError(w, http.StatusServiceUnavailable, "egress_missing", err.Error())
		return
	}
	creds := s.store.current()
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   s.catalogModels(r.Context(), creds),
	})
}

// catalogModels merges the upstream name list with the models.dev metadata.
func (s *server) catalogModels(ctx context.Context, creds credentials) []map[string]any {
	caps := s.caps.Get()
	byID := make(map[string]Capability, len(caps))
	for _, c := range caps {
		byID[c.ID] = c
	}
	ids := []string{}
	if remote, err := s.client.catalog(ctx, creds.effectiveKey()); err == nil {
		ids = parseModelIDs(remote)
	}
	if len(ids) == 0 {
		// Upstream unreachable: fall back to the capability table's ids so the
		// model list is not blank. Reachability is not claimed either way.
		for _, c := range caps {
			ids = append(ids, c.ID)
		}
	}
	out := make([]map[string]any, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		entry := map[string]any{
			"id": id, "object": "model", "owned_by": "opencode", "name": id,
		}
		if c, ok := byID[id]; ok {
			entry["name"] = firstNonEmpty(c.Name, id)
			if c.Context > 0 {
				entry["context_length"] = c.Context
			}
			if c.MaxOutput > 0 {
				entry["max_output_tokens"] = c.MaxOutput
			}
			entry["vision"] = c.SupportsImage()
			entry["supports_image"] = c.SupportsImage()
			entry["tools"] = c.ToolCall
			if len(c.Efforts) > 0 {
				// Real levels only: an empty selector is worse than none.
				entry["thinking"] = c.Efforts
			}
			entry["free"] = c.IsFree
		}
		out = append(out, entry)
	}
	return out
}

// parseModelIDs extracts the ids from an OpenAI-shaped model list.
func parseModelIDs(body []byte) []string {
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	ids := make([]string, 0, len(payload.Data))
	for _, d := range payload.Data {
		if d.ID != "" {
			ids = append(ids, d.ID)
		}
	}
	return ids
}
