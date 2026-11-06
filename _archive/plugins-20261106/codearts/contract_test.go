package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// accountUpstream records each upstream call and answers with a fixed handler.
type accountUpstream struct {
	mu     sync.Mutex
	auths  []string
	srv    *httptest.Server
	handle func(w http.ResponseWriter, r *http.Request)
}

func newAccountUpstream(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) *accountUpstream {
	t.Helper()
	u := &accountUpstream{handle: handle}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		u.mu.Lock()
		u.auths = append(u.auths, r.Header.Get("Authorization"))
		u.mu.Unlock()
		u.handle(w, r)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *accountUpstream) calls() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.auths...)
}

func (u *accountUpstream) hits() int { return len(u.calls()) }

func okSSE(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprint(w, sseLine(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "hi"}}}}))
	_, _ = fmt.Fprint(w, sseLine(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}}))
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}

func alwaysRateLimited(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write([]byte(`{"error_code":"InferHub.ModelArts.81111.429","error_msg":"TPM exceeded"}`))
}

func alwaysUnauthorized(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error_code":"APIG.0602","error_msg":"invalid token"}`))
}

// forcedAccountPlugin builds a plugin with a fake chat endpoint and two
// accounts (alice, bob) unless the caller seeds its own.
func forcedAccountPlugin(t *testing.T, upstream *accountUpstream, statsURL string) (*Plugin, http.Handler, Account, Account) {
	t.Helper()
	ep := testEndpoints()
	ep.Chat = upstream.srv.URL
	if statsURL != "" {
		ep.Statistics = statsURL
	}
	p, h := newTestPlugin(t, func(o *Options) { o.Endpoints = ep })
	alice := seedAccount(t, p, "alice")
	bob := seedAccount(t, p, "bob")
	return p, h, alice, bob
}

func TestForcedAccountIsUsedAndReported(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			upstream := newAccountUpstream(t, okSSE)
			_, h, alice, bob := forcedAccountPlugin(t, upstream, "")

			rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{
				body:    chatRequestBody(t, "GLM-5.2", stream, nil),
				headers: map[string]string{headerAccount: bob.ID, headerRequestID: "req-1"},
			})
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get(headerAccount); got != bob.ID {
				t.Fatalf("response %s = %q, want %q", headerAccount, got, bob.ID)
			}
			calls := upstream.calls()
			if len(calls) != 1 {
				t.Fatalf("upstream calls = %d, want 1", len(calls))
			}
			if !strings.Contains(calls[0], "Access=AK-bob,") {
				t.Fatalf("forced account not used: %q", calls[0])
			}
			if strings.Contains(calls[0], "AK-alice") {
				t.Fatalf("unexpected alice call: %q", calls[0])
			}
			_ = alice
		})
	}
}

func TestForcedUnknownAccountReturns409(t *testing.T) {
	upstream := newAccountUpstream(t, okSSE)
	_, h, _, _ := forcedAccountPlugin(t, upstream, "")

	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{
		body:    chatRequestBody(t, "GLM-5.2", false, nil),
		headers: map[string]string{headerAccount: "acc_missing"},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body %s", rec.Code, rec.Body.String())
	}
	env := decodeAs[errorEnvelope](t, rec)
	if env.Error.Type != "account_unavailable" || env.Error.Code != "account_unavailable" {
		t.Fatalf("error = %+v", env.Error)
	}
	if upstream.hits() != 0 {
		t.Fatalf("upstream must not be called, hits = %d", upstream.hits())
	}
}

func TestForcedDisabledOrCredentialLessAccountReturns409(t *testing.T) {
	upstream := newAccountUpstream(t, okSSE)
	p, h, alice, _ := forcedAccountPlugin(t, upstream, "")

	if rec := doReq(t, h, http.MethodPatch, "/accounts/"+alice.ID, reqOpts{body: `{"enabled":false}`}); rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d", rec.Code)
	}
	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{
		body:    chatRequestBody(t, "GLM-5.2", false, nil),
		headers: map[string]string{headerAccount: alice.ID},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("disabled status = %d, want 409, body %s", rec.Code, rec.Body.String())
	}
	if env := decodeAs[errorEnvelope](t, rec); env.Error.Code != "account_unavailable" {
		t.Fatalf("error = %+v", env.Error)
	}

	// Credential-less account (no usable token and nothing to refresh).
	now := p.now()
	bare := &Account{
		ID:       "acc_bare_test",
		Label:    "bare",
		Enabled:  true,
		AddedAt:  now.Unix(),
		Identity: newIdentity(now.Unix()),
	}
	p.mu.Lock()
	p.accounts = append(p.accounts, bare)
	p.mu.Unlock()
	rec = doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{
		body:    chatRequestBody(t, "GLM-5.2", false, nil),
		headers: map[string]string{headerAccount: bare.ID},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("credential-less status = %d, want 409, body %s", rec.Code, rec.Body.String())
	}
	if upstream.hits() != 0 {
		t.Fatalf("upstream must not be called, hits = %d", upstream.hits())
	}
}

func TestForcedAccountDoesNotRotateOnRateLimit(t *testing.T) {
	upstream := newAccountUpstream(t, alwaysRateLimited)
	_, h, alice, _ := forcedAccountPlugin(t, upstream, "")

	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{
		body:    chatRequestBody(t, "GLM-5.2", false, nil),
		headers: map[string]string{headerAccount: alice.ID},
	})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429, body %s", rec.Code, rec.Body.String())
	}
	if hits := upstream.hits(); hits != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no rotation)", hits)
	}
	if strings.Contains(upstream.calls()[0], "AK-bob") {
		t.Fatalf("rotated to bob: %q", upstream.calls()[0])
	}
	if got := rec.Header().Get(headerAccount); got != alice.ID {
		t.Fatalf("%s = %q, want %q", headerAccount, got, alice.ID)
	}
	if got := rec.Header().Get(headerAccountStatus); got != "rate_limited" {
		t.Fatalf("%s = %q", headerAccountStatus, got)
	}
	cd, err := strconv.Atoi(rec.Header().Get(headerCooldown))
	if err != nil || cd <= 0 {
		t.Fatalf("%s = %q, want positive seconds", headerCooldown, rec.Header().Get(headerCooldown))
	}
	if env := decodeAs[errorEnvelope](t, rec); env.Error.Code != "rate_limited" {
		t.Fatalf("error = %+v", env.Error)
	}
}

func TestForcedAccountQuotaFailureReturnsUpstreamError(t *testing.T) {
	upstream := newAccountUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error_code":"4291","error_msg":"insufficient quota"}`))
	})
	_, h, alice, _ := forcedAccountPlugin(t, upstream, "")

	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{
		body:    chatRequestBody(t, "GLM-5.2", false, nil),
		headers: map[string]string{headerAccount: alice.ID},
	})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429, body %s", rec.Code, rec.Body.String())
	}
	if hits := upstream.hits(); hits != 1 {
		t.Fatalf("upstream calls = %d, want 1", hits)
	}
	if !strings.Contains(decodeAs[errorEnvelope](t, rec).Error.Message, "insufficient quota") {
		t.Fatalf("upstream message lost: %s", rec.Body.String())
	}
}

func TestHedgeSuppressesRetriesAndRotation(t *testing.T) {
	cases := []struct {
		name    string
		headers func(alice Account) map[string]string
		want    int
	}{
		{"forced+hedge", func(a Account) map[string]string { return map[string]string{headerAccount: a.ID, headerHedge: "1"} }, http.StatusTooManyRequests},
		{"hedge-only", func(Account) map[string]string { return map[string]string{headerHedge: "1"} }, http.StatusTooManyRequests},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newAccountUpstream(t, alwaysRateLimited)
			_, h, alice, _ := forcedAccountPlugin(t, upstream, "")
			rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{
				body:    chatRequestBody(t, "GLM-5.2", false, nil),
				headers: tc.headers(alice),
			})
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d, body %s", rec.Code, tc.want, rec.Body.String())
			}
			if hits := upstream.hits(); hits != 1 {
				t.Fatalf("upstream calls = %d, want 1 (hedge must not retry)", hits)
			}
		})
	}
}

func TestHedgeSuppressesAuthRefreshRetry(t *testing.T) {
	upstream := newAccountUpstream(t, alwaysUnauthorized)
	_, h, alice, _ := forcedAccountPlugin(t, upstream, "")

	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{
		body:    chatRequestBody(t, "GLM-5.2", false, nil),
		headers: map[string]string{headerAccount: alice.ID, headerHedge: "1"},
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body %s", rec.Code, rec.Body.String())
	}
	if hits := upstream.hits(); hits != 1 {
		t.Fatalf("upstream calls = %d, want 1", hits)
	}
	if env := decodeAs[errorEnvelope](t, rec); env.Error.Code != "upstream_auth" {
		t.Fatalf("error = %+v", env.Error)
	}
}

func TestAccountsExposeV03Fields(t *testing.T) {
	stats := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"code": 0,
			"data": map[string]any{
				"package": map[string]any{
					"is_credit_package": true,
					"package_name_cn":   "Pro 月卡",
				},
				"metrics": []any{map[string]any{
					"name":                  "usageTotalPackageCredit",
					"package_credit_amount": 5000,
					"package_credit_remain": 1200,
				}},
			},
		})
	}))
	defer stats.Close()

	upstream := newAccountUpstream(t, okSSE)
	p, h, alice, _ := forcedAccountPlugin(t, upstream, stats.URL)

	rec := doReq(t, h, http.MethodGet, "/accounts", reqOpts{})
	if rec.Code != http.StatusOK {
		t.Fatalf("accounts status = %d, body %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Accounts) != 2 {
		t.Fatalf("accounts = %d", len(env.Accounts))
	}
	var view map[string]any
	for _, a := range env.Accounts {
		if a["id"] == alice.ID {
			view = a
		}
	}
	if view == nil {
		t.Fatal("alice not found")
	}
	quota, _ := view["quota"].(map[string]any)
	if quota["plan"] != "Pro 月卡" {
		t.Fatalf("quota.plan = %v", quota["plan"])
	}
	if quota["remaining"] != 1200.0 || quota["total"] != 5000.0 {
		t.Fatalf("quota = %v", quota)
	}
	cred, _ := view["credential"].(map[string]any)
	if cred == nil || cred["present"] != true || cred["refreshable"] != true {
		t.Fatalf("credential = %v", cred)
	}
	exp, ok := cred["expires_at"].(float64)
	stored, _ := p.findAccount(alice.ID)
	wantExp := stored.Credential.expiry().Unix()
	if !ok || int64(exp) != wantExp {
		t.Fatalf("credential.expires_at = %v, want %d", cred["expires_at"], wantExp)
	}
	if _, present := view["cooldown_until"]; present {
		t.Fatalf("cooldown_until must be omitted when not cooling: %v", view["cooldown_until"])
	}
	if _, present := view["model_cooldowns"]; present {
		t.Fatal("model_cooldowns is not tracked by this plugin")
	}
}

func TestAccountsReportCooldownAfterRateLimit(t *testing.T) {
	upstream := newAccountUpstream(t, alwaysRateLimited)
	_, h, alice, _ := forcedAccountPlugin(t, upstream, "")

	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{
		body:    chatRequestBody(t, "GLM-5.2", false, nil),
		headers: map[string]string{headerAccount: alice.ID},
	})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("chat status = %d", rec.Code)
	}
	env := decodeAs[accountsEnvelope](t, doReq(t, h, http.MethodGet, "/accounts", reqOpts{}))
	var view accountView
	for _, a := range env.Accounts {
		if a.ID == alice.ID {
			view = a
		}
	}
	if view.CooldownUntil == 0 || view.Quota.ResetAt != view.CooldownUntil {
		t.Fatalf("cooldown_until = %d, reset_at = %d", view.CooldownUntil, view.Quota.ResetAt)
	}
}
