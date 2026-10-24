package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func forcedSetup(t *testing.T, maas *fakeMaaS) (*Plugin, http.Handler, fixtureSession) {
	t.Helper()
	s := defaultSession(maas.URL())
	root := makeInstall(t, s)
	p, h, _ := newTestPlugin(t, func(o *Options) { o.ForcedOfficeAceHome = installHome(t, root) })
	return p, h, s
}

func TestForcedMatchingAccountWorks(t *testing.T) {
	maas := newFakeMaaS(t, successSSE)
	p, h, s := forcedSetup(t, maas)
	id := accountIDFor(s.userKey())

	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{
		body:    sampleChatBody("glm-5.3", "hi", nil),
		headers: map[string]string{headerAccount: id, headerRequestID: "req-oa-1"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(headerAccount); got != id {
		t.Fatalf("%s = %q, want %q", headerAccount, got, id)
	}
	if reqs := maas.snapshot(); len(reqs) != 1 {
		t.Fatalf("upstream requests = %d, want 1", len(reqs))
	}
	if a, _ := p.findAccount(id); a.Status != statusOK {
		t.Fatalf("account = %+v", a)
	}
}

func TestForcedMismatchReturns409(t *testing.T) {
	maas := newFakeMaaS(t, successSSE)
	_, h, _ := forcedSetup(t, maas)

	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{
		body:    sampleChatBody("glm-5.3", "hi", nil),
		headers: map[string]string{headerAccount: "acc_not_this_one"},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body %s", rec.Code, rec.Body.String())
	}
	env := decodeAs[errorEnvelope](t, rec)
	if env.Error.Type != "account_unavailable" || env.Error.Code != "account_unavailable" {
		t.Fatalf("error = %+v", env.Error)
	}
	if reqs := maas.snapshot(); len(reqs) != 0 {
		t.Fatalf("upstream must not be called, requests = %d", len(reqs))
	}
}

func TestForcedDisabledAccountReturns409(t *testing.T) {
	maas := newFakeMaaS(t, successSSE)
	_, h, s := forcedSetup(t, maas)
	id := accountIDFor(s.userKey())
	if rec := doReq(t, h, http.MethodPatch, "/accounts/"+id, reqOpts{body: `{"enabled":false}`}); rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d", rec.Code)
	}

	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{
		body:    sampleChatBody("glm-5.3", "hi", nil),
		headers: map[string]string{headerAccount: id},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body %s", rec.Code, rec.Body.String())
	}
	if reqs := maas.snapshot(); len(reqs) != 0 {
		t.Fatalf("upstream must not be called, requests = %d", len(reqs))
	}
}

func TestForcedAccountSSECarriesHeader(t *testing.T) {
	maas := newFakeMaaS(t, successSSE)
	_, h, s := forcedSetup(t, maas)
	id := accountIDFor(s.userKey())

	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{
		body:    sampleChatBody("glm-5.3", "hi", map[string]any{"stream": true}),
		headers: map[string]string{headerAccount: id},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(headerAccount); got != id {
		t.Fatalf("%s = %q, want %q", headerAccount, got, id)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("SSE body truncated: %s", body)
	}
}

func TestHedgeNeverRefreshes(t *testing.T) {
	maas := newFakeMaaS(t, func(w http.ResponseWriter, r *http.Request, body string) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"expired"}`))
	})
	s := defaultSession(maas.URL())
	s.STSExpiresAt = fixedNow.Add(-time.Hour).Format(time.RFC3339)
	s.SessionExpiresAt = s.STSExpiresAt
	root := makeInstall(t, s)
	iam := newFakeIAM(t, "sts-renewed")
	// Auto refresh is enabled globally: the hedge flag alone must stop it.
	p, _, _ := newTestPlugin(t, func(o *Options) {
		o.ForcedOfficeAceHome = installHome(t, root)
		o.IAMURL = iam.URL()
	})
	p.cfgMu.Lock()
	p.cfg.AutoRefresh = true
	p.cfgMu.Unlock()

	rec := doReq(t, p.Handler(), http.MethodPost, "/v1/chat/completions", reqOpts{
		body:    sampleChatBody("glm-5.3", "hi", nil),
		headers: map[string]string{headerAccount: accountIDFor(s.userKey()), headerHedge: "1"},
	})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body %s", rec.Code, rec.Body.String())
	}
	if calls := iam.callCount(); calls != 0 {
		t.Fatalf("IAM calls = %d, want 0 for a hedged request", calls)
	}
}

func TestAccountsExposeV03Fields(t *testing.T) {
	s := defaultSession("maas.example.test")
	root := makeInstall(t, s)
	local := newFakeLocal(t)
	p, h, _ := newTestPlugin(t, func(o *Options) {
		o.ForcedOfficeAceHome = installHome(t, root)
		o.LocalAPIURL = local.URL()
	})

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
	if len(env.Accounts) != 1 {
		t.Fatalf("accounts = %d", len(env.Accounts))
	}
	a := env.Accounts[0]
	quota, _ := a["quota"].(map[string]any)
	if quota["plan"] != "Pro" {
		t.Fatalf("quota.plan = %v (quota %v)", quota["plan"], quota)
	}
	cred, _ := a["credential"].(map[string]any)
	if cred == nil || cred["present"] != true || cred["refreshable"] != true {
		t.Fatalf("credential = %v", cred)
	}
	wantExp := fixedNow.Add(time.Hour).Unix()
	if exp, ok := cred["expires_at"].(float64); !ok || int64(exp) != wantExp {
		t.Fatalf("credential.expires_at = %v, want %d", cred["expires_at"], wantExp)
	}
	if _, present := a["cooldown_until"]; present {
		t.Fatalf("cooldown_until must be omitted: %v", a["cooldown_until"])
	}
	_ = p
}

func TestRateLimitSetsAccountStatusHeader(t *testing.T) {
	maas := newFakeMaaS(t, func(w http.ResponseWriter, r *http.Request, body string) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"slow down"}`))
	})
	_, h, s := forcedSetup(t, maas)
	id := accountIDFor(s.userKey())

	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{
		body:    sampleChatBody("glm-5.3", "hi", nil),
		headers: map[string]string{headerAccount: id},
	})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(headerAccount); got != id {
		t.Fatalf("%s = %q", headerAccount, got)
	}
	if got := rec.Header().Get(headerAccountStatus); got != "rate_limited" {
		t.Fatalf("%s = %q", headerAccountStatus, got)
	}
	if got := rec.Header().Get(headerCredits); got != "" {
		t.Fatalf("%s must be omitted when credits are unknown: %q", headerCredits, got)
	}
}
