package model

import (
	"encoding/json"
	"strings"
)

// ParseModelList decodes a GET /v1/models response into catalog entries.
//
// Three callers used to decode this payload independently (the plugin client, the
// host-side A client and the gateway's own passthrough), and they drifted: only
// one of them understood A's "display"/"rate"/"supports_image" spellings, and
// none understood "supported_efforts". A plugin that published its catalog in
// those spellings therefore showed up in the UI with vendor/multiplier/vision/
// thinking empty, while a field whose name happened to match (context_length)
// came through fine. That split — same response, half the fields alive — is the
// signature of a decoding gap, not of missing upstream data.
//
// The accepted spellings, in precedence order:
//
//	id            id
//	name          display, name
//	context       context_length, context_window, context_length_max
//	vision        vision, supports_image
//	multiplier    multiplier, rate          (0 keeps meaning "not published")
//	thinking      thinking, supported_efforts, reasoning_efforts, efforts
//	free          free
//
// Encoding variants are accepted too: the payload may be {"data":[...]},
// {"models":[...]} or a bare array.
//
// A body that is not JSON at all is an error, not an empty list: swallowing it
// would turn a broken upstream into "this model has no catalog metadata", which
// is the failure mode this parser exists to make visible.
func ParseModelList(body []byte) ([]ModelInfo, error) {
	type entry struct {
		ID            string   `json:"id"`
		Name          string   `json:"name"`
		Display       string   `json:"display"`
		ContextLength int      `json:"context_length"`
		ContextWindow int      `json:"context_window"`
		ContextMax    int      `json:"context_length_max"`
		Tools         bool     `json:"tools"`
		Vision        bool     `json:"vision"`
		SupportsImage bool     `json:"supports_image"`
		Vendor        string   `json:"vendor"`
		Multiplier    float64  `json:"multiplier"`
		Rate          float64  `json:"rate"`
		Thinking      []string `json:"thinking"`
		Efforts       []string `json:"supported_efforts"`
		ReasonEfforts []string `json:"reasoning_efforts"`
		PlainEfforts  []string `json:"efforts"`
		Free          bool     `json:"free"`
	}

	var payload struct {
		Data   []entry `json:"data"`
		Models []entry `json:"models"`
	}
	entries := []entry(nil)
	err := json.Unmarshal(body, &payload)
	if err == nil {
		entries = payload.Data
		if len(entries) == 0 {
			entries = payload.Models
		}
	}
	if len(entries) == 0 {
		var list []entry
		if listErr := json.Unmarshal(body, &list); listErr == nil {
			entries = list
			err = nil
		} else if err != nil {
			return nil, err
		}
	}

	out := make([]ModelInfo, 0, len(entries))
	for _, e := range entries {
		id := strings.TrimSpace(e.ID)
		if id == "" {
			continue
		}
		name := strings.TrimSpace(e.Display)
		if name == "" {
			name = strings.TrimSpace(e.Name)
		}
		ctxLen := e.ContextLength
		if ctxLen == 0 {
			ctxLen = e.ContextWindow
		}
		if ctxLen == 0 {
			ctxLen = e.ContextMax
		}
		multiplier := e.Multiplier
		if multiplier == 0 {
			multiplier = e.Rate
		}
		out = append(out, ModelInfo{
			ID:            id,
			Name:          name,
			ContextLength: ctxLen,
			Tools:         e.Tools,
			Vision:        e.Vision || e.SupportsImage,
			Vendor:        strings.TrimSpace(e.Vendor),
			Multiplier:    multiplier,
			Thinking:      firstTiers(e.Thinking, e.Efforts, e.ReasonEfforts, e.PlainEfforts),
			Free:          e.Free,
		})
	}
	return out, nil
}

// firstTiers returns the first non-empty tier list, so a model that reports both
// a canonical and a legacy spelling does not lose its levels.
func firstTiers(lists ...[]string) []string {
	for _, l := range lists {
		if len(l) > 0 {
			return l
		}
	}
	return nil
}
