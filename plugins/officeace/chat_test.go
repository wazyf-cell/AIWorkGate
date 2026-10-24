package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

type chatResp struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage map[string]float64 `json:"usage"`
}

func TestChatNonStreamAggregates(t *testing.T) {
	maas := newFakeMaaS(t, func(w http.ResponseWriter, r *http.Request, body string) {
		if r.URL.Path != "/v2/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("test-app-key-0001:test-app-secret-0001"))
		if r.Header.Get("Authorization") != wantAuth {
			t.Errorf("authorization = %q, want Basic", r.Header.Get("Authorization"))
		}
		var req map[string]any
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Errorf("upstream body: %v", err)
		}
		if req["stream"] != true {
			t.Errorf("stream = %v, want true", req["stream"])
		}
		if _, ok := req["thinking"]; ok {
			t.Errorf("thinking must not be invented: %v", req["thinking"])
		}
		successSSE(w, r, body)
	})
	s := defaultSession(maas.URL())
	root := makeInstall(t, s)
	_, h, _ := newTestPlugin(t, func(o *Options) { o.ForcedOfficeAceHome = installHome(t, root) })

	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions",
		reqOpts{body: sampleChatBody("glm-5.3", "hi", map[string]any{"temperature": 0.2, "top_p": 0.9, "max_tokens": 64})})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	resp := decodeAs[chatResp](t, rec)
	if resp.Object != "chat.completion" || resp.Model != "glm-5.3" || len(resp.Choices) != 1 {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.Choices[0].Message.Content != "hello world" || resp.Choices[0].FinishReason != "stop" {
		t.Fatalf("choice = %+v", resp.Choices[0])
	}
	if resp.Usage["prompt_tokens"] != 7 || resp.Usage["completion_tokens"] != 2 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	reqs := maas.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("upstream requests = %d", len(reqs))
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(reqs[0].Body), &sent)
	if sent["temperature"] != 0.2 || sent["top_p"] != 0.9 || sent["max_tokens"] != float64(64) {
		t.Fatalf("sampling params = %+v", sent)
	}
}

func TestChatStreamPassthrough(t *testing.T) {
	maas := newFakeMaaS(t, successSSE)
	s := defaultSession(maas.URL())
	root := makeInstall(t, s)
	_, h, _ := newTestPlugin(t, func(o *Options) { o.ForcedOfficeAceHome = installHome(t, root) })

	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions",
		reqOpts{body: sampleChatBody("glm-5.3", "hi", map[string]any{"stream": true})})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"hello"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("SSE body = %s", body)
	}
}

func TestChatToolsAndExplicitThinking(t *testing.T) {
	maas := newFakeMaaS(t, func(w http.ResponseWriter, r *http.Request, body string) {
		var req map[string]any
		_ = json.Unmarshal([]byte(body), &req)
		if _, ok := req["tools"]; !ok {
			t.Errorf("tools missing in upstream body: %s", body)
		}
		th, _ := req["thinking"].(map[string]any)
		if th["type"] != "disabled" {
			t.Errorf("thinking = %v", req["thinking"])
		}
		writeSSE(w,
			`{"id":"c1","model":"glm-5.3","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}}]},"finish_reason":null}]}`,
			`{"id":"c1","model":"glm-5.3","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]},"finish_reason":"tool_calls"}]}`,
		)
	})
	s := defaultSession(maas.URL())
	root := makeInstall(t, s)
	_, h, _ := newTestPlugin(t, func(o *Options) { o.ForcedOfficeAceHome = installHome(t, root) })

	tools := []map[string]any{{
		"type": "function",
		"function": map[string]any{
			"name":       "get_weather",
			"parameters": map[string]any{"type": "object"},
		},
	}}
	body := sampleChatBody("glm-5.3", "weather?", map[string]any{
		"tools":    tools,
		"thinking": map[string]any{"type": "disabled"},
	})
	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{body: body})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	resp := decodeAs[chatResp](t, rec)
	choice := resp.Choices[0]
	if choice.FinishReason != "tool_calls" || len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("choice = %+v", choice)
	}
	call := choice.Message.ToolCalls[0]
	if call.Function.Name != "get_weather" || call.Function.Arguments != `{"city":"Paris"}` {
		t.Fatalf("tool call = %+v", call)
	}
}

func TestChatImagesStrippedWithoutVision(t *testing.T) {
	maas := newFakeMaaS(t, func(w http.ResponseWriter, r *http.Request, body string) {
		if strings.Contains(body, "image_url") || strings.Contains(body, "https://example.com/a.png") {
			t.Errorf("image should be stripped: %s", body)
		}
		if !strings.Contains(body, "image omitted") {
			t.Errorf("missing placeholder: %s", body)
		}
		successSSE(w, r, body)
	})
	s := defaultSession(maas.URL())
	root := makeInstall(t, s)
	_, h, log := newTestPlugin(t, func(o *Options) { o.ForcedOfficeAceHome = installHome(t, root) })

	body := `{"model":"glm-5.3","messages":[{"role":"user","content":[{"type":"text","text":"what is this"},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`
	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{body: body})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(log.String(), "images stripped") {
		t.Fatalf("missing warning log: %s", log.String())
	}
}

func TestChatImagesPassWhenVisionKnown(t *testing.T) {
	maas := newFakeMaaS(t, func(w http.ResponseWriter, r *http.Request, body string) {
		if !strings.Contains(body, "image_url") {
			t.Errorf("image should be preserved: %s", body)
		}
		successSSE(w, r, body)
	})
	s := defaultSession(maas.URL())
	root := makeInstall(t, s)
	p, h, _ := newTestPlugin(t, func(o *Options) { o.ForcedOfficeAceHome = installHome(t, root) })
	p.modelsMu.Lock()
	p.models = []Model{{ID: "glm-5.3", Vision: boolPtr(true)}}
	p.modelsSource = "test"
	p.modelsFetchedAt = fixedNow
	p.modelsMu.Unlock()

	body := `{"model":"glm-5.3","messages":[{"role":"user","content":[{"type":"text","text":"see"},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`
	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{body: body})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
}

func TestBasicFallsBackToHMACOn401(t *testing.T) {
	maas := newFakeMaaS(t, func(w http.ResponseWriter, r *http.Request, body string) {
		if strings.HasPrefix(r.Header.Get("Authorization"), "Basic ") {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid basic"}`))
			return
		}
		successSSE(w, r, body)
	})
	s := defaultSession(maas.URL())
	root := makeInstall(t, s)
	_, h, log := newTestPlugin(t, func(o *Options) { o.ForcedOfficeAceHome = installHome(t, root) })

	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{body: sampleChatBody("glm-5.3", "hi", nil)})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	reqs := maas.snapshot()
	if len(reqs) != 2 {
		t.Fatalf("upstream requests = %d", len(reqs))
	}
	if !strings.HasPrefix(reqs[1].Authorization, "SDK-HMAC-SHA256 ") {
		t.Fatalf("second auth = %q", reqs[1].Authorization)
	}
	if reqs[1].SecurityToken != s.STSToken || reqs[1].ProjectID == "" {
		t.Fatalf("hmac headers = %+v", reqs[1])
	}
	if !strings.Contains(log.String(), `"auth":"hmac"`) {
		t.Fatalf("log missing hmac auth mode: %s", log.String())
	}
}

func TestRefreshOnExpiredThenHMACWithNewToken(t *testing.T) {
	maas := newFakeMaaS(t, func(w http.ResponseWriter, r *http.Request, body string) {
		if r.Header.Get("X-Security-Token") != "sts-renewed" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"expired"}`))
			return
		}
		successSSE(w, r, body)
	})
	s := defaultSession(maas.URL())
	s.STSExpiresAt = fixedNow.Add(-time.Hour).Format(time.RFC3339)
	s.SessionExpiresAt = s.STSExpiresAt
	root := makeInstall(t, s)
	iam := newFakeIAM(t, "sts-renewed")
	p, h, _ := newTestPlugin(t, func(o *Options) {
		o.ForcedOfficeAceHome = installHome(t, root)
		o.IAMURL = iam.URL()
	})
	p.cfgMu.Lock()
	p.cfg.AutoRefresh = true
	p.cfgMu.Unlock()

	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{body: sampleChatBody("glm-5.3", "hi", nil)})
	if rec.Code != http.StatusOK {
		for _, rq := range maas.snapshot() {
			t.Logf("upstream: path=%s auth=%q token=%q", rq.Path, rq.Authorization, rq.SecurityToken)
		}
		t.Logf("iam calls=%d", iam.callCount())
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	if iam.callCount() < 1 {
		t.Fatal("IAM was not called")
	}
	reqs := maas.snapshot()
	last := reqs[len(reqs)-1]
	if last.SecurityToken != "sts-renewed" || !strings.HasPrefix(last.Authorization, "SDK-HMAC-SHA256 ") {
		t.Fatalf("last upstream request = %+v", last)
	}
}

func TestChatWithoutAccount(t *testing.T) {
	_, h, _ := newTestPlugin(t, nil)
	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{body: sampleChatBody("glm-5.3", "hi", nil)})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	if env := decodeAs[errorEnvelope](t, rec); env.Error.Type != "account_unavailable" {
		t.Fatalf("error = %+v", env.Error)
	}
}

func TestChatUpstreamErrorAndRateLimit(t *testing.T) {
	maas := newFakeMaaS(t, func(w http.ResponseWriter, r *http.Request, body string) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"slow down"}`))
	})
	s := defaultSession(maas.URL())
	root := makeInstall(t, s)
	p, h, _ := newTestPlugin(t, func(o *Options) { o.ForcedOfficeAceHome = installHome(t, root) })
	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{body: sampleChatBody("glm-5.3", "hi", nil)})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	if env := decodeAs[errorEnvelope](t, rec); env.Error.Type != "upstream_rate_limited" {
		t.Fatalf("error = %+v", env.Error)
	}
	if a, _ := p.findAccount(accountIDFor(s.userKey())); a.Status != statusRateLimited {
		t.Fatalf("account status = %q", a.Status)
	}
}

func TestNoAutoRefreshByDefault(t *testing.T) {
	maas := newFakeMaaS(t, func(w http.ResponseWriter, r *http.Request, body string) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"expired"}`))
	})
	s := defaultSession(maas.URL())
	s.STSExpiresAt = fixedNow.Add(-time.Hour).Format(time.RFC3339)
	s.SessionExpiresAt = s.STSExpiresAt
	root := makeInstall(t, s)
	iam := newFakeIAM(t, "sts-renewed")
	_, h, _ := newTestPlugin(t, func(o *Options) {
		o.ForcedOfficeAceHome = installHome(t, root)
		o.IAMURL = iam.URL()
	})

	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{body: sampleChatBody("glm-5.3", "hi", nil)})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	if iam.callCount() != 0 {
		t.Fatalf("IAM calls = %d, want 0 with auto_refresh off", iam.callCount())
	}
}
