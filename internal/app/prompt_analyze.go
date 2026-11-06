package app

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"aiworkgate/internal/gateway"
	"aiworkgate/internal/webui"
)

// promptAnalyze estimates a pasted request body's six-section token split
// (D-55). The window share is best-effort: an unknown model still returns the
// breakdown, only without context_window/window_share.
func (a *App) promptAnalyze(model string, body json.RawMessage) (any, error) {
	composition, err := gateway.AnalyzePrompt(body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", webui.ErrInvalidRequest, err)
	}
	model = strings.TrimSpace(model)
	if model == "" {
		var head struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &head)
		model = strings.TrimSpace(head.Model)
	}
	payload := map[string]any{
		"total_tokens": composition.TotalTokens,
		"sections":     composition.Sections,
		"estimated":    true,
		"model":        model,
	}
	if a.store == nil || model == "" {
		return payload, nil
	}
	pluginID, modelID, window := a.store.FindModelContextLength(model)
	if window <= 0 {
		return payload, nil
	}
	share := float64(composition.TotalTokens) / float64(window)
	payload["context_window"] = window
	payload["window_share"] = math.Round(share*10000) / 10000
	payload["resolved_plugin"] = pluginID
	payload["resolved_model"] = modelID
	return payload, nil
}
