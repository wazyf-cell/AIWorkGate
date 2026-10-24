package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func unlimitedSubscriptionJSON(expires time.Time) string {
	return fmt.Sprintf(`{"planId":"p1","planName":"Unlimited","tier":"standard","status":"active",`+
		`"expiresAt":%q,"usedCredits":123,"totalCredits":0,"unlimitedCredits":true}`,
		expires.Format(time.RFC3339))
}

// TestUnlimitedQuotaHasNoNegativeBalance: an unlimited plan must not be
// serialized as remaining=-1/total=-1. Those sentinels do not exist in the host
// contract and the host's absolute 10%/100-credit threshold reads them as a
// depleted account.
func TestUnlimitedQuotaHasNoNegativeBalance(t *testing.T) {
	s := defaultSession("maas.example.test")
	root := makeInstall(t, s)
	local := newFakeLocal(t)
	local.onSub = func(int) (int, string) {
		return http.StatusOK, unlimitedSubscriptionJSON(fixedNow.Add(30 * 24 * time.Hour))
	}
	_, h, _ := newTestPlugin(t, func(o *Options) {
		o.ForcedOfficeAceHome = installHome(t, root)
		o.LocalAPIURL = local.URL()
	})

	rec := doReq(t, h, http.MethodGet, "/accounts", reqOpts{})
	if rec.Code != http.StatusOK {
		t.Fatalf("accounts status = %d; body %s", rec.Code, rec.Body.String())
	}
	var raw struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	quota, _ := raw.Accounts[0]["quota"].(map[string]any)
	if quota == nil {
		t.Fatalf("quota missing: %s", rec.Body.String())
	}
	if quota["unlimited"] != true {
		t.Fatalf("quota.unlimited = %v, want true: %v", quota["unlimited"], quota)
	}
	for _, key := range []string{"remaining", "total"} {
		if v, ok := quota[key]; ok {
			t.Fatalf("unlimited quota must omit %q, got %v: %v", key, v, quota)
		}
	}

	typed := listAccounts(t, h).Accounts
	if len(typed) != 1 || typed[0].Quota == nil {
		t.Fatalf("accounts = %+v", typed)
	}
	if !typed[0].Quota.Unlimited || typed[0].Quota.Remaining != 0 || typed[0].Quota.Total != 0 {
		t.Fatalf("unlimited quota = %+v", typed[0].Quota)
	}
	if typed[0].Quota.Plan != "Unlimited" {
		t.Fatalf("plan = %q", typed[0].Quota.Plan)
	}
}
