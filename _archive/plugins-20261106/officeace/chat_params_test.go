package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

func chatToolDef() []map[string]any {
	return []map[string]any{{
		"type": "function",
		"function": map[string]any{
			"name":       "get_weather",
			"parameters": map[string]any{"type": "object"},
		},
	}}
}

// TestChatForwardsSupportedParams verifies the OpenAI request fields the MaaS
// chat API accepts are no longer dropped by the translation.
func TestChatForwardsSupportedParams(t *testing.T) {
	maas := newFakeMaaS(t, successSSE)
	root := makeInstall(t, defaultSession(maas.URL()))
	_, h, _ := newTestPlugin(t, func(o *Options) { o.ForcedOfficeAceHome = installHome(t, root) })

	body := sampleChatBody("glm-5.3", "hi", map[string]any{
		"tools":             chatToolDef(),
		"tool_choice":       "required",
		"stream_options":    map[string]any{"include_usage": true},
		"n":                 2,
		"presence_penalty":  0.3,
		"frequency_penalty": 0.4,
		"seed":              42,
	})
	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{body: body})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	reqs := maas.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("upstream requests = %d", len(reqs))
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(reqs[0].Body), &sent); err != nil {
		t.Fatalf("decode upstream body: %v", err)
	}
	if sent["tool_choice"] != "required" {
		t.Fatalf("tool_choice = %v", sent["tool_choice"])
	}
	so, _ := sent["stream_options"].(map[string]any)
	if so["include_usage"] != true {
		t.Fatalf("stream_options = %v", sent["stream_options"])
	}
	if sent["n"] != float64(2) {
		t.Fatalf("n = %v", sent["n"])
	}
	if sent["presence_penalty"] != 0.3 || sent["frequency_penalty"] != 0.4 {
		t.Fatalf("penalties = %v / %v", sent["presence_penalty"], sent["frequency_penalty"])
	}
	if sent["seed"] != float64(42) {
		t.Fatalf("seed = %v", sent["seed"])
	}
}

// TestChatDoesNotInventOptionalParams keeps absent fields absent: forwarding
// must be driven by the caller, never by defaults.
func TestChatDoesNotInventOptionalParams(t *testing.T) {
	maas := newFakeMaaS(t, successSSE)
	root := makeInstall(t, defaultSession(maas.URL()))
	_, h, _ := newTestPlugin(t, func(o *Options) { o.ForcedOfficeAceHome = installHome(t, root) })

	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{body: sampleChatBody("glm-5.3", "hi", nil)})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body.String())
	}
	reqs := maas.snapshot()
	var sent map[string]any
	if err := json.Unmarshal([]byte(reqs[0].Body), &sent); err != nil {
		t.Fatalf("decode upstream body: %v", err)
	}
	for _, key := range []string{"tool_choice", "stream_options", "n", "presence_penalty", "frequency_penalty", "seed"} {
		if v, ok := sent[key]; ok {
			t.Fatalf("%s must not be invented: %v", key, v)
		}
	}
}

// TestChatNonStreamKeepsMultipleChoices: forwarding n only makes sense if the
// aggregator keeps each choice separate instead of concatenating their text.
func TestChatNonStreamKeepsMultipleChoices(t *testing.T) {
	maas := newFakeMaaS(t, func(w http.ResponseWriter, r *http.Request, body string) {
		writeSSE(w,
			`{"id":"multi","model":"glm-5.3","choices":[{"index":0,"delta":{"content":"first"},"finish_reason":null}]}`,
			`{"id":"multi","model":"glm-5.3","choices":[{"index":1,"delta":{"content":"second"},"finish_reason":null}]}`,
			`{"id":"multi","model":"glm-5.3","choices":[{"index":0,"delta":{},"finish_reason":"stop"},{"index":1,"delta":{},"finish_reason":"stop"}]}`,
		)
	})
	root := makeInstall(t, defaultSession(maas.URL()))
	_, h, _ := newTestPlugin(t, func(o *Options) { o.ForcedOfficeAceHome = installHome(t, root) })

	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions",
		reqOpts{body: sampleChatBody("glm-5.3", "hi", map[string]any{"n": 2})})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body.String())
	}
	resp := decodeAs[chatResp](t, rec)
	if len(resp.Choices) != 2 {
		t.Fatalf("choices = %d, want 2: %s", len(resp.Choices), rec.Body.String())
	}
	if resp.Choices[0].Message.Content != "first" || resp.Choices[1].Message.Content != "second" {
		t.Fatalf("choices = %+v", resp.Choices)
	}
}
