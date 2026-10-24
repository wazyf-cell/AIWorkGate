package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

const testToken = "test-plugin-token"

// newTestServer builds a server whose data dir is a t.TempDir and whose
// upstream is already configured to point at baseURL.
func newTestServer(t *testing.T, baseURL, apiKey string) *server {
	t.Helper()
	s, err := newServer(serverConfig{
		ID:        pluginID,
		Token:     testToken,
		DataDir:   t.TempDir(),
		APILevel:  apiLevel,
		StartedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	if baseURL != "" {
		if err := s.store.set(baseURL, apiKey); err != nil {
			t.Fatalf("store.set: %v", err)
		}
	}
	return s
}

func doReq(t *testing.T, h http.Handler, method, path, body string, token string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// TestHealthIsUnauthenticatedButChatIsNot covers the auth boundary: the host
// polls GET /health with the token, but a human debugging by hand has none, and
// the endpoint leaks only liveness. Every other route (inference included) must
// reject a request without the token, otherwise any local process could spend
// the assistant's credentials.
func TestHealthIsUnauthenticatedButChatIsNot(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:1", "sk-not-used")
	h := s.handler()

	// 1. /health answers 200 without any Authorization header.
	rec := doReq(t, h, http.MethodGet, "/health", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("/health without token = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var health map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &health); err != nil {
		t.Fatalf("/health body is not JSON: %v", err)
	}
	if ok, _ := health["ok"].(bool); !ok {
		t.Fatalf("/health ok = %v, want true; body=%s", health["ok"], rec.Body.String())
	}
	if status, _ := health["status"].(string); !strings.EqualFold(status, "ok") {
		t.Fatalf("/health status = %q, want ok", status)
	}

	// 2. /v1/chat/completions without a token is rejected before any upstream
	//    call. The upstream base URL above is a closed port, so a 502 here
	//    would prove the request slipped past auth.
	rec = doReq(t, h, http.MethodPost, "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/v1/chat/completions without token = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	var errResp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("401 body is not the OpenAI error envelope: %v", err)
	}
	if errResp.Error.Code != "unauthorized" {
		t.Fatalf("401 error.code = %q, want unauthorized", errResp.Error.Code)
	}

	// 3. A wrong token is rejected too.
	rec = doReq(t, h, http.MethodPost, "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`, "wrong-token")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token = %d, want 401", rec.Code)
	}

	// 4. A non-health control route (/meta) is not public either.
	rec = doReq(t, h, http.MethodGet, "/meta", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/meta without token = %d, want 401", rec.Code)
	}
}

// TestChatStreamsSSEThroughFromUpstream is the core passthrough test: a fake
// assistant answers 200 + SSE, and the plugin must relay the status, the
// event-stream content type and every byte unchanged (no buffering, no
// re-framing), while replacing the caller's plugin token with the stored key.
func TestChatStreamsSSEThroughFromUpstream(t *testing.T) {
	const wantKey = "sk-assistant-test-key"

	var gotAuth, gotPath, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		if r.Header.Get("X-Client-Key") != "" {
			t.Errorf("host bookkeeping header X-Client-Key leaked upstream: %q", r.Header.Get("X-Client-Key"))
		}

		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for _, chunk := range []string{
			"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n",
			"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"llo\"}}]}\n\n",
			"data: [DONE]\n\n",
		} {
			_, _ = io.WriteString(w, chunk)
			if fl != nil {
				fl.Flush()
			}
		}
	}))
	defer upstream.Close()

	s := newTestServer(t, upstream.URL, wantKey)
	rec := doReq(t, s.handler(), http.MethodPost, "/v1/chat/completions",
		`{"model":"glm-5.3","stream":true,"messages":[{"role":"user","content":"hi"}]}`, testToken)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q, want /v1/chat/completions", gotPath)
	}
	if gotAuth != "Bearer "+wantKey {
		t.Fatalf("upstream Authorization = %q, want Bearer <stored key>", gotAuth)
	}
	if !strings.Contains(gotBody, `"glm-5.3"`) || !strings.Contains(gotBody, `"stream":true`) {
		t.Fatalf("upstream body was rewritten or dropped: %q", gotBody)
	}

	want := "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"llo\"}}]}\n\n" +
		"data: [DONE]\n\n"
	if got := rec.Body.String(); got != want {
		t.Fatalf("SSE body not passed through byte-for-byte:\n got=%q\nwant=%q", got, want)
	}

	// The store round-trips through DPAPI on Windows, so the key must survive
	// a reload from disk and never appear in the /meta payload.
	base, key, ok := s.store.get()
	if !ok || base != upstream.URL || key != wantKey {
		t.Fatalf("store.get() = (%q, %q, %v), want (%q, %q, true)", base, key, ok, upstream.URL, wantKey)
	}
	meta := doReq(t, s.handler(), http.MethodGet, "/meta", "", testToken)
	if strings.Contains(meta.Body.String(), wantKey) {
		t.Fatalf("/meta leaked the upstream key: %s", meta.Body.String())
	}
}

// TestModelsForwardsUpstreamList checks GET /v1/models relays the assistant's
// catalogue and reports 503 (not a bare 200 with an empty list) before the
// upstream has been configured, so the host can tell "not ready" from "no
// models".
func TestModelsForwardsUpstreamList(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("upstream path = %q, want /v1/models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-m" {
			t.Errorf("upstream Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"glm-5.3","object":"model","name":"GLM 5.3"}]}`)
	}))
	defer upstream.Close()

	s := newTestServer(t, upstream.URL, "sk-m")
	rec := doReq(t, s.handler(), http.MethodGet, "/v1/models", "", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if len(payload.Data) != 1 || payload.Data[0].ID != "glm-5.3" {
		t.Fatalf("models = %+v, want [glm-5.3]", payload.Data)
	}

	// Not configured yet: 503 with an explicit code.
	empty := newTestServer(t, "", "")
	rec = doReq(t, empty.handler(), http.MethodGet, "/v1/models", "", testToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured /v1/models = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
}

// TestAccountsSatisfySchedulerPort is a host-contract test, not a plugin
// feature: the host builds its scheduling pool from GET /accounts and requires a
// selectable account for every plugin request, otherwise every aichat/<model>
// call dies with 503 ErrNoCandidate before it reaches this plugin
// (internal/scheduler/scheduler.go evaluateAccountsLocked; reproduced by
// internal/gateway/dispatch_test.go TestScheduledChatReportsNoCandidate).
//
// It also pins the two credit-filter traps:
//   - Credential.Present must be true, else the account is filtered as
//     "no credential".
//   - quota must stay without numbers, because Remaining<=0 together with
//     Total>0 (or ExpiresAt>0) parks the account as "credits depleted"
//     (scheduler.go creditIneligibleReason).
func TestAccountsSatisfySchedulerPort(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	s := newTestServer(t, upstream.URL, "sk-acc")
	rec := doReq(t, s.handler(), http.MethodGet, "/accounts", "", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Accounts []struct {
			ID         string `json:"id"`
			Enabled    bool   `json:"enabled"`
			Status     string `json:"status"`
			Credential *struct {
				Present bool `json:"present"`
			} `json:"credential"`
			Quota struct {
				Remaining float64 `json:"remaining"`
				Total     float64 `json:"total"`
				ExpiresAt int64   `json:"expires_at"`
			} `json:"quota"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body is not the accounts envelope: %v", err)
	}
	if len(payload.Accounts) != 1 {
		t.Fatalf("accounts = %d, want exactly 1 (0 would make the scheduler refuse every request)",
			len(payload.Accounts))
	}
	acc := payload.Accounts[0]
	if !acc.Enabled {
		t.Fatal("account must be enabled or the scheduler filters it")
	}
	if acc.Status != "ok" {
		t.Fatalf("status = %q, want ok", acc.Status)
	}
	if acc.Credential == nil || !acc.Credential.Present {
		t.Fatalf("credential.present must be true (scheduler filters \"no credential\"): %+v", acc.Credential)
	}
	if acc.Quota.Total > 0 || acc.Quota.ExpiresAt > 0 {
		t.Fatalf("quota must stay unstated (Remaining<=0 with Total/ExpiresAt>0 = \"credits depleted\"): %+v", acc.Quota)
	}

	// An unconfigured plugin reports no accounts at all — the host then has
	// nothing to schedule, which is the honest "not ready" state.
	empty := newTestServer(t, "", "")
	rec = doReq(t, empty.handler(), http.MethodGet, "/accounts", "", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("unconfigured /accounts = %d, want 200", rec.Code)
	}
	var emptyPayload struct {
		Accounts []any `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &emptyPayload); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if len(emptyPayload.Accounts) != 0 {
		t.Fatalf("unconfigured accounts = %d, want 0", len(emptyPayload.Accounts))
	}
}

// TestCheckinAndRefreshAnswer501 pins the honest failure behind the missing
// checkin/refresh capabilities: the manifest declares neither, so the route must
// answer 501 with a stable code rather than a 404 (looks like a typo) or a fake
// success (the host would believe a check-in happened).
func TestCheckinAndRefreshAnswer501(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:1", "sk-unused")
	for _, action := range []string{"checkin", "refresh"} {
		rec := doReq(t, s.handler(), http.MethodPost, "/accounts/default/"+action, "", testToken)
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("POST /accounts/default/%s = %d, want 501; body=%s", action, rec.Code, rec.Body.String())
		}
		var errResp struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
			t.Fatalf("%s body is not the OpenAI error envelope: %v", action, err)
		}
		if errResp.Error.Code != "not_implemented" {
			t.Fatalf("%s error.code = %q, want not_implemented", action, errResp.Error.Code)
		}
	}
}

// TestUpstreamStorePersistsAndRejectsBadInput covers the file-backed store: the
// credential survives a fresh store on the same directory (i.e. it really was
// written to disk, sealed) and a bad base URL is refused instead of stored.
func TestUpstreamStorePersistsAndRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	first, err := newStore(dir)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	if err := first.set("http://127.0.0.1:17864/", "sk-persisted"); err != nil {
		t.Fatalf("set: %v", err)
	}

	second, err := newStore(dir)
	if err != nil {
		t.Fatalf("reload newStore: %v", err)
	}
	base, key, ok := second.get()
	if !ok || base != "http://127.0.0.1:17864" || key != "sk-persisted" {
		t.Fatalf("reloaded store = (%q, %q, %v)", base, key, ok)
	}

	// The on-disk bytes must not contain the key in the clear on Windows
	// (DPAPI). Off Windows the fallback is documented as plaintext 0600.
	if sealKind() == "dpapi-user" {
		raw, err := os.ReadFile(second.path)
		if err != nil {
			t.Fatalf("read store file: %v", err)
		}
		if strings.Contains(string(raw), "sk-persisted") {
			t.Fatalf("store file contains the api key in plaintext")
		}
	}

	for _, bad := range []string{"", "127.0.0.1:17864", "ftp://x"} {
		if err := second.set(bad, "k"); err == nil {
			t.Fatalf("set(%q) succeeded, want an error", bad)
		}
	}
}
