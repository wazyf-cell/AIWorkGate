package app

import (
	"encoding/json"
	"testing"

	"aiworkgate/internal/model"
)

func TestPromptAnalyzeResolvesContextWindow(t *testing.T) {
	app, st := newSchedulerApp(t)
	if err := st.ReplaceModelCatalog("codearts", []model.ModelCatalogEntry{
		{ModelID: "openai/gpt-4o", ContextLength: 128000},
	}); err != nil {
		t.Fatalf("ReplaceModelCatalog: %v", err)
	}

	body := json.RawMessage(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`)
	payload, err := app.promptAnalyze("", body)
	if err != nil {
		t.Fatalf("promptAnalyze: %v", err)
	}
	result := payload.(map[string]any)
	if result["model"] != "gpt-4o" {
		t.Fatalf("model = %v, want the body's model", result["model"])
	}
	if result["context_window"] != 128000 {
		t.Fatalf("context_window = %v, want 128000", result["context_window"])
	}
	if result["resolved_model"] != "openai/gpt-4o" {
		t.Fatalf("resolved_model = %v", result["resolved_model"])
	}
	if share, _ := result["window_share"].(float64); share <= 0 || share >= 1 {
		t.Fatalf("window_share = %v, want a small positive fraction", result["window_share"])
	}
	if result["estimated"] != true {
		t.Fatalf("estimated = %v, want true", result["estimated"])
	}
}

func TestPromptAnalyzeUnknownModelKeepsBreakdown(t *testing.T) {
	app, _ := newSchedulerApp(t)
	payload, err := app.promptAnalyze("nobody/knows-this", json.RawMessage(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("promptAnalyze: %v", err)
	}
	result := payload.(map[string]any)
	if _, ok := result["context_window"]; ok {
		t.Fatalf("context_window present for an unknown model: %v", result)
	}
	if result["total_tokens"] == 0 {
		t.Fatal("total_tokens = 0, want the breakdown to survive")
	}
}

func TestPromptAnalyzeRejectsInvalidBody(t *testing.T) {
	app, _ := newSchedulerApp(t)
	if _, err := app.promptAnalyze("", json.RawMessage(`[]`)); err == nil {
		t.Fatal("promptAnalyze accepted a JSON array, want an invalid-request error")
	}
}
