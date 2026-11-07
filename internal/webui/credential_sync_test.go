package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// credentialSyncOptions builds the receive/push wiring the tests exercise.
// The token reader and the applier are in-memory doubles; the push callback is
// only used for the 501 answer of an unwired host.
func credentialSyncOptions(token string, haveToken bool) Options {
	return Options{
		CredentialSyncToken: func() (string, bool) {
			return token, haveToken
		},
		SyncCredentials: func(pluginID string, items []SyncCredentialItem) (SyncResult, error) {
			views := make([]SyncCredentialView, 0, len(items))
			for _, it := range items {
				keys := make([]string, 0, len(it.Secret))
				for k := range it.Secret {
					keys = append(keys, k)
				}
				views = append(views, SyncCredentialView{AccountID: it.AccountID, Keys: keys, Present: true})
			}
			return SyncResult{PluginID: pluginID, Count: len(views), Credentials: views}, nil
		},
	}
}

// postInternal sends one receive request as a loopback caller (the httptest
// handler runs in-process, so RemoteAddr is the loopback 192.0.2.1:1234 style
// unless overridden).
func postInternal(t *testing.T, handler http.Handler, remote, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, internalCredentialsEndpoint, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = remote
	if token != "" {
		request.Header.Set("X-Sync-Token", token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

const syncBody = `{"plugin_id":"bridge-wb","credentials":[{"account_id":"acc-1","secret":{"access":"SECRET_ACCESS","refresh":"SECRET_REFRESH"}}]}`

func TestInternalCredentialsAcceptsLoopbackWithToken(t *testing.T) {
	handler := New(credentialSyncOptions("tok-123", true))
	recorder := postInternal(t, handler, "127.0.0.1:40000", "tok-123", syncBody)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var result SyncResult
	decodeJSONBody(t, recorder, &result)
	if result.PluginID != "bridge-wb" || result.Count != 1 {
		t.Fatalf("result = %+v, want plugin bridge-wb with 1 credential", result)
	}
	if len(result.Credentials) != 1 || result.Credentials[0].AccountID != "acc-1" || !result.Credentials[0].Present {
		t.Fatalf("credentials = %+v, want acc-1 present", result.Credentials)
	}
}

func TestInternalCredentialsRedactsSecrets(t *testing.T) {
	handler := New(credentialSyncOptions("tok-123", true))
	recorder := postInternal(t, handler, "127.0.0.1:40000", "tok-123", syncBody)
	body := recorder.Body.String()
	if strings.Contains(body, "SECRET_ACCESS") || strings.Contains(body, "SECRET_REFRESH") {
		t.Fatalf("response leaked the secret: %s", body)
	}
	if !strings.Contains(body, `"keys"`) || !strings.Contains(body, `"access"`) {
		t.Fatalf("response should carry redacted key names: %s", body)
	}
}

func TestInternalCredentialsRejectsRemoteSource(t *testing.T) {
	handler := New(credentialSyncOptions("tok-123", true))
	recorder := postInternal(t, handler, "10.0.0.5:1234", "tok-123", syncBody)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("remote status = %d, want 403 loopback_only", recorder.Code)
	}
	// IPv6 remote fails the same way.
	recorder = postInternal(t, handler, "[2001:db8::1]:1234", "tok-123", syncBody)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("remote v6 status = %d, want 403", recorder.Code)
	}
}

func TestInternalCredentialsTokenChecks(t *testing.T) {
	handler := New(credentialSyncOptions("tok-123", true))
	cases := []struct {
		name  string
		token string
		want  int
	}{
		{"missing", "", http.StatusUnauthorized},
		{"wrong", "nope", http.StatusUnauthorized},
		{"correct", "tok-123", http.StatusOK},
		// Loopback-IPv6 source with the right token passes: loopback is loopback.
		{"v6-correct", "tok-123", http.StatusOK},
	}
	for _, tc := range cases {
		remote := "127.0.0.1:40001"
		if tc.name == "v6-correct" {
			remote = "[::1]:40001"
		}
		if recorder := postInternal(t, handler, remote, tc.token, syncBody); recorder.Code != tc.want {
			t.Fatalf("%s: status = %d, want %d (body %s)", tc.name, recorder.Code, tc.want, recorder.Body.String())
		}
	}
}

func TestInternalCredentialsUnconfiguredToken503(t *testing.T) {
	handler := New(credentialSyncOptions("", false))
	recorder := postInternal(t, handler, "127.0.0.1:40000", "tok-123", syncBody)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	var payload struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	decodeJSONBody(t, recorder, &payload)
	if payload.Error.Type != "sync_token_unconfigured" {
		t.Fatalf("type = %q, want sync_token_unconfigured", payload.Error.Type)
	}
}

func TestInternalCredentialsBadRequest(t *testing.T) {
	handler := New(credentialSyncOptions("tok", true))
	cases := []struct {
		name string
		body string
	}{
		{"no plugin", `{"credentials":[{"account_id":"a","secret":{}}]}`},
		{"no credentials", `{"plugin_id":"p","credentials":[]}`},
		{"no account", `{"plugin_id":"p","credentials":[{"secret":{}}]}`},
		{"secret not object", `{"plugin_id":"p","credentials":[{"account_id":"a","secret":"x"}]}`},
		{"malformed json", `{`},
	}
	for _, tc := range cases {
		if recorder := postInternal(t, handler, "127.0.0.1:40000", "tok", tc.body); recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400 (body %s)", tc.name, recorder.Code, recorder.Body.String())
		}
	}
}

func TestInternalCredentialsUnwiredHost(t *testing.T) {
	handler := New(Options{})
	recorder := postInternal(t, handler, "127.0.0.1:40000", "tok", syncBody)
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", recorder.Code)
	}
}

func TestInternalCredentialsMethodAndGuardExemption(t *testing.T) {
	handler := New(credentialSyncOptions("tok", true))
	// GET is a 405, not a guard 403: the route exists and is POST-only.
	request := httptest.NewRequest(http.MethodGet, internalCredentialsEndpoint, nil)
	request.RemoteAddr = "127.0.0.1:40000"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", recorder.Code)
	}
	// The guard exemption means no session cookie is needed: proven by every
	// 200 above (doRequest would have exchanged a cookie; postInternal does not).
}

func TestValidSyncTarget(t *testing.T) {
	cases := []struct {
		target string
		want   bool
	}{
		{"user@server.example.com", true},
		{"u@h", true},
		{"u@192.168.1.9", true},
		{"u@[2001:db8::1]", true},
		{"u@h:2222", true},
		{"u@h:1", true},
		{"", false},
		{"noat.example.com", false},
		{"u@h extra", false},
		{"u@h;rm -rf /", false},
		{"u@h&&evil", false},
		{"$(calc)@h", false},
		{"u '` @h", false},
		{"u@h:99999", false},
	}
	for _, tc := range cases {
		if got := ValidSyncTarget(tc.target); got != tc.want {
			t.Errorf("ValidSyncTarget(%q) = %v, want %v", tc.target, got, tc.want)
		}
	}
}

func TestCredentialsSyncPushEndpointWiring(t *testing.T) {
	// An unwired host answers 501 on the push endpoint (normal admin route,
	// so this also exercises route registration).
	handler := New(Options{})
	body := strings.NewReader(`{"target":"u@h","dry_run":true}`)
	request := httptest.NewRequest(http.MethodPost, "/api/credentials/sync", body)
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(hostSessionCookieFor(t, handler))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 (body %s)", recorder.Code, recorder.Body.String())
	}
}

func TestCredentialsSyncRejectsBadTarget(t *testing.T) {
	handler := New(Options{
		CredentialsSync: func(req CredentialsSyncRequest) (*CredentialsSyncResult, error) {
			t.Fatal("the host callback must not run for a bad target")
			return nil, nil
		},
	})
	request := httptest.NewRequest(http.MethodPost, "/api/credentials/sync", strings.NewReader(`{"target":"u@h; rm -rf /"}`))
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(hostSessionCookieFor(t, handler))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
	}
	_ = json.RawMessage{}
}
