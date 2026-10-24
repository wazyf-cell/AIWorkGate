package main

// Login-closure tests. Every response body below is the shape the reference and
// the live endpoints actually return (measured 2026-10-22):
//
//	device authorization 200 {"device_code","user_code","verification_uri",
//	                            "verification_uri_complete","expires_in","interval"}
//	pending               400 {"error":"authorization_pending",...}
//	slow_down             400 {"error":"slow_down",...}
//	authorized            200 {"access_token","refresh_token","token_type",...}
//
// They drive the real handlers and the real polling goroutine, so a break in the
// state machine, the persistence step or the account projection fails here.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const deviceAuthBody = `{"device_code":"dc-test","user_code":"ABCD-EFGH",` +
	`"verification_uri":"https://authkit.cline.bot/device",` +
	`"verification_uri_complete":"https://authkit.cline.bot/device?user_code=ABCD-EFGH",` +
	`"expires_in":300,"interval":5}`

const registerBody = `{"accessToken":"workos:at-1","refreshToken":"rt-1",` +
	`"expiresAt":"2026-10-23T00:00:00Z","accountId":"acc_1",` +
	`"userInfo":{"clineUserId":"user_1","email":"a@b.c","firstName":"Ada","lastName":"Lovelace"}}`

// loginHarness wires a server to a mock WorkOS + Cline gateway and records the
// sleep durations of the polling loop.
type loginHarness struct {
	srv        *server
	handler    http.Handler
	mu         sync.Mutex
	sleeps     []time.Duration
	authCalls  int
	chatStatus int // 0 = accept, otherwise the status the probe receives
}

func newLoginHarness(t *testing.T) *loginHarness {
	t.Helper()
	h := &loginHarness{}
	mux := http.NewServeMux()
	mux.HandleFunc("/user_management/authorize/device", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, deviceAuthBody)
	})
	mux.HandleFunc("/user_management/authenticate", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.authCalls++
		n := h.authCalls
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"authorization_pending","error_description":"The authorization request is still pending user approval."}`)
		case 2:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"slow_down"}`)
		default:
			_, _ = io.WriteString(w, `{"access_token":"at-1","refresh_token":"rt-1","token_type":"Bearer"}`)
		}
	})
	mux.HandleFunc("/api/v1/auth/register", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, registerBody)
	})
	mux.HandleFunc("/api/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		status := h.chatStatus
		h.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":{"message":"invalid token"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[]}\n\ndata: [DONE]\n\n")
	})
	mock := httptest.NewServer(mux)
	t.Cleanup(mock.Close)

	srv, err := newServer(serverConfig{ID: "cline", Token: "tk", DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	srv.client.workOs = mock.URL
	srv.client.api = mock.URL
	h.srv, h.handler = srv, srv.routes()

	srv.slowDownStep = time.Second
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

func (h *loginHarness) do(t *testing.T, method, path, body string) (int, map[string]any) {
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

func (h *loginHarness) awaitTerminal(t *testing.T, sid string) map[string]any {
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

func TestDeviceLoginPendingSlowDownThenSuccess(t *testing.T) {
	h := newLoginHarness(t)
	code, start := h.do(t, http.MethodPost, "/accounts/login", `{"method":"oauth"}`)
	if code != http.StatusOK || start["status"] != "pending" {
		t.Fatalf("start = %d %v", code, start)
	}
	// M1: the fields the host/front-end read must be present and named exactly.
	if start["url"] != "https://authkit.cline.bot/device?user_code=ABCD-EFGH" || start["code"] != "ABCD-EFGH" {
		t.Fatalf("start URL/code = %v/%v", start["url"], start["code"])
	}
	sid, _ := start["session_id"].(string)
	end := h.awaitTerminal(t, sid)
	if end["status"] != "success" || end["account_id"] != "default" {
		t.Fatalf("terminal = %v", end)
	}

	// The token was persisted bare, the refresh token and expiry survived, and the
	// register-time identity was projected onto the account label.
	stored, ok := h.srv.store.get()
	if !ok || stored.AccessToken != "at-1" || stored.RefreshToken != "rt-1" {
		t.Fatalf("stored = %+v", stored)
	}
	if stored.ExpireTime != 1792713600000 {
		t.Fatalf("expiry = %d", stored.ExpireTime)
	}
	if stored.AccountID != "user_1" || stored.label() != "Ada Lovelace" {
		t.Fatalf("identity = %+v", stored)
	}
	// /accounts must now list exactly one account.
	_, accounts := h.do(t, http.MethodGet, "/accounts", "")
	list, _ := accounts["accounts"].([]any)
	if len(list) != 1 {
		t.Fatalf("accounts = %v", accounts)
	}

	// slow_down must grow the interval (5s -> 6s); pending keeps it.
	h.mu.Lock()
	sleeps := append([]time.Duration(nil), h.sleeps...)
	h.mu.Unlock()
	if len(sleeps) != 3 || sleeps[0] != 5*time.Second || sleeps[1] != 5*time.Second || sleeps[2] != 6*time.Second {
		t.Fatalf("poll intervals = %v, want [5s 5s 6s]", sleeps)
	}

	// A repeated poll after success must stay success (host retries are normal).
	_, again := h.do(t, http.MethodGet, "/accounts/login/"+sid, "")
	if again["status"] != "success" {
		t.Fatalf("repeat poll = %v", again)
	}
}

func TestDeviceLoginDeniedAndExpired(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"denied", `{"error":"access_denied","error_description":"The user denied the request"}`, "授权被拒绝"},
		{"expired", `{"error":"expired_token"}`, "设备码已过期"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newLoginHarness(t)
			mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, deviceAuthorizationPath) {
					_, _ = io.WriteString(w, deviceAuthBody)
					return
				}
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer mock.Close()
			h.srv.client.workOs = mock.URL

			_, start := h.do(t, http.MethodPost, "/accounts/login", `{"method":"oauth"}`)
			sid, _ := start["session_id"].(string)
			end := h.awaitTerminal(t, sid)
			if end["status"] != "failed" || !strings.Contains(end["error"].(string), tc.want) {
				t.Fatalf("%s terminal = %v", tc.name, end)
			}
			if h.srv.store.configured() {
				t.Fatal("a failed login must not store a credential")
			}
		})
	}
}

func TestDeviceLoginRejectsMissingTokenFields(t *testing.T) {
	h := newLoginHarness(t)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, deviceAuthorizationPath) {
			_, _ = io.WriteString(w, deviceAuthBody)
			return
		}
		// access_token without refresh_token is not a complete WorkOS grant.
		_, _ = io.WriteString(w, `{"access_token":"at-only","token_type":"Bearer"}`)
	}))
	defer mock.Close()
	h.srv.client.workOs = mock.URL

	_, start := h.do(t, http.MethodPost, "/accounts/login", `{"method":"oauth"}`)
	sid, _ := start["session_id"].(string)
	end := h.awaitTerminal(t, sid)
	if end["status"] != "failed" || !strings.Contains(end["error"].(string), "缺少必要字段") {
		t.Fatalf("terminal = %v", end)
	}
}

func TestLoginRejectsUnknownMethod(t *testing.T) {
	h := newLoginHarness(t)
	code, body := h.do(t, http.MethodPost, "/accounts/login", `{"method":"cookie"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %v", code, body)
	}
}
