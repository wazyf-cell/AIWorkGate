package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestClassifyLocalFailureTable pins the attribution table of the optional
// check-in contract fields (plan 2026-10-21 §4). Only unambiguous causes may
// carry a kind; "OfficeAce is not running" explicitly must not look like a dead
// session.
func TestClassifyLocalFailureTable(t *testing.T) {
	p := &Plugin{now: func() time.Time { return fixedNow }}
	cases := []struct {
		name    string
		err     error
		want    string
		resetAt int64
	}{
		{"401 is a dead session", &localHTTPError{Status: http.StatusUnauthorized, Message: "expired"}, kindSessionDead, 0},
		{"403 is a dead session", &localHTTPError{Status: http.StatusForbidden, Message: "forbidden"}, kindSessionDead, 0},
		{"429 is rate limited", &localHTTPError{Status: http.StatusTooManyRequests, Message: "slow down", RetryAfter: 120},
			kindRateLimited, fixedNow.Unix() + 120},
		{"429 without Retry-After has no reset", &localHTTPError{Status: http.StatusTooManyRequests}, kindRateLimited, 0},
		{"402 means out of credits", &localHTTPError{Status: http.StatusPaymentRequired}, kindQuota, 0},
		{"business error text carries quota", &localHTTPError{Status: 200, Message: "额度不足"}, kindQuota, 0},
		{"english quota text", &localHTTPError{Status: 500, Message: "insufficient quota"}, kindQuota, 0},
		{"generic 500 stays unattributed", &localHTTPError{Status: 500, Message: "boom"}, "", 0},
		{"service down is NOT session_dead", &localHTTPError{Status: 0, Message: "connection refused"}, "", 0},
		{"plain error with quota text", errString("余额不足"), kindQuota, 0},
		{"plain error without hint", errString("network timeout"), "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, resetAt := p.classifyLocalFailure(tc.err)
			if kind != tc.want || resetAt != tc.resetAt {
				t.Fatalf("classifyLocalFailure = (%q, %d), want (%q, %d)", kind, resetAt, tc.want, tc.resetAt)
			}
		})
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestCheckinReportsSessionDeadOnLocal401(t *testing.T) {
	s := defaultSession("maas.example.test")
	root := makeInstall(t, s)
	local := newFakeLocal(t)
	local.setSubStatus(http.StatusUnauthorized)
	local.setSubscription(5000, 1200, fixedNow.Add(30*24*time.Hour))
	_, h, _ := newTestPlugin(t, func(o *Options) {
		o.ForcedOfficeAceHome = installHome(t, root)
		o.LocalAPIURL = local.URL()
	})
	id := accountIDFor(s.userKey())

	rec := doReq(t, h, http.MethodPost, "/accounts/"+id+"/checkin", reqOpts{})
	if rec.Code != http.StatusOK {
		t.Fatalf("checkin status = %d body %s", rec.Code, rec.Body.String())
	}
	res := decodeAs[checkinResult](t, rec)
	if res.Result != "fail" || res.Kind != kindSessionDead {
		t.Fatalf("checkin = %+v, want fail with kind session_dead", res)
	}
	if !strings.Contains(res.Message, "登录态已失效") {
		t.Fatalf("message = %q", res.Message)
	}
}

func TestCheckinServiceDownHasNoKind(t *testing.T) {
	s := defaultSession("maas.example.test")
	root := makeInstall(t, s)
	// The default LocalAPIURL points at a closed port, so the local app is down.
	_, h, _ := newTestPlugin(t, func(o *Options) {
		o.ForcedOfficeAceHome = installHome(t, root)
	})
	id := accountIDFor(s.userKey())

	rec := doReq(t, h, http.MethodPost, "/accounts/"+id+"/checkin", reqOpts{})
	res := decodeAs[checkinResult](t, rec)
	if res.Result != "fail" || res.Kind != "" {
		t.Fatalf("checkin = %+v, want fail with no kind", res)
	}
	if !strings.Contains(res.Message, "OfficeAce 未运行") {
		t.Fatalf("message = %q", res.Message)
	}
}

func TestCheckinClaimQuotaText(t *testing.T) {
	s := defaultSession("maas.example.test")
	root := makeInstall(t, s)
	local := newFakeLocal(t)
	local.setClaim(http.StatusOK, `{"error":"额度不足"}`)
	_, h, _ := newTestPlugin(t, func(o *Options) {
		o.ForcedOfficeAceHome = installHome(t, root)
		o.LocalAPIURL = local.URL()
	})
	id := accountIDFor(s.userKey())

	rec := doReq(t, h, http.MethodPost, "/accounts/"+id+"/checkin", reqOpts{})
	res := decodeAs[checkinResult](t, rec)
	if res.Result != "fail" || res.Kind != kindQuota {
		t.Fatalf("checkin = %+v, want fail with kind quota", res)
	}
}
