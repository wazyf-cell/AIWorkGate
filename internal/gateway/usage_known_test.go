package gateway

import (
	"io"
	"net/http"
	"testing"

	"aiworkgate/internal/model"
)

// D-18⑦: usage_known separates "the upstream reported usage" from "we never
// saw usage". The latter must keep prompt/completion 0 without pretending the
// upstream accounted for anything.

func TestBodyTapUsageKnown(t *testing.T) {
	cases := []struct {
		name  string
		sse   bool
		parts []string
		want  bool
	}{
		{"non-stream openai", false, []string{`{"id":"x","usage":{"prompt_tokens":3,"completion_tokens":4}}`}, true},
		{"non-stream split feed", false, []string{`{"id":"x","usa`, `ge":{"prompt_tokens":5,`, `"completion_tokens":6}}`}, true},
		{"non-stream anthropic", false, []string{`{"usage":{"input_tokens":7,"output_tokens":8}}`}, true},
		{"non-stream nested message", false, []string{`{"message":{"usage":{"input_tokens":9,"output_tokens":10}}}`}, true},
		{"non-stream zero counts still known", false, []string{`{"id":"x","usage":{"prompt_tokens":0,"completion_tokens":0}}`}, true},
		{"non-stream usage null", false, []string{`{"id":"x","usage":null}`}, false},
		{"non-stream no usage", false, []string{`{"id":"x","choices":[]}`}, false},
		{"non-stream unparsable", false, []string{`{"id":"x","usage":`}, false},
		{"sse usage in final chunk", true, []string{
			"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\n",
			"data: [DONE]\n\n"}, true},
		{"sse deltas only", true, []string{
			"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
			"data: [DONE]\n\n"}, false},
		{"sse usage null", true, []string{"data: {\"usage\":null}\n\n"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tap := newBodyTap(1024, tc.sse)
			for _, p := range tc.parts {
				tap.feed([]byte(p))
			}
			tap.finish()
			if tap.usageKnown != tc.want {
				t.Fatalf("usageKnown = %v, want %v", tap.usageKnown, tc.want)
			}
		})
	}
}

func decisionUsageGateway(t *testing.T, respondBody string, logs *fakeLogSink) *Gateway {
	t.Helper()
	srv, _ := newUpstreamServer(t, jsonResponder(http.StatusOK, respondBody))
	return newTestGateway(t, Options{
		DecisionUpstreams: func() []DecisionUpstream {
			return []DecisionUpstream{{
				ID: "jev", BaseURL: srv.URL + "/v1", Protocol: "systemone",
				ModelID: "NeoHorse-Jev-4B", APIKey: "sk-jev",
			}}
		},
		Log: logs,
	})
}

func TestDecisionUsageKnownRecorded(t *testing.T) {
	logs := &fakeLogSink{}
	g := decisionUsageGateway(t,
		`{"answers":[{"type":"noul","value":0.73}],"usage":{"input_tokens":12,"output_tokens":3}}`, logs)

	rr := serve(g, decisionRequest(t, "/v1/systemone",
		`{"state":"pick one","questions":[{"type":"noul"}]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rr.Code, rr.Body.String())
	}
	rows := logs.wait(t, 1)
	if !rows[0].UsageKnown {
		t.Fatalf("usage_known = false, want true (upstream reported usage): %+v", rows[0])
	}
	if rows[0].PromptTokens != 12 || rows[0].CompletionTokens != 3 {
		t.Fatalf("usage = %d/%d, want 12/3", rows[0].PromptTokens, rows[0].CompletionTokens)
	}
}

func TestDecisionNoUsageStaysUnknown(t *testing.T) {
	logs := &fakeLogSink{}
	g := decisionUsageGateway(t, `{"answers":[]}`, logs)

	rr := serve(g, decisionRequest(t, "/v1/systemone",
		`{"state":"pick one","questions":[{"type":"noul"}]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rr.Code, rr.Body.String())
	}
	rows := logs.wait(t, 1)
	if rows[0].UsageKnown {
		t.Fatalf("usage_known = true without upstream usage: %+v", rows[0])
	}
	if rows[0].PromptTokens != 0 || rows[0].CompletionTokens != 0 {
		t.Fatalf("tokens = %d/%d without usage, want 0/0 (not fabricated)",
			rows[0].PromptTokens, rows[0].CompletionTokens)
	}
}

func chatUsageGateway(t *testing.T, respond func(http.ResponseWriter, *http.Request, []byte), logs *fakeLogSink) *Gateway {
	t.Helper()
	srv, _ := newUpstreamServer(t, respond)
	reg := newFakeRegistry().add(
		pluginStatus("p", true, model.ModelInfo{ID: "chat"}), srv.URL, "tok")
	return newTestGateway(t, Options{Plugins: reg, Log: logs})
}

func TestChatNonStreamUsageKnown(t *testing.T) {
	logs := &fakeLogSink{}
	g := chatUsageGateway(t, jsonResponder(http.StatusOK,
		`{"id":"x","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":22}}`), logs)

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p/chat","messages":[]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	rows := logs.wait(t, 1)
	if !rows[0].UsageKnown || rows[0].PromptTokens != 11 || rows[0].CompletionTokens != 22 {
		t.Fatalf("row = %+v, want usage_known true with 11/22", rows[0])
	}
}

func TestChatNonStreamNoUsageUnknown(t *testing.T) {
	logs := &fakeLogSink{}
	g := chatUsageGateway(t, jsonResponder(http.StatusOK, `{"id":"x","choices":[]}`), logs)

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p/chat","messages":[]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	rows := logs.wait(t, 1)
	if rows[0].UsageKnown {
		t.Fatalf("usage_known = true without upstream usage: %+v", rows[0])
	}
	if rows[0].PromptTokens != 0 || rows[0].CompletionTokens != 0 {
		t.Fatalf("tokens = %d/%d without usage, want 0/0 (not fabricated)",
			rows[0].PromptTokens, rows[0].CompletionTokens)
	}
}

func TestChatStreamUsageKnown(t *testing.T) {
	logs := &fakeLogSink{}
	g := chatUsageGateway(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":7}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}, logs)

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p/chat","stream":true,"messages":[]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	rows := logs.wait(t, 1)
	if !rows[0].Stream {
		t.Fatalf("stream flag lost: %+v", rows[0])
	}
	if !rows[0].UsageKnown || rows[0].PromptTokens != 5 || rows[0].CompletionTokens != 7 {
		t.Fatalf("row = %+v, want aggregated usage_known true with 5/7", rows[0])
	}
}

func TestChatStreamNoUsageUnknown(t *testing.T) {
	logs := &fakeLogSink{}
	g := chatUsageGateway(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}, logs)

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p/chat","stream":true,"messages":[]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	rows := logs.wait(t, 1)
	if rows[0].UsageKnown {
		t.Fatalf("usage_known = true on a stream without usage: %+v", rows[0])
	}
	if rows[0].PromptTokens != 0 || rows[0].CompletionTokens != 0 {
		t.Fatalf("tokens = %d/%d without usage, want 0/0 (not fabricated)",
			rows[0].PromptTokens, rows[0].CompletionTokens)
	}
}
