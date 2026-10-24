package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"aiworkgate/internal/model"
)

// newEndpointManager returns a manager whose only plugin "fake" already looks
// running and points at handler, so the typed client can be exercised without
// launching a process.
func newEndpointManager(t *testing.T, handler http.Handler) *Manager {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	_, portText, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatalf("parse test server url %q: %v", server.URL, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse test server port %q: %v", portText, err)
	}
	return &Manager{
		plugins: map[string]*pluginState{
			"fake": {
				id:      "fake",
				enabled: true,
				running: true,
				healthy: true,
				port:    port,
				token:   "test-token",
			},
		},
		now: time.Now,
	}
}

func TestResetIdentitySendsPathAndBody(t *testing.T) {
	type captured struct {
		method string
		path   string
		auth   string
		ctype  string
		body   map[string]string
	}
	var got captured
	mgr := newEndpointManager(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		got.method = r.Method
		got.path = r.URL.Path
		got.auth = r.Header.Get("Authorization")
		got.ctype = r.Header.Get("Content-Type")
		if err := json.Unmarshal(data, &got.body); err != nil {
			t.Errorf("decode request body %q: %v", data, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	if err := mgr.ResetIdentity(context.Background(), "fake", "acc_1", "os"); err != nil {
		t.Fatalf("ResetIdentity: %v", err)
	}
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if got.path != "/accounts/acc_1/identity/reset" {
		t.Errorf("path = %q, want /accounts/acc_1/identity/reset", got.path)
	}
	if got.auth != "Bearer test-token" {
		t.Errorf("authorization = %q, want the plugin token", got.auth)
	}
	if !strings.HasPrefix(got.ctype, "application/json") {
		t.Errorf("content-type = %q, want application/json", got.ctype)
	}
	if got.body["mode"] != "os" {
		t.Errorf("body mode = %q, want os", got.body["mode"])
	}

	// The mode is normalized, and account ids are path-escaped.
	var escaped string
	mgr = newEndpointManager(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		escaped = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	if err := mgr.ResetIdentity(context.Background(), "fake", "acc 1", " Request "); err != nil {
		t.Fatalf("ResetIdentity(request): %v", err)
	}
	if escaped != "/accounts/acc%201/identity/reset" {
		t.Errorf("escaped path = %q, want the account id escaped", escaped)
	}
}

func TestResetIdentitySurfacesPluginError(t *testing.T) {
	mgr := newEndpointManager(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"message":"os identity is locked by policy","type":"conflict"}}`))
	}))

	err := mgr.ResetIdentity(context.Background(), "fake", "acc_1", "os")
	if err == nil {
		t.Fatal("ResetIdentity returned no error for a 409 answer")
	}
	if !strings.Contains(err.Error(), "os identity is locked by policy") {
		t.Fatalf("error = %v, want the plugin message", err)
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusConflict {
		t.Fatalf("error = %v, want an HTTPError with status 409", err)
	}
}

// TestCallTracksInFlight pins the idle-reaper guard: a proxied call is counted
// as in flight for its whole duration and released afterwards, including when
// the plugin answers with an error.
func TestCallTracksInFlight(t *testing.T) {
	var mgr *Manager
	var observed []int
	mgr = newEndpointManager(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mgr.mu.Lock()
		observed = append(observed, mgr.plugins["fake"].inflight)
		mgr.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/meta":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
		default:
			_, _ = w.Write([]byte(`{"accounts":[]}`))
		}
	}))

	if _, err := mgr.Accounts(context.Background(), "fake"); err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if _, err := mgr.Meta(context.Background(), "fake"); err == nil {
		t.Fatal("Meta should surface the 500")
	}

	if len(observed) != 2 || observed[0] != 1 || observed[1] != 1 {
		t.Fatalf("in-flight counts seen by the plugin = %v, want [1 1]", observed)
	}
	mgr.mu.Lock()
	after := mgr.plugins["fake"].inflight
	mgr.mu.Unlock()
	if after != 0 {
		t.Fatalf("in-flight count after the calls = %d, want 0", after)
	}
}

// TestSetUpstreamPostsBaseURLAndKeyToTheAdminEndpoint pins the Q18 handover
// shape: POST /admin/upstream with exactly {base_url, api_key} under the plugin
// bearer token, and a 2xx answer (the plugin echoes a fingerprint) is success.
func TestSetUpstreamPostsBaseURLAndKeyToTheAdminEndpoint(t *testing.T) {
	type captured struct {
		method string
		path   string
		auth   string
		ctype  string
		body   map[string]any
	}
	var got captured
	mgr := newEndpointManager(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		got.method, got.path = r.Method, r.URL.Path
		got.auth, got.ctype = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		if err := json.Unmarshal(data, &got.body); err != nil {
			t.Errorf("decode request body %q: %v", data, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"base_url":"http://127.0.0.1:17864","key_present":true,"key_fingerprint":"sha256:305b2ac4"}`))
	}))

	if err := mgr.SetUpstream(context.Background(), "fake", "http://127.0.0.1:17864/", "sk-SMOKE-T3-KEY"); err != nil {
		t.Fatalf("SetUpstream: %v", err)
	}
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if got.path != "/admin/upstream" {
		t.Errorf("path = %q, want /admin/upstream", got.path)
	}
	if got.auth != "Bearer test-token" {
		t.Errorf("authorization = %q, want the plugin token, not the A key", got.auth)
	}
	if !strings.HasPrefix(got.ctype, "application/json") {
		t.Errorf("content-type = %q, want application/json", got.ctype)
	}
	if got.body["base_url"] != "http://127.0.0.1:17864" {
		t.Errorf("body base_url = %v, want the trailing slash trimmed", got.body["base_url"])
	}
	if got.body["api_key"] != "sk-SMOKE-T3-KEY" {
		t.Errorf("body api_key = %v, want the handed-over key", got.body["api_key"])
	}
	if len(got.body) != 2 {
		t.Errorf("body keys = %v, want exactly base_url and api_key", got.body)
	}
}

// TestSetUpstreamSurfacesPluginError: a 500 is an error with the plugin's
// message, and the key never appears in it.
func TestSetUpstreamSurfacesPluginError(t *testing.T) {
	const key = "sk-SMOKE-T3-KEY"
	mgr := newEndpointManager(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"sealing is unavailable","type":"internal_error"}}`))
	}))

	err := mgr.SetUpstream(context.Background(), "fake", "http://127.0.0.1:17864", key)
	if err == nil {
		t.Fatal("SetUpstream returned no error for a 500 answer")
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusInternalServerError {
		t.Fatalf("error = %v, want an HTTPError with status 500", err)
	}
	if !strings.Contains(err.Error(), "sealing is unavailable") {
		t.Fatalf("error = %v, want the plugin message", err)
	}
	if strings.Contains(err.Error(), key) {
		t.Fatalf("error %q leaks the api_key", err)
	}
}

// TestSetUpstreamIsIdempotent: pushing the same pair twice reaches the plugin
// twice and both succeed, so a host restart does not have to know whether an
// earlier push already landed.
func TestSetUpstreamIsIdempotent(t *testing.T) {
	var posts int
	var bodies []string
	mgr := newEndpointManager(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		posts++
		bodies = append(bodies, string(data))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	for i := 0; i < 2; i++ {
		if err := mgr.SetUpstream(context.Background(), "fake", "http://127.0.0.1:17864", "sk-SMOKE-T3-KEY"); err != nil {
			t.Fatalf("push %d: %v", i+1, err)
		}
	}
	if posts != 2 {
		t.Fatalf("posts = %d, want 2", posts)
	}
	if bodies[0] != bodies[1] {
		t.Fatalf("bodies differ between identical pushes: %q vs %q", bodies[0], bodies[1])
	}
}

// TestSetUpstreamRejectsEmptyValues: the plugin stores whatever pair it is
// given, so an empty key must be refused before any request is sent.
func TestSetUpstreamRejectsEmptyValues(t *testing.T) {
	var calls int
	mgr := newEndpointManager(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	cases := []struct {
		name    string
		baseURL string
		apiKey  string
	}{
		{"empty key", "http://127.0.0.1:17864", ""},
		{"blank key", "http://127.0.0.1:17864", "   "},
		{"empty base url", "", "sk-SMOKE-T3-KEY"},
		{"relative base url", "127.0.0.1:17864", "sk-SMOKE-T3-KEY"},
	}
	for _, tc := range cases {
		if err := mgr.SetUpstream(context.Background(), "fake", tc.baseURL, tc.apiKey); err == nil {
			t.Fatalf("%s: SetUpstream returned no error", tc.name)
		}
	}
	if calls != 0 {
		t.Fatalf("plugin was called %d times, want 0 rejected requests", calls)
	}
}

func TestLoginStartFillsURLFromAliases(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"canonical url", `{"session_id":"s","status":"pending","url":"https://a.example/x"}`, "https://a.example/x"},
		{"login_url", `{"session_id":"s","status":"pending","login_url":"https://b.example/portal#/login"}`, "https://b.example/portal#/login"},
		{"authorize_url", `{"session_id":"s","status":"pending","authorize_url":"https://c.example/auth"}`, "https://c.example/auth"},
		{"verification_uri_complete", `{"session_id":"s","status":"pending","verification_uri_complete":"https://d.example/device?code=1"}`, "https://d.example/device?code=1"},
		{"no alias", `{"session_id":"s","status":"pending","instructions":"type a code"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := newEndpointManager(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			start, err := mgr.LoginStart(context.Background(), "fake", "oauth")
			if err != nil {
				t.Fatalf("LoginStart: %v", err)
			}
			if start.URL != tc.want {
				t.Errorf("URL = %q, want %q (alias dropped before the UI sees it)", start.URL, tc.want)
			}
		})
	}
}

// A plugin may report an unmetered account. The host must keep the flag
// through the /accounts decode, otherwise the zero counters look depleted.
func TestAccountsKeepsUnlimitedQuota(t *testing.T) {
	var payload model.AccountsResponse
	if err := json.Unmarshal([]byte(`{"accounts":[{"id":"a1","quota":{"unlimited":true}}]}`), &payload); err != nil {
		t.Fatalf("decode accounts: %v", err)
	}
	if len(payload.Accounts) != 1 || !payload.Accounts[0].Quota.Unlimited {
		t.Fatalf("accounts = %+v, want the unlimited flag", payload.Accounts)
	}
}
