package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func decisionGateway(t *testing.T, srvURL, protocol, modelID string) *Gateway {
	t.Helper()
	return newTestGateway(t, Options{
		DecisionUpstreams: func() []DecisionUpstream {
			return []DecisionUpstream{{
				ID: "jev", BaseURL: srvURL + "/v1", Protocol: protocol,
				ModelID: modelID, APIKey: "sk-jev",
			}}
		},
	})
}

func decisionRequest(t *testing.T, path, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+clientKey)
	return req
}

func TestDecisionSystemonePassthrough(t *testing.T) {
	srv, rec := newUpstreamServer(t, jsonResponder(http.StatusOK,
		`{"answers":[{"type":"noul","value":0.73}],"usage":{"input_tokens":12,"output_tokens":3},"traceId":"t-1"}`))
	g := decisionGateway(t, srv.URL, "systemone", "NeoHorse-Jev-4B")

	rr := serve(g, decisionRequest(t, "/v1/systemone",
		`{"state":"pick one","questions":[{"type":"noul"}]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"traceId":"t-1"`) {
		t.Fatalf("response not passed through: %s", rr.Body.String())
	}
	up := rec.last(t)
	if up.Path != "/v1/systemone" {
		t.Fatalf("upstream path = %q, want /v1/systemone", up.Path)
	}
	if up.Authorization != "Bearer sk-jev" {
		t.Fatalf("upstream auth = %q", up.Authorization)
	}
	var body map[string]any
	if err := json.Unmarshal(up.Body, &body); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	if body["model"] != "NeoHorse-Jev-4B" || body["state"] != "pick one" {
		t.Fatalf("upstream body = %v", body)
	}
}

func TestDecisionAliasAndProtocolSelection(t *testing.T) {
	srv, rec := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"answers":[]}`))
	g := newTestGateway(t, Options{
		DecisionUpstreams: func() []DecisionUpstream {
			return []DecisionUpstream{
				{ID: "a", BaseURL: srv.URL + "/v1", Protocol: "systemone", ModelID: "m-systemone"},
				{ID: "b", BaseURL: srv.URL + "/v1", Protocol: "decision", ModelID: "m-decision"},
			}
		},
	})
	serve(g, decisionRequest(t, "/v1/decision", `{"state":"x","questions":[]}`))
	if got := rec.last(t).Path; got != "/v1/decision" {
		t.Fatalf("alias path = %q, want /v1/decision", got)
	}
	serve(g, decisionRequest(t, "/v1/systemone", `{"state":"x","questions":[]}`))
	if got := rec.last(t).Path; got != "/v1/systemone" {
		t.Fatalf("systemone path = %q", got)
	}
}

func TestDecisionModelRejectedOnChat(t *testing.T) {
	srv, rec := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"answers":[]}`))
	g := decisionGateway(t, srv.URL, "systemone", "NeoHorse-Jev-4B")

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"NeoHorse-Jev-4B","messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "structured_model") {
		t.Fatalf("chat with decision model = %d (%s), want 400 structured_model", rr.Code, rr.Body.String())
	}
	if rec.count() != 0 {
		t.Fatalf("decision upstream must not be called from chat")
	}
}

func TestDecisionModelsListedStructured(t *testing.T) {
	srv, _ := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"answers":[]}`))
	g := decisionGateway(t, srv.URL, "systemone", "NeoHorse-Jev-4B")

	rr := serve(g, newGetRequest(t, "/v1/models", clientKey))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var payload modelsList
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, entry := range payload.Data {
		if entry.ID == "NeoHorse-Jev-4B" {
			if !entry.Structured {
				t.Fatalf("decision model lacks structured:true: %+v", entry)
			}
			return
		}
	}
	t.Fatalf("decision model missing from %v", modelIDs(payload))
}

func TestDecisionPayloadLimit(t *testing.T) {
	srv, _ := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"answers":[]}`))
	g := decisionGateway(t, srv.URL, "systemone", "m")

	big := strings.Repeat("x", (1<<20)+16)
	rr := serve(g, decisionRequest(t, "/v1/systemone", `{"state":"`+big+`","questions":[]}`))
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize = %d (%s), want 413", rr.Code, rr.Body.String())
	}
}

func TestDecisionErrorPassthroughNoRetry(t *testing.T) {
	calls := 0
	srv, _ := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"code":"rate_limited","message":"slow down","traceId":"t-9"}`))
	})
	g := decisionGateway(t, srv.URL, "systemone", "m")

	rr := serve(g, decisionRequest(t, "/v1/systemone", `{"state":"x","questions":[]}`))
	if rr.Code != http.StatusTooManyRequests || !strings.Contains(rr.Body.String(), `"traceId":"t-9"`) {
		t.Fatalf("error passthrough = %d (%s)", rr.Code, rr.Body.String())
	}
	if calls != 1 {
		t.Fatalf("decision upstream called %d times, want exactly 1 (no retry)", calls)
	}
}

// D-18⑧/F-12 本地预判回归：带图只收单题、纯文本最多 16 题，超限在本地拒而
// 不发上游；合法形状照常转发。
func TestDecisionLocalQuestionLimits(t *testing.T) {
	srv, rec := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"answers":[]}`))
	g := decisionGateway(t, srv.URL, "systemone", "m")

	cases := []struct {
		name string
		body string
		want int
	}{
		{"image single question ok", `{"image":"iVBOR","questions":[{"type":"choice","image":"x"}]}`, http.StatusOK},
		{"top-level image two questions rejected", `{"image":"iVBOR","questions":[{"type":"choice"},{"type":"noul"}]}`, http.StatusBadRequest},
		{"per-question image two questions rejected", `{"questions":[{"type":"choice","image":"x"},{"type":"noul"}]}`, http.StatusBadRequest},
		{"image empty string is plain text", `{"image":"","questions":[{"type":"noul"},{"type":"noul"}]}`, http.StatusOK},
		{"image null is plain text", `{"image":null,"questions":[{"type":"noul"},{"type":"noul"}]}`, http.StatusOK},
		{"sixteen text questions ok", multiQuestions(16), http.StatusOK},
		{"seventeen text questions rejected", multiQuestions(17), http.StatusBadRequest},
		// 实测 JEV 对象形状（键=题id，task-70）：题数上限与带图单题保护同样生效。
		{"object image single question ok", `{"image":"iVBOR","questions":{"q1":{"type":"choice","image":"x"}}}`, http.StatusOK},
		{"object image two questions rejected", `{"image":"iVBOR","questions":{"q1":{"type":"choice"},"q2":{"type":"noul"}}}`, http.StatusBadRequest},
		{"object sixteen text questions ok", multiQuestionsObject(16), http.StatusOK},
		{"object seventeen text questions rejected", multiQuestionsObject(17), http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := serve(g, decisionRequest(t, "/v1/systemone", tc.body))
			if rr.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rr.Code, tc.want, rr.Body.String())
			}
		})
	}
	if got := rec.count(); got != 6 {
		t.Fatalf("upstream calls = %d, want 6 (rejected shapes never forwarded)", got)
	}
}

// multiQuestionsObject builds the object-shaped questions spelling (实测 JEV:
// 键=题id）with n questions.
func multiQuestionsObject(n int) string {
	qs := make([]string, n)
	for i := range qs {
		qs[i] = `"q` + strconv.Itoa(i+1) + `":{"type":"noul"}`
	}
	return `{"questions":{` + strings.Join(qs, ",") + `}}`
}

func multiQuestions(n int) string {
	qs := make([]string, n)
	for i := range qs {
		qs[i] = `{"type":"noul"}`
	}
	return `{"questions":[` + strings.Join(qs, ",") + `]}`
}
