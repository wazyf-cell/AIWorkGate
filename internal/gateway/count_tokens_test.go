package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// D-51: POST /v1/messages/count_tokens answers the local estimate. The default
// caliber (本地估算) is fixed in count_tokens.go; these tests lock the wire
// shape, the monotonicity and the authentication requirement.

func TestCountTokensLocalEstimate(t *testing.T) {
	g := newTestGateway(t, Options{Plugins: newFakeRegistry()})

	body := `{"model":"claude-x","messages":[{"role":"user","content":"hello world"}]}`
	rr := serve(g, newChatRequest(t, "/v1/messages/count_tokens", clientKey, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	var payload struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body: %v (%s)", err, rr.Body.String())
	}
	if payload.InputTokens <= 0 {
		t.Fatalf("input_tokens = %d, want a positive estimate", payload.InputTokens)
	}
	if got := rr.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("content-type = %q", got)
	}
	// Extra keys would break clients that strict-decode the object.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	if len(raw) != 1 {
		t.Fatalf("response keys = %d, want exactly input_tokens (%s)", len(raw), rr.Body.String())
	}

	// A system block and tools are counted on the Anthropic face too.
	withSystem := `{"model":"claude-x","system":"You are terse.","messages":[` +
		`{"role":"user","content":"hi"}],"tools":[{"name":"f","description":"a tool",` +
		`"input_schema":{"type":"object"}}]}`
	rr2 := serve(g, newChatRequest(t, "/v1/messages/count_tokens", clientKey, withSystem))
	var bigger struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(rr2.Body.Bytes(), &bigger); err != nil {
		t.Fatalf("decode body: %v (%s)", err, rr2.Body.String())
	}
	if bigger.InputTokens <= payload.InputTokens {
		t.Fatalf("system+tools ignored: %d <= %d", bigger.InputTokens, payload.InputTokens)
	}
}

func TestCountTokensGrowsWithMessages(t *testing.T) {
	g := newTestGateway(t, Options{Plugins: newFakeRegistry()})

	count := func(body string) int {
		t.Helper()
		rr := serve(g, newChatRequest(t, "/v1/messages/count_tokens", clientKey, body))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d (body %s)", rr.Code, rr.Body.String())
		}
		var payload struct {
			InputTokens int `json:"input_tokens"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode body: %v (%s)", err, rr.Body.String())
		}
		return payload.InputTokens
	}

	small := count(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	large := count(`{"model":"m","messages":[{"role":"user","content":"` +
		strings.Repeat("hello world ", 400) + `"}]}`)
	if large <= small {
		t.Fatalf("input_tokens did not grow: small=%d large=%d", small, large)
	}
}

func TestCountTokensRequiresKey(t *testing.T) {
	g := newTestGateway(t, Options{Plugins: newFakeRegistry()})

	rr := serve(g, newChatRequest(t, "/v1/messages/count_tokens", "",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body %s)", rr.Code, rr.Body.String())
	}
	errPayload := decodeError(t, rr)
	if errPayload.Error.Type != "authentication_error" {
		t.Fatalf("error type = %q, want authentication_error", errPayload.Error.Type)
	}

	// A wrong key is refused the same way.
	rr2 := serve(g, newChatRequest(t, "/v1/messages/count_tokens", "agk_wrong",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if rr2.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key status = %d, want 401", rr2.Code)
	}
}

// TestCountTokensNotBillableNotForwarded locks the two routing decisions of
// D-51: the endpoint is not in isInferencePath (no key quota burn) and it never
// reaches a custom upstream even when the model name matches a custom model.
func TestCountTokensNotBillableNotForwarded(t *testing.T) {
	srv, rec := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"ok":true}`))
	cm := CustomModel{
		ID: "cm-1", Name: "claude-local", Protocol: "anthropic", BaseURL: srv.URL,
		UpstreamModel: "claude-x", Enabled: true,
	}
	g := newTestGateway(t, customFixture(t, []CustomModel{cm}, customUpstreamKey, Options{Plugins: newFakeRegistry()}))

	rr := serve(g, newChatRequest(t, "/v1/messages/count_tokens", clientKey,
		`{"model":"claude-local","messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rr.Code, rr.Body.String())
	}
	if rec.count() != 0 {
		t.Fatalf("upstream received %d calls, want 0 (count_tokens must not forward)", rec.count())
	}
	var payload struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil || payload.InputTokens <= 0 {
		t.Fatalf("body = %s (err %v), want positive input_tokens", rr.Body.String(), err)
	}
}
