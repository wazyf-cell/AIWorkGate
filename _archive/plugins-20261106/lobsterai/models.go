package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// lobsteraiModel is one published model.
type lobsteraiModel struct {
	ID            string   `json:"id"`
	Name          string   `json:"name,omitempty"`
	ContextLength int      `json:"context_length,omitempty"`
	Tools         bool     `json:"tools,omitempty"`
	Vision        bool     `json:"vision,omitempty"`
	Thinking      []string `json:"thinking,omitempty"`
}

// rawModelRow is one row of GET /api/models/available. The field names are the
// reference's (modelId / modelName / contextWindow / maxTokens / supportsImage /
// thinkingConfig), which differ from the OpenAI catalog shape.
type rawModelRow struct {
	ModelID          string  `json:"modelId"`
	ModelName        string  `json:"modelName"`
	ContextWindow    int     `json:"contextWindow"`
	MaxTokens        int     `json:"maxTokens"`
	SupportsImage    bool    `json:"supportsImage"`
	SupportsThinking bool    `json:"supportsThinking"`
	CostMultiplier   float64 `json:"costMultiplier"`
	Description      string  `json:"description"`
	ThinkingConfig   struct {
		Levels []string `json:"levels"`
	} `json:"thinkingConfig"`
}

// parseModelsBody ports readLobsteraiModelArray + parseLobsteraiModels:
// code must be 0, the array is either `data` or `data.data`, the id field is
// `modelId` (not `id`) and the display name is `modelName`.
func parseModelsBody(body []byte) ([]lobsteraiModel, error) {
	var env struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("lobsterai: models envelope: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("lobsterai: models envelope code=%d", env.Code)
	}

	// data is either the array itself or {data: [...]}.
	var rows []rawModelRow
	if err := json.Unmarshal(env.Data, &rows); err != nil {
		var nested struct {
			Data []rawModelRow `json:"data"`
		}
		if err2 := json.Unmarshal(env.Data, &nested); err2 != nil {
			return nil, fmt.Errorf("lobsterai: models list is neither an array nor data.data: %w", err)
		}
		rows = nested.Data
	}

	out := make([]lobsteraiModel, 0, len(rows))
	for _, e := range rows {
		m, ok := publishModel(e.ModelID, e.ModelName, e.ContextWindow, e.SupportsImage, e.CostMultiplier)
		if !ok {
			continue
		}
		// Thinking levels travel with the model when the server reports them.
		if levels := e.ThinkingConfig.Levels; len(levels) > 0 {
			m.Thinking = levels
		}
		out = append(out, m)
	}
	return out, nil
}

// publishModel applies the reference's two filters (non-empty modelId, id without
// "/") and the cost-multiplier display suffix.
func publishModel(id, name string, contextWindow int, supportsImage bool, cost float64) (lobsteraiModel, bool) {
	id = strings.TrimSpace(id)
	if id == "" || strings.Contains(id, "/") {
		return lobsteraiModel{}, false
	}
	display := strings.TrimSpace(name)
	if display == "" {
		display = id
	}
	display = displayNameFor(display, cost)
	return lobsteraiModel{
		ID:            id,
		Name:          display,
		ContextLength: contextWindow,
		Tools:         true,
		Vision:        supportsImage,
	}, true
}

// displayNameFor ports displayNameFor: append " · xN" when a cost multiplier exists.
func displayNameFor(name string, cost float64) string {
	if cost <= 0 {
		return name
	}
	return fmt.Sprintf("%s · x%s", name, formatMultiplier(cost))
}

// formatMultiplier ports formatMultiplier.
func formatMultiplier(v float64) string {
	s := fmt.Sprintf("%.4f", v)
	s = strings.TrimRight(s, "0")
	return strings.TrimRight(s, ".")
}
