package app

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aiworkgate/internal/gateway"
	"aiworkgate/internal/store"
)

// TestCustomConnectionE2E verifies end-to-end routing through all three
// custom connection protocols using fake upstreams.
func TestCustomConnectionE2E(t *testing.T) {
	// Create test store
	st := openTestStoreForApp(t)

	// Start fake upstream server serving all three protocols
	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == "/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"id":      "chatcmpl-fake-openai",
				"object":  "chat.completion",
				"created": time.Now().Unix(),
				"model":   "vendor-chat",
				"choices": []map[string]any{{
					"index": 0,
					"message": map[string]any{
						"role":    "assistant",
						"content": "OPENAI-OK",
					},
					"finish_reason": "stop",
				}},
				"usage": map[string]any{
					"prompt_tokens":     10,
					"completion_tokens": 5,
					"total_tokens":      15,
				},
			})
		case path == "/v1/messages":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"id":    "msg-fake-anthropic",
				"type":  "message",
				"role":  "assistant",
				"model": "claude-local",
				"content": []map[string]any{{
					"type": "text",
					"text": "ANTHROPIC-OK",
				}},
				"stop_reason": "end_turn",
				"usage": map[string]any{
					"input_tokens":  10,
					"output_tokens": 5,
				},
			})
		case path == "/v1beta/models/gemini-test:generateContent":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"candidates": []map[string]any{{
					"content": map[string]any{
						"parts": []map[string]any{{
							"text": "GEMINI-OK",
						}},
					},
					"finishReason": "STOP",
				}},
				"usageMetadata": map[string]any{
					"promptTokenCount":     10,
					"candidatesTokenCount": 5,
					"totalTokenCount":      15,
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer fakeUpstream.Close()

	// Create three test connections
	connections := []struct {
		label    string
		protocol string
		model    string
	}{
		{"fake-openai", "openai", "vendor-chat"},
		{"fake-anthropic", "anthropic", "claude-local"},
		{"fake-gemini", "gemini", "gemini-test"},
	}

	for _, c := range connections {
		conn, err := st.UpsertCustomConnection(store.CustomConnection{
			Label:    c.label,
			BaseURL:  fakeUpstream.URL,
			Protocol: c.protocol,
			Enabled:  true,
		})
		if err != nil {
			t.Fatalf("create connection %s: %v", c.label, err)
		}

		// Add a model to each connection
		if _, err := st.UpsertCustomModel(store.CustomModel{
			ConnectionID:     conn.ID,
			Name:             c.model,
			BaseURL:          fakeUpstream.URL,
			UpstreamModel:    c.model,
			Enabled:          true,
			ContextWindow:    64000,
			MaxOutputTokens:  2048,
			InputTypes:       []string{"text", "image"},
			ReasoningTiers:   []string{"low", "high"},
			DefaultReasoning: "low",
		}); err != nil {
			t.Fatalf("create model %s: %v", c.model, err)
		}
	}

	// Build gateway options with a permissive key verifier for testing
	opts := gateway.Options{
		Keys: &testKeyVerifier{},
		CustomModels: func() ([]gateway.CustomModel, error) {
			models, err := st.ListCustomModels()
			if err != nil {
				return nil, err
			}
			result := make([]gateway.CustomModel, len(models))
			for i, m := range models {
				result[i] = gateway.CustomModel{
					ID:                m.ID,
					ConnectionID:      m.ConnectionID,
					Name:              m.Name,
					BaseURL:           m.BaseURL,
					Protocol:          m.Protocol,
					ConnectionEnabled: true,
					UpstreamModel:     m.UpstreamModel,
					Enabled:           m.Enabled,
					ContextWindow:     m.ContextWindow,
					MaxOutputTokens:   m.MaxOutputTokens,
					InputTypes:        m.InputTypes,
					ReasoningTiers:    m.ReasoningTiers,
					DefaultReasoning:  m.DefaultReasoning,
				}
			}
			return result, nil
		},
		CustomModelKey: func(id string) (string, bool) {
			return "test-key", true
		},
	}

	gw := gateway.New(opts)

	// Test OpenAI protocol routing
	t.Run("OpenAI routing", func(t *testing.T) {
		body := `{"model":"vendor-chat","messages":[{"role":"user","content":"test"}]}`
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer test-client-key")
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		gw.ServeHTTP(w, req)

		resp := w.Result()
		respBody, _ := io.ReadAll(resp.Body)

		if resp.StatusCode != http.StatusOK {
			t.Errorf("OpenAI routing status = %d, want 200 (body: %s)", resp.StatusCode, string(respBody))
		}

		t.Logf("OpenAI response body: %s", string(respBody))

		var result map[string]any
		if err := json.Unmarshal(respBody, &result); err != nil {
			t.Fatalf("OpenAI response parse error: %v (body: %s)", err, string(respBody))
		}

		choicesRaw, ok := result["choices"]
		if !ok {
			t.Fatalf("OpenAI routing has no choices field")
		}
		
		choices, ok := choicesRaw.([]interface{})
		if !ok || len(choices) == 0 {
			t.Fatalf("OpenAI routing returned no choices (type: %T)", choicesRaw)
		}

		message, ok := choices[0].(map[string]any)["message"].(map[string]any)
		if !ok {
			t.Fatalf("OpenAI choice has no message")
		}

		content, _ := message["content"].(string)
		if content != "OPENAI-OK" {
			t.Errorf("OpenAI content = %q, want %q", content, "OPENAI-OK")
		}
	})

	// Test Anthropic protocol routing
	t.Run("Anthropic routing", func(t *testing.T) {
		body := `{"model":"claude-local","messages":[{"role":"user","content":"test"}]}`
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer test-client-key")
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		gw.ServeHTTP(w, req)

		resp := w.Result()
		respBody, _ := io.ReadAll(resp.Body)

		if resp.StatusCode != http.StatusOK {
			t.Errorf("Anthropic routing status = %d, want 200 (body: %s)", resp.StatusCode, string(respBody))
		}

		var result map[string]any
		if err := json.Unmarshal(respBody, &result); err != nil {
			t.Fatalf("Anthropic response parse error: %v", err)
		}

		choicesRaw, ok := result["choices"]
		if !ok {
			t.Fatalf("Anthropic routing has no choices field")
		}
		
		choices, ok := choicesRaw.([]interface{})
		if !ok || len(choices) == 0 {
			t.Fatalf("Anthropic routing returned no choices (type: %T)", choicesRaw)
		}

		message, ok := choices[0].(map[string]any)["message"].(map[string]any)
		if !ok {
			t.Fatalf("Anthropic choice has no message")
		}

		content, _ := message["content"].(string)
		if content != "ANTHROPIC-OK" {
			t.Errorf("Anthropic content = %q, want %q", content, "ANTHROPIC-OK")
		}
	})

	// Test Gemini protocol routing
	t.Run("Gemini routing", func(t *testing.T) {
		body := `{"model":"gemini-test","messages":[{"role":"user","content":"test"}]}`
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer test-client-key")
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		gw.ServeHTTP(w, req)

		resp := w.Result()
		respBody, _ := io.ReadAll(resp.Body)

		if resp.StatusCode != http.StatusOK {
			t.Errorf("Gemini routing status = %d, want 200 (body: %s)", resp.StatusCode, string(respBody))
		}

		var result map[string]any
		if err := json.Unmarshal(respBody, &result); err != nil {
			t.Fatalf("Gemini response parse error: %v", err)
		}

		choicesRaw, ok := result["choices"]
		if !ok {
			t.Fatalf("Gemini routing has no choices field")
		}
		
		choices, ok := choicesRaw.([]interface{})
		if !ok || len(choices) == 0 {
			t.Fatalf("Gemini routing returned no choices (type: %T)", choicesRaw)
		}

		message, ok := choices[0].(map[string]any)["message"].(map[string]any)
		if !ok {
			t.Fatalf("Gemini choice has no message")
		}

		content, _ := message["content"].(string)
		if content != "GEMINI-OK" {
			t.Errorf("Gemini content = %q, want %q", content, "GEMINI-OK")
		}
	})
}

func openTestStoreForApp(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

type testKeyVerifier struct{}

func (v *testKeyVerifier) Verify(plain string) (id, name string, ok bool) {
	return "test-key-id", "test-key", true
}

func (v *testKeyVerifier) Touch(id string) {}
