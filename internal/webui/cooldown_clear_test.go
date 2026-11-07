package webui

// D-45 手动解除冷却端点的传输层测试：POST /api/accounts/{plugin}/{account}/
// cooldown/clear 的 405/404 语义与回调接线。

import (
	"errors"
	"net/http"
	"testing"
)

func TestAccountCooldownClearRoute(t *testing.T) {
	type clearCall struct{ pluginID, accountID string }
	var calls []clearCall
	handler := New(Options{
		Plugins: pluginsWithRunning(true),
		AccountCooldownClear: func(pluginID, accountID string) error {
			calls = append(calls, clearCall{pluginID, accountID})
			return nil
		},
	})

	recorder := doRequest(t, handler, http.MethodPost, "/api/accounts/mock/acc_1/cooldown/clear", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("clear = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		OK bool `json:"ok"`
	}
	decodeJSONBody(t, recorder, &payload)
	if !payload.OK {
		t.Fatalf("clear payload = %+v", payload)
	}
	if len(calls) != 1 || calls[0] != (clearCall{"mock", "acc_1"}) {
		t.Fatalf("calls = %+v", calls)
	}

	// 405: the clear is a POST-only action.
	recorder = doRequest(t, handler, http.MethodGet, "/api/accounts/mock/acc_1/cooldown/clear", "")
	if recorder.Code != http.StatusMethodNotAllowed || errorType(t, recorder) != "method_not_allowed" {
		t.Fatalf("GET clear = %d (%s), want 405 method_not_allowed", recorder.Code, errorType(t, recorder))
	}
	if allow := recorder.Header().Get("Allow"); allow != http.MethodPost {
		t.Fatalf("Allow header = %q, want POST", allow)
	}

	// 404: an unknown plugin is not found (the account id is host-side state
	// and is deliberately not validated).
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/ghost/acc_1/cooldown/clear", "")
	if recorder.Code != http.StatusNotFound || errorType(t, recorder) != "not_found" {
		t.Fatalf("unknown plugin = %d (%s), want 404 not_found", recorder.Code, errorType(t, recorder))
	}
	if len(calls) != 1 {
		t.Fatalf("rejected calls reached the host: %+v", calls)
	}

	// 404: a route missing a path segment never reaches the handler.
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/cooldown/clear", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("missing segment = %d, want 404", recorder.Code)
	}

	// A host callback failure maps through the account error mapping.
	failHandler := New(Options{
		Plugins: pluginsWithRunning(true),
		AccountCooldownClear: func(pluginID, accountID string) error {
			return errors.New("boom")
		},
	})
	recorder = doRequest(t, failHandler, http.MethodPost, "/api/accounts/mock/acc_1/cooldown/clear", "")
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("callback failure = %d, want 502 plugin_error mapping", recorder.Code)
	}
}

// TestAccountCooldownClearUnavailable locks the no-host answer: without the
// callback the endpoint answers 501 instead of pretending success.
func TestAccountCooldownClearUnavailable(t *testing.T) {
	handler := New(Options{Plugins: pluginsWithRunning(true)})
	recorder := doRequest(t, handler, http.MethodPost, "/api/accounts/mock/acc_1/cooldown/clear", "")
	if recorder.Code != http.StatusNotImplemented || errorType(t, recorder) != "not_implemented" {
		t.Fatalf("no host = %d (%s), want 501 not_implemented", recorder.Code, errorType(t, recorder))
	}
}
