package main

// Login-closure tests for MiniMax. The response bodies are the shapes the
// reference parses and the live endpoint returns (measured 2026-10-22):
//
//	device code 200 {"device_code","user_code","verification_uri",
//	                  "verification_uri_complete","expires_in","interval"}
//	pending      400 {"error":"authorization_pending",...}
//	slow_down    400 {"error":"slow_down",...}
//	grant        200 {"access_token","refresh_token","token_type":"Bearer",
//	                  "expires_in","scope":"agent.default"}
//
// The tests drive the real handlers and the real polling goroutine, so a break in
// the state machine, grant validation or persistence fails here.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const mmDeviceBody = `{"device_code":"dc-test","user_code":"5YEN-D3EU",` +
	`"verification_uri":"https://account.minimax.cn/oauth-authorize",` +
	`"verification_uri_complete":"https://account.minimax.cn/oauth-authorize?user_code=5YEN-D3EU",` +
	`"expires_in":300,"interval":3}`

func TestAbandonedPasteSessionIsSwept(t *testing.T) {
	srv, _ := newTestServer(t, "http://127.0.0.1:1")
	srv.loginMu.Lock()
	if srv.logins == nil {
		srv.logins = map[string]*loginFlow{}
	}
	srv.logins["stale-paste"] = &loginFlow{method: "paste_code", status: "pending", createdAt: time.Now().Add(-16 * time.Minute)}
	srv.logins["fresh-paste"] = &loginFlow{method: "paste_code", status: "pending", createdAt: time.Now()}
	srv.loginMu.Unlock()

	srv.sweepLogins()

	srv.loginMu.Lock()
	_, stale := srv.logins["stale-paste"]
	_, fresh := srv.logins["fresh-paste"]
	srv.loginMu.Unlock()
	if stale {
		t.Fatalf("an abandoned paste session survived the sweep")
	}
	if !fresh {
		t.Fatalf("a fresh paste session was swept")
	}
}

func testJWT(sub string, exp int64) string {
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"sub":%q,"exp":%d}`, sub, exp)))
	return head + "." + payload + ".sig"
}

func grantBody(token, refresh string) string {
	return fmt.Sprintf(`{"access_token":%q,"refresh_token":%q,"token_type":"Bearer",`+
		`"expires_in":3600,"scope":"agent.default"}`, token, refresh)
}

type mmHarness struct {
	srv     *server
	handler http.Handler
	mu      sync.Mutex
	sleeps  []time.Duration
	forms   []url.Values
}

func newMMHarness(t *testing.T, tokenHandler http.HandlerFunc) *mmHarness {
	t.Helper()
	h := &mmHarness{}
	mux := http.NewServeMux()
	mux.HandleFunc(deviceCodePath, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		h.mu.Lock()
		h.forms = append(h.forms, r.PostForm)
		h.mu.Unlock()
		_, _ = io.WriteString(w, mmDeviceBody)
	})
	mux.HandleFunc(tokenPath, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		h.mu.Lock()
		h.forms = append(h.forms, r.PostForm)
		h.mu.Unlock()
		tokenHandler(w, r)
	})
	// The paste flow validates a token with GET models; answer it locally so no
	// test touches the real API.
	mux.HandleFunc(modelsPath, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"id":"MiniMax-M2.1"}]}`)
	})
	mock := httptest.NewServer(mux)
	t.Cleanup(mock.Close)

	srv, err := newServer(pluginConfig{ID: "minimax", Token: "tk", DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	srv.client.accountBase = mock.URL
	oldAPI := apiBase
	apiBase = mock.URL
	t.Cleanup(func() { apiBase = oldAPI })
	h.srv, h.handler = srv, srv.routes()

	srv.slowDownStep = 5 * time.Second
	srv.pollSleep = func(d time.Duration) <-chan time.Time {
		h.mu.Lock()
		h.sleeps = append(h.sleeps, d)
		h.mu.Unlock()
		ch := make(chan time.Time)
		close(ch)
		return ch
	}

	return h
}

func (h *mmHarness) do(t *testing.T, method, path, body string) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Authorization", "Bearer tk")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func (h *mmHarness) awaitTerminal(t *testing.T, sid string) map[string]any {
	t.Helper()
	for i := 0; i < 400; i++ {
		code, body := h.do(t, http.MethodGet, "/accounts/login/"+sid, "")
		if code != http.StatusOK {
			t.Fatalf("poll status = %d", code)
		}
		if status, _ := body["status"].(string); status != "pending" {
			return body
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("login did not finish")
	return nil
}

// pendingSequence answers 400 pending, 400 slow_down, then a grant.
func pendingSequence(token string) http.HandlerFunc {
	calls := 0
	return func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch calls {
		case 1:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"authorization_pending","error_description":"the user has not finished authorizing yet"}`)
		case 2:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"slow_down"}`)
		default:
			_, _ = io.WriteString(w, grantBody(token, "rt-1"))
		}
	}
}

func TestMiniMaxDeviceLoginPendingSlowDownSuccess(t *testing.T) {
	exp := time.Now().Add(time.Hour).Unix()
	jwt := testJWT("acct-1", exp)
	h := newMMHarness(t, pendingSequence(jwt))

	code, start := h.do(t, http.MethodPost, "/accounts/login", `{"method":"oauth"}`)
	if code != http.StatusOK || start["status"] != "pending" {
		t.Fatalf("start = %d %v", code, start)
	}
	// M1: the names the host/front-end read.
	if start["url"] != "https://account.minimax.cn/oauth-authorize?user_code=5YEN-D3EU" || start["code"] != "5YEN-D3EU" {
		t.Fatalf("url/code = %v/%v", start["url"], start["code"])
	}
	sid, _ := start["session_id"].(string)
	end := h.awaitTerminal(t, sid)
	if end["status"] != "success" || end["account_id"] != "acct-1" {
		t.Fatalf("terminal = %v", end)
	}

	stored, ok := h.srv.store.get()
	if !ok || stored.AccessToken != jwt || stored.RefreshToken != "rt-1" {
		t.Fatalf("stored = %+v", stored)
	}
	if stored.TokenType != "Bearer" || stored.ExpiresAt != fmt.Sprintf("%d000", exp) || stored.AccountID != "acct-1" {
		t.Fatalf("stored grant metadata = %+v", stored)
	}
	_, accounts := h.do(t, http.MethodGet, "/accounts", "")
	list, _ := accounts["accounts"].([]any)
	if len(list) != 1 {
		t.Fatalf("accounts = %v", accounts)
	}
	acct := list[0].(map[string]any)
	cred := acct["credential"].(map[string]any)
	if acct["status"] != "ok" || cred["expires_at"] != float64(exp) {
		t.Fatalf("account = %v", acct)
	}

	// slow_down must grow the interval (3s -> 8s); pending keeps it.
	h.mu.Lock()
	sleeps := append([]time.Duration(nil), h.sleeps...)
	forms := append([]url.Values(nil), h.forms...)
	h.mu.Unlock()
	if len(sleeps) != 3 || sleeps[0] != 3*time.Second || sleeps[1] != 3*time.Second || sleeps[2] != 8*time.Second {
		t.Fatalf("poll intervals = %v, want [3s 3s 8s]", sleeps)
	}
	// The device-code request carries PKCE, and the poll echoes the verifier that
	// matches the challenge (reference createMinimaxPkce/buildMinimaxPollBody).
	dev := forms[0]
	if dev.Get("client_id") != productClientID || dev.Get("code_challenge_method") != "S256" || dev.Get("code_challenge") == "" {
		t.Fatalf("device form = %v", dev)
	}
	token := forms[1]
	if token.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" ||
		token.Get("client_id") != productClientID || token.Get("device_code") != "dc-test" {
		t.Fatalf("poll form = %v", token)
	}
	sum := sha256.Sum256([]byte(token.Get("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != dev.Get("code_challenge") {
		t.Fatal("code_verifier does not match the device-code challenge")
	}

	// A repeated poll after success stays success (host retries are normal).
	if _, again := h.do(t, http.MethodGet, "/accounts/login/"+sid, ""); again["status"] != "success" {
		t.Fatalf("repeat poll = %v", again)
	}
}

func TestMiniMaxDeviceLoginDeniedAndExpired(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"denied-status", http.StatusOK, `{"status":"denied"}`, "denied"},
		{"denied-error", http.StatusBadRequest, `{"error":"access_denied"}`, "denied"},
		{"expired-error", http.StatusBadRequest, `{"error":"expired_token"}`, "expired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newMMHarness(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			_, start := h.do(t, http.MethodPost, "/accounts/login", `{"method":"oauth"}`)
			sid, _ := start["session_id"].(string)
			end := h.awaitTerminal(t, sid)
			if end["status"] != "failed" || !strings.Contains(end["message"].(string), tc.want) {
				t.Fatalf("terminal = %v", end)
			}
			if h.srv.store.configured() {
				t.Fatal("a failed login must not store a credential")
			}
		})
	}
}

func TestMiniMaxDeviceLoginValidatesTheGrant(t *testing.T) {
	jwt := testJWT("acct-1", 1792713600)
	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing-refresh", fmt.Sprintf(`{"access_token":%q,"token_type":"Bearer","expires_in":3600,"scope":"agent.default"}`, jwt), "refresh_token"},
		{"bad-token-type", strings.Replace(grantBody(jwt, "rt"), `"Bearer"`, `"mac"`, 1), "token_type"},
		{"missing-expiry", strings.Replace(grantBody(jwt, "rt"), `"expires_in":3600`, `"expires_in":0`, 1), "expires_in"},
		{"wrong-scope", strings.Replace(grantBody(jwt, "rt"), `"agent.default"`, `"other.scope"`, 1), "scope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newMMHarness(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			})
			_, start := h.do(t, http.MethodPost, "/accounts/login", `{"method":"oauth"}`)
			sid, _ := start["session_id"].(string)
			end := h.awaitTerminal(t, sid)
			if end["status"] != "failed" || !strings.Contains(end["message"].(string), tc.want) {
				t.Fatalf("terminal = %v, want failure mentioning %s", end, tc.want)
			}
		})
	}
	// token_type is compared case-insensitively, as the reference does.
	t.Run("lowercase-bearer", func(t *testing.T) {
		h := newMMHarness(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"access_token":"`+jwt+`","refresh_token":"rt","token_type":"bearer","expires_in":3600,"scope":"agent.default"}`)
		})
		_, start := h.do(t, http.MethodPost, "/accounts/login", `{"method":"oauth"}`)
		sid, _ := start["session_id"].(string)
		if end := h.awaitTerminal(t, sid); end["status"] != "success" {
			t.Fatalf("lowercase bearer rejected: %v", end)
		}
	})
}
