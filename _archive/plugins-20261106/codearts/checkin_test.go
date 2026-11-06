package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type checkinServer struct {
	mu        sync.Mutex
	claimable bool
	status    string
	statsFail bool
	claimFail bool
	claims    []map[string]any
	confirms  []map[string]any
	statsHits int32
}

func (s *checkinServer) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/statistics", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&s.statsHits, 1)
		if s.statsFail {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"code": 0,
			"data": map[string]any{
				"package": map[string]any{"is_credit_package": true, "package_name_cn": "基础版"},
				"metrics": []any{
					map[string]any{"name": "usageTotalPackageCredit", "package_credit_amount": 5000, "package_credit_remain": 1200, "package_credit_used": 3800},
					map[string]any{"name": "usageBasicPackageCredit", "package_credit_amount": 5000, "package_credit_remain": 1200},
				},
			},
		})
	})
	mux.HandleFunc("/delivery", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("channel") != "IDE" {
			t.Errorf("delivery channel = %q", r.URL.Query().Get("channel"))
		}
		s.mu.Lock()
		claimable, status := s.claimable, s.status
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"code": 0,
			"data": map[string]any{"items": []any{map[string]any{
				"campaignId": 1, "type": "USER_LOGIN", "title": "每日登录",
				"claimable": claimable, "status": status, "benefitAmount": 100,
			}}},
		})
	})
	mux.HandleFunc("/claim", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.claims = append(s.claims, body)
		fail := s.claimFail
		s.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":400,"message":"already claimed"}`))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"id": "benefit-1", "benefitAmount": 100}})
	})
	mux.HandleFunc("/confirm", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.confirms = append(s.confirms, body)
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}})
	})
	return mux
}

func checkinEndpoints(srv *httptest.Server) Endpoints {
	ep := testEndpoints()
	ep.Statistics = srv.URL + "/statistics"
	ep.Delivery = srv.URL + "/delivery"
	ep.Claim = srv.URL + "/claim"
	ep.Confirm = srv.URL + "/confirm"
	return ep
}

func TestCheckinClaimFlow(t *testing.T) {
	cs := &checkinServer{claimable: true, status: "AVAILABLE"}
	srv := httptest.NewServer(cs.handler(t))
	defer srv.Close()
	ep := checkinEndpoints(srv)
	p, h := newTestPlugin(t, func(o *Options) { o.Endpoints = ep })
	acct := seedAccount(t, p, "alice")

	rec := doReq(t, h, http.MethodPost, "/accounts/"+acct.ID+"/checkin", reqOpts{})
	if rec.Code != http.StatusOK {
		t.Fatalf("checkin status = %d, body %s", rec.Code, rec.Body.String())
	}
	res := decodeAs[checkinResult](t, rec)
	if res.Result != "ok" {
		t.Fatalf("result = %+v", res)
	}
	if res.CreditsBefore == nil || res.CreditsAfter == nil || *res.CreditsBefore != 1200 || *res.CreditsAfter != 1300 {
		t.Fatalf("credits = %+v", res)
	}
	if res.Message != "+100" || res.At == 0 {
		t.Fatalf("message/at = %+v", res)
	}
	cs.mu.Lock()
	claims := append([]map[string]any(nil), cs.claims...)
	confirms := append([]map[string]any(nil), cs.confirms...)
	cs.mu.Unlock()
	if len(claims) != 1 || claims[0]["channel"] != "IDE" {
		t.Fatalf("claims = %+v", claims)
	}
	if campaign, _ := claims[0]["campaignId"].(float64); campaign != 1 {
		t.Fatalf("claim campaignId = %v", claims[0]["campaignId"])
	}
	if len(confirms) != 1 {
		t.Fatalf("confirms = %+v", confirms)
	}
	if campaign, _ := confirms[0]["campaignId"].(float64); campaign != 1 {
		t.Fatalf("confirm campaignId = %v", confirms[0]["campaignId"])
	}

	views := listAccounts(t, h).Accounts
	if !views[0].Quota.TodayCheckedIn || views[0].Quota.CheckedInAt == 0 {
		t.Fatalf("quota after checkin = %+v", views[0].Quota)
	}
	if views[0].Quota.Remaining != 1300 {
		t.Fatalf("quota remaining = %v", views[0].Quota.Remaining)
	}
}

func TestCheckinAlreadyClaimed(t *testing.T) {
	cs := &checkinServer{claimable: false, status: "CLAIMED"}
	srv := httptest.NewServer(cs.handler(t))
	defer srv.Close()
	ep := checkinEndpoints(srv)
	p, h := newTestPlugin(t, func(o *Options) { o.Endpoints = ep })
	acct := seedAccount(t, p, "alice")

	rec := doReq(t, h, http.MethodPost, "/accounts/"+acct.ID+"/checkin", reqOpts{})
	res := decodeAs[checkinResult](t, rec)
	if res.Result != "already" || res.Message != "今天已领取" {
		t.Fatalf("result = %+v", res)
	}
	if res.CreditsBefore == nil || *res.CreditsBefore != 1200 {
		t.Fatalf("credits = %+v", res)
	}
	cs.mu.Lock()
	claims := len(cs.claims)
	cs.mu.Unlock()
	if claims != 0 {
		t.Fatalf("claim should not be called when not claimable: %+v", cs.claims)
	}
	if views := listAccounts(t, h).Accounts; !views[0].Quota.TodayCheckedIn {
		t.Fatalf("already claim should mark today_checked_in: %+v", views[0].Quota)
	}
}

func TestCheckinFailuresReportFail(t *testing.T) {
	t.Run("http error", func(t *testing.T) {
		cs := &checkinServer{claimable: true, status: "AVAILABLE", statsFail: true}
		srv := httptest.NewServer(cs.handler(t))
		defer srv.Close()
		ep := checkinEndpoints(srv)
		p, h := newTestPlugin(t, func(o *Options) { o.Endpoints = ep })
		acct := seedAccount(t, p, "alice")

		rec := doReq(t, h, http.MethodPost, "/accounts/"+acct.ID+"/checkin", reqOpts{})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		res := decodeAs[checkinResult](t, rec)
		if res.Result != "fail" || !strings.Contains(res.Message, "账户信息查询失败") {
			t.Fatalf("result = %+v", res)
		}
	})
	t.Run("claim error", func(t *testing.T) {
		cs := &checkinServer{claimable: true, status: "AVAILABLE", claimFail: true}
		srv := httptest.NewServer(cs.handler(t))
		defer srv.Close()
		ep := checkinEndpoints(srv)
		p, h := newTestPlugin(t, func(o *Options) { o.Endpoints = ep })
		acct := seedAccount(t, p, "alice")

		rec := doReq(t, h, http.MethodPost, "/accounts/"+acct.ID+"/checkin", reqOpts{})
		res := decodeAs[checkinResult](t, rec)
		if res.Result != "fail" || !strings.Contains(res.Message, "already claimed") {
			t.Fatalf("result = %+v", res)
		}
	})
}

func TestAccountsQuotaCaching(t *testing.T) {
	cs := &checkinServer{claimable: false, status: "AVAILABLE"}
	srv := httptest.NewServer(cs.handler(t))
	defer srv.Close()
	ep := checkinEndpoints(srv)
	clock := newFakeClock()
	p, h := newTestPlugin(t, func(o *Options) {
		o.Endpoints = ep
		o.Now = clock.Now
	})
	seedAccount(t, p, "alice")

	views := listAccounts(t, h).Accounts
	if views[0].Quota.Remaining != 1200 || views[0].Quota.Total != 5000 {
		t.Fatalf("quota = %+v", views[0].Quota)
	}
	hitsAfterFirst := atomic.LoadInt32(&cs.statsHits)
	if hitsAfterFirst != 1 {
		t.Fatalf("stats hits = %d, want 1", hitsAfterFirst)
	}
	listAccounts(t, h)
	if got := atomic.LoadInt32(&cs.statsHits); got != hitsAfterFirst {
		t.Fatalf("stats refetched within TTL: %d", got)
	}
	clock.Advance(61 * time.Second)
	listAccounts(t, h)
	if got := atomic.LoadInt32(&cs.statsHits); got != hitsAfterFirst+1 {
		t.Fatalf("stats hits after TTL = %d, want %d", got, hitsAfterFirst+1)
	}
}

func TestRefreshEndpoint(t *testing.T) {
	rec := &stsRecorder{expiryDelta: 2 * time.Hour}
	clock := newFakeClock()
	sts := httptest.NewServer(rec.handler(t, clock.Now))
	defer sts.Close()
	ep := testEndpoints()
	ep.STS = sts.URL
	p, h := newTestPlugin(t, func(o *Options) {
		o.Endpoints = ep
		o.Now = clock.Now
	})
	acct := seedAccount(t, p, "alice")
	p.mu.Lock()
	if a := p.accountByIDLocked(acct.ID); a != nil {
		a.Credential.RefreshToken = "R1"
		a.Credential.CodeVerifier = "VERIFIER-1"
		a.Credential.ExpiresAt = clock.Now().Add(30 * time.Minute).Format(time.RFC3339)
	}
	p.mu.Unlock()
	if err := p.saveAccounts(); err != nil {
		t.Fatalf("save: %v", err)
	}
	rec.mu.Lock()
	rec.current = "R1"
	rec.next = 2
	rec.mu.Unlock()

	httpResp := doReq(t, h, http.MethodPost, "/accounts/"+acct.ID+"/refresh", reqOpts{})
	if httpResp.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, body %s", httpResp.Code, httpResp.Body.String())
	}
	res := decodeAs[refreshResult](t, httpResp)
	if res.Result != "ok" || res.ExpiresAt == 0 {
		t.Fatalf("refresh = %+v", res)
	}
	stored, _ := p.findAccount(acct.ID)
	if stored.Credential.RefreshToken != "R2" || stored.Credential.AccessKeyID != "AK2" {
		t.Fatalf("refreshed credential = %+v", stored.Credential)
	}
}

// ---------------------------------------------------------------------------
// failure attribution (contract v0.4, plan 2026-10-21 §4)
// ---------------------------------------------------------------------------

func TestClassifySnapFailureTable(t *testing.T) {
	cases := []struct {
		name string
		res  snapResult
		want string
	}{
		{"401 is a dead session", snapResult{status: http.StatusUnauthorized, message: "HTTP 401: expired"}, kindSessionDead},
		{"403 is a dead session", snapResult{status: http.StatusForbidden, message: "HTTP 403"}, kindSessionDead},
		{"429 is rate limited", snapResult{status: http.StatusTooManyRequests, message: "HTTP 429"}, kindRateLimited},
		{"402 means out of credits", snapResult{status: http.StatusPaymentRequired}, kindQuota},
		{"business quota wording", snapResult{status: 200, message: "活动领取失败：额度不足"}, kindQuota},
		{"english quota wording", snapResult{status: 500, message: "insufficient credit"}, kindQuota},
		{"generic failure stays unattributed", snapResult{status: 500, message: "HTTP 500: boom"}, ""},
		{"unknown business code stays unattributed", snapResult{status: 200, message: "业务码 1005"}, ""},
		{"empty stays unattributed", snapResult{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifySnapFailure(tc.res); got != tc.want {
				t.Fatalf("classifySnapFailure(%+v) = %q, want %q", tc.res, got, tc.want)
			}
		})
	}
}

func TestRetryAfterReset(t *testing.T) {
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.Local)
	if got := retryAfterReset(now, "120"); got != now.Unix()+120 {
		t.Fatalf("delay-seconds = %d, want %d", got, now.Unix()+120)
	}
	date := now.Add(10 * time.Minute).UTC().Format(http.TimeFormat)
	if got := retryAfterReset(now, date); got != now.Unix()+600 {
		t.Fatalf("http-date = %d, want %d", got, now.Unix()+600)
	}
	for _, value := range []string{"", "0", "-5", "soon"} {
		if got := retryAfterReset(now, value); got != 0 {
			t.Fatalf("retryAfterReset(%q) = %d, want 0", value, got)
		}
	}
}

func TestSnapErrorKeepsMessageAndAttribution(t *testing.T) {
	err := snapError("账户信息查询失败", snapResult{
		status: http.StatusTooManyRequests, message: "HTTP 429: slow down", resetAt: 777,
	})
	if err.Error() != "账户信息查询失败：HTTP 429: slow down" {
		t.Fatalf("message = %q", err.Error())
	}
	kind, resetAt := checkinErrorAttribution(err)
	if kind != kindRateLimited || resetAt != 777 {
		t.Fatalf("attribution = (%q, %d)", kind, resetAt)
	}
	// A plain error still yields whatever its own text proves.
	kind, resetAt = checkinErrorAttribution(errString("余额不足"))
	if kind != kindQuota || resetAt != 0 {
		t.Fatalf("plain error attribution = (%q, %d)", kind, resetAt)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestCheckinStatistics429CarriesResetAt(t *testing.T) {
	clock := newFakeClock()
	mux := http.NewServeMux()
	mux.HandleFunc("/statistics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"code":429,"message":"too many requests"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	ep := testEndpoints()
	ep.Statistics = srv.URL + "/statistics"
	p, h := newTestPlugin(t, func(o *Options) {
		o.Endpoints = ep
		o.Now = clock.Now
	})
	acct := seedAccount(t, p, "alice")

	rec := doReq(t, h, http.MethodPost, "/accounts/"+acct.ID+"/checkin", reqOpts{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	res := decodeAs[checkinResult](t, rec)
	if res.Result != "fail" || res.Kind != kindRateLimited {
		t.Fatalf("result = %+v, want fail/rate_limited", res)
	}
	if res.ResetAt != clock.Now().Unix()+120 {
		t.Fatalf("reset_at = %d, want %d", res.ResetAt, clock.Now().Unix()+120)
	}
	if !strings.Contains(res.Message, "账户信息查询失败") {
		t.Fatalf("message = %q", res.Message)
	}
}

func TestCheckinStatistics401IsSessionDead(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/statistics", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":401,"message":"expired"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	ep := testEndpoints()
	ep.Statistics = srv.URL + "/statistics"
	p, h := newTestPlugin(t, func(o *Options) { o.Endpoints = ep })
	acct := seedAccount(t, p, "alice")

	rec := doReq(t, h, http.MethodPost, "/accounts/"+acct.ID+"/checkin", reqOpts{})
	res := decodeAs[checkinResult](t, rec)
	if res.Result != "fail" || res.Kind != kindSessionDead || res.ResetAt != 0 {
		t.Fatalf("result = %+v, want fail/session_dead", res)
	}
}
