package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The HTTP surface: token enforcement, the health probe, the empty-vs-configured
// account list, and the "not configured" path that must fail honestly rather
// than pretend to call upstream.

func newTestServer(t *testing.T) *server {
	t.Helper()
	st, err := newStore(t.TempDir())
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	up := newUpstream("")
	eng := &engine{cfg: defaultEngineConfig(), st: st, up: up}
	return &server{
		cfg:    pluginConfig{ID: "autoglm", APILevel: 2, Token: "test-token", DataDir: t.TempDir()},
		st:     st,
		up:     up,
		engine: eng,
		models: &modelCache{},
	}
}

func do(t *testing.T, s *server, method, path, body string, withToken bool) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if withToken {
		r.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, r)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

func TestHealthIsOKWithoutToken(t *testing.T) {
	s := newTestServer(t)
	rec := do(t, s, http.MethodGet, "/health", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	got := decode(t, rec)
	// The supervisor treats ok:false or status!=ok as unhealthy.
	if got["ok"] != true || got["status"] != "ok" {
		t.Fatalf("health = %v", got)
	}
	if got["configured"] != false {
		t.Fatalf("configured = %v, want false", got["configured"])
	}
	if got["sealing"] != sealKind() {
		t.Fatalf("sealing = %v, want %q", got["sealing"], sealKind())
	}
}

func TestProtectedEndpointsRejectMissingToken(t *testing.T) {
	s := newTestServer(t)
	for _, path := range []string{"/meta", "/accounts", "/v1/models", "/v1/chat/completions", "/checkin", "/refresh"} {
		method := http.MethodGet
		if path == "/v1/chat/completions" || path == "/checkin" || path == "/refresh" {
			method = http.MethodPost
		}
		rec := do(t, s, method, path, "{}", false)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without token: status = %d, want 401", path, rec.Code)
		}
	}
	// A wrong token must be rejected too, not just a missing one.
	r := httptest.NewRequest(http.MethodGet, "/meta", nil)
	r.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: status = %d, want 401", rec.Code)
	}
}

func TestAccountsIsEmptyBeforeLogin(t *testing.T) {
	s := newTestServer(t)
	rec := do(t, s, http.MethodGet, "/accounts", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Accounts) != 0 {
		t.Fatalf("accounts = %v, want an empty list", out.Accounts)
	}
}

func TestAccountsReportsStoredAccountWithoutLeakingToken(t *testing.T) {
	s := newTestServer(t)
	a := newAccount(accountData{
		AccessToken: "Bearer super-secret-token-value",
		UserID:      "uid-42",
		UserName:    "alice",
		Region:      regionCN,
	})
	if err := s.st.upsert(a); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	rec := do(t, s, http.MethodGet, "/accounts", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "super-secret-token-value") {
		t.Fatal("the account listing leaks the access token")
	}
	var out struct {
		Accounts []struct {
			ID         string `json:"id"`
			Label      string `json:"label"`
			Enabled    bool   `json:"enabled"`
			Status     string `json:"status"`
			Credential struct {
				Present bool `json:"present"`
			} `json:"credential"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(out.Accounts))
	}
	got := out.Accounts[0]
	if got.ID != "cn/uid-42" {
		t.Fatalf("id = %q, want cn/uid-42", got.ID)
	}
	if got.Label != "alice" || !got.Enabled || got.Status != "ok" || !got.Credential.Present {
		t.Fatalf("account = %+v", got)
	}
}

func TestMetaReportsIdentityAndCapabilities(t *testing.T) {
	s := newTestServer(t)
	rec := do(t, s, http.MethodGet, "/meta", "", true)
	got := decode(t, rec)
	if got["id"] != "autoglm" {
		t.Fatalf("id = %v", got["id"])
	}
	if got["api_level"] != float64(2) {
		t.Fatalf("api_level = %v", got["api_level"])
	}
	caps, _ := got["capabilities"].([]any)
	want := map[string]bool{"upstream": true, "accounts": true}
	for _, c := range caps {
		delete(want, c.(string))
	}
	if len(want) != 0 {
		t.Fatalf("capabilities are missing %v (got %v)", want, caps)
	}
}

func TestChatFailsHonestlyWhenUnconfigured(t *testing.T) {
	s := newTestServer(t)
	body := `{"model":"zai_auto","messages":[{"role":"user","content":"hi"}]}`
	rec := do(t, s, http.MethodPost, "/v1/chat/completions", body, true)
	// With no account there is nothing to call; the reference answers 503 and so
	// must this port, rather than fabricating an empty completion.
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := decode(t, rec)
	errObj, _ := got["error"].(map[string]any)
	if errObj == nil || errObj["type"] != "no_healthy_account" {
		t.Fatalf("error = %v", got["error"])
	}
}

func TestChatRejectsBadBodyBeforeTouchingUpstream(t *testing.T) {
	s := newTestServer(t)
	cases := []struct {
		body   string
		status int
	}{
		{`{"model":"m","messages":[]}`, http.StatusBadRequest},
		{`{"model":"m"}`, http.StatusBadRequest},
		{`not json`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		rec := do(t, s, http.MethodPost, "/v1/chat/completions", tc.body, true)
		if rec.Code != tc.status {
			t.Errorf("body %q: status = %d, want %d (%s)", tc.body, rec.Code, tc.status, rec.Body.String())
		}
	}
	// A GET must not be accepted as a chat call.
	rec := do(t, s, http.MethodGet, "/v1/chat/completions", "", true)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET chat: status = %d", rec.Code)
	}
}

// A rejected phone/code is an upstream business code on an HTTP 200 envelope.
// It must reach the operator as a 4xx; a 502 reads as "plugin broken".
func TestLoginErrorStatusKeepsUserMistakesAs4xx(t *testing.T) {
	status, kind := loginErrorStatus(&UpstreamError{
		Kind: "client", Status: http.StatusOK, Code: 400001, Msg: "Request data error",
	})
	if status != http.StatusBadRequest || kind != "invalid_request" {
		t.Fatalf("client error -> %d/%s, want 400/invalid_request", status, kind)
	}
	status, kind = loginErrorStatus(&UpstreamError{Kind: "server", Status: http.StatusBadGateway})
	if status != http.StatusBadGateway || kind != "upstream_error" {
		t.Fatalf("server error -> %d/%s, want 502/upstream_error", status, kind)
	}
	status, _ = loginErrorStatus(&UpstreamError{Kind: "soft_rate", Status: http.StatusTooManyRequests})
	if status != http.StatusTooManyRequests {
		t.Fatalf("soft rate -> %d, want 429", status)
	}
}

// Closing the dialog must settle the flow as failed, not leave it pending.
func TestLoginCancelMarksTheFlowFailed(t *testing.T) {
	s := newTestServer(t)
	rec := do(t, s, http.MethodPost, "/accounts/login", `{"method":"oauth"}`, true)
	sid, _ := decode(t, rec)["session_id"].(string)
	if sid == "" {
		t.Fatalf("start = %s", rec.Body.String())
	}
	rec = do(t, s, http.MethodDelete, "/accounts/login/"+sid, "", true)
	if got := decode(t, rec)["status"]; got != "failed" {
		t.Fatalf("cancel = %v, want failed", got)
	}
	rec = do(t, s, http.MethodGet, "/accounts/login/"+sid, "", true)
	if got := decode(t, rec)["status"]; got != "failed" {
		t.Fatalf("poll after cancel = %v, want failed", got)
	}
}

// The desktop import has no fallback: with no ~/.openclaw-autoclaw state the
// operator must get a readable 404 desktop_data_missing, never a 200 that looks
// like a completed login.
func TestLoginImportDesktopWithoutStateIs404(t *testing.T) {
	t.Setenv("USERPROFILE", t.TempDir())
	s := newTestServer(t)
	rec := do(t, s, http.MethodPost, "/accounts/login", `{"method":"global_import_desktop"}`, true)
	sid, _ := decode(t, rec)["session_id"].(string)
	if sid == "" {
		t.Fatalf("start = %s", rec.Body.String())
	}
	rec = do(t, s, http.MethodPost, "/accounts/login/"+sid+"/submit", `{"values":{"action":"import"}}`, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("import without desktop state = %d %s, want 404", rec.Code, rec.Body.String())
	}
	got := decode(t, rec)
	errObj, _ := got["error"].(map[string]any)
	if errObj == nil || errObj["type"] != "desktop_data_missing" {
		t.Fatalf("error = %v, want desktop_data_missing", got["error"])
	}
	if msg, _ := errObj["message"].(string); !strings.Contains(msg, ".openclaw-autoclaw") {
		t.Fatalf("message = %q, want it to name the desktop data directory", msg)
	}
	if s.st.configured() {
		t.Fatal("a missing desktop state must not persist an account")
	}
}
