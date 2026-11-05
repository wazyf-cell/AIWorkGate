package opchost

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"aiworkgate/internal/model"
)

func TestCheckinPath(t *testing.T) {
	cases := map[string]string{
		"acc-1": "/accounts/acc-1/checkin",
		"a/b":   "/accounts/a%2Fb/checkin",
		"":      "/accounts//checkin",
	}
	for accountID, want := range cases {
		if got := CheckinPath(accountID); got != want {
			t.Fatalf("CheckinPath(%q) = %q, want %q", accountID, got, want)
		}
	}
}

// TestContractTimeoutsMirrorTheHost pins the budget the bridge must answer
// within; the values are copied from internal/plugins/client.go and
// supervisor.go.
func TestContractTimeoutsMirrorTheHost(t *testing.T) {
	cases := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"health", HealthTimeout, 2 * time.Second},
		{"control", ControlTimeout, 60 * time.Second},
		{"checkin", CheckinTimeout, 5 * time.Minute},
		{"identity", IdentityTimeout, 20 * time.Second},
		{"response header", ResponseHeaderTimeout, 90 * time.Second},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Fatalf("%s timeout = %s, want %s", tc.name, tc.got, tc.want)
		}
	}
	if MaxResponseBytes != 8<<20 {
		t.Fatalf("max response bytes = %d, want %d", MaxResponseBytes, 8<<20)
	}
}

func TestMetaRoundTrip(t *testing.T) {
	in := Meta{
		ID:           "trae-cn",
		Name:         "Trae CN",
		Version:      "0.1.0",
		APILevel:     model.MaxAPILevel,
		Capabilities: []string{model.CapAccounts},
		AccountCount: 2,
		UptimeS:      42,
	}
	blob, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal Meta: %v", err)
	}
	for _, key := range []string{`"id"`, `"version"`, `"api_level"`, `"account_count"`, `"uptime_s"`} {
		if !strings.Contains(string(blob), key) {
			t.Fatalf("Meta JSON %s is missing %s", blob, key)
		}
	}
	var out Meta
	if err := json.Unmarshal(blob, &out); err != nil {
		t.Fatalf("unmarshal Meta: %v", err)
	}
	if out.ID != in.ID || out.APILevel != in.APILevel || out.AccountCount != in.AccountCount || out.UptimeS != in.UptimeS {
		t.Fatalf("Meta round trip = %+v, want %+v", out, in)
	}
}

// TestAccountsResponseUsesModelTypes proves the aliases really are the host's
// structs, so a bridge and the host cannot drift apart.
func TestAccountsResponseUsesModelTypes(t *testing.T) {
	payload := AccountsResponse{Accounts: []Account{{
		ID:      "acc-1",
		Label:   "first",
		Enabled: true,
		Status:  model.AccountOK,
	}}}
	blob, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal AccountsResponse: %v", err)
	}
	if !strings.Contains(string(blob), `"accounts"`) || !strings.Contains(string(blob), `"acc-1"`) {
		t.Fatalf("AccountsResponse JSON = %s, want the accounts envelope", blob)
	}
	var host model.AccountsResponse
	if err := json.Unmarshal(blob, &host); err != nil {
		t.Fatalf("host could not decode bridge output: %v", err)
	}
	if len(host.Accounts) != 1 || host.Accounts[0].ID != "acc-1" || host.Accounts[0].Status != model.AccountOK {
		t.Fatalf("host decoded %+v, want the same account", host.Accounts)
	}
}

func TestCheckinResultFields(t *testing.T) {
	before, after := 10.0, 20.0
	result := CheckinResult{
		Result:        model.CheckinOK,
		CreditsBefore: &before,
		CreditsAfter:  &after,
		Message:       "signed in",
		At:            1234,
		Kind:          model.StatusKindNormal,
		ResetAt:       5678,
		DedupScope:    model.DedupScopeDevice,
		DeviceKey:     "device-1",
	}
	blob, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal CheckinResult: %v", err)
	}
	for _, key := range []string{"result", "credits_before", "credits_after", "message", "at", "kind", "reset_at", "dedup_scope", "device_key"} {
		if !strings.Contains(string(blob), `"`+key+`"`) {
			t.Fatalf("CheckinResult JSON %s is missing %q", blob, key)
		}
	}
	var out CheckinResult
	if err := json.Unmarshal(blob, &out); err != nil {
		t.Fatalf("unmarshal CheckinResult: %v", err)
	}
	if out.Result != model.CheckinOK || out.CreditsBefore == nil || *out.CreditsBefore != before || out.DedupScope != model.DedupScopeDevice {
		t.Fatalf("CheckinResult round trip = %+v", out)
	}
	if got, want := CheckinPath("device-1"), "/accounts/device-1/checkin"; got != want {
		t.Fatalf("CheckinPath = %q, want %q", got, want)
	}
}

func TestHealthDecoding(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		wantOK *bool
		wantSt string
	}{
		{"healthy", `{"ok":true,"status":"ok"}`, boolPtr(true), "ok"},
		{"ok false", `{"ok":false}`, boolPtr(false), ""},
		{"empty body", `{}`, nil, ""},
		{"status only", `{"status":"degraded"}`, nil, "degraded"},
		{"extra fields ignored", `{"status":"ok","running":true,"nested":{"a":1}}`, nil, "ok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var h Health
			if err := json.Unmarshal([]byte(tc.body), &h); err != nil {
				t.Fatalf("decode %s: %v", tc.body, err)
			}
			switch {
			case tc.wantOK == nil && h.OK != nil:
				t.Fatalf("ok = %v, want nil", *h.OK)
			case tc.wantOK != nil && (h.OK == nil || *h.OK != *tc.wantOK):
				t.Fatalf("ok = %v, want %v", h.OK, *tc.wantOK)
			}
			if h.Status != tc.wantSt {
				t.Fatalf("status = %q, want %q", h.Status, tc.wantSt)
			}
		})
	}
}

func boolPtr(v bool) *bool { return &v }
