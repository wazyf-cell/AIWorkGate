package app

import (
	"testing"

	"aiworkgate/internal/egress"
	"aiworkgate/internal/model"
)

// TestAccountStatePrecedence pins the merged account verdict: plugin credential
// failures outrank host cooldowns, host cooldowns outrank the plugin's own
// rate-limit hint, and an expired cooldown no longer colours the row.
func TestAccountStatePrecedence(t *testing.T) {
	now := int64(1_700_000_000)
	cases := []struct {
		name       string
		status     string
		hostKind   string
		until      int64
		wantState  string
		wantSource string
	}{
		{"healthy", model.AccountOK, model.StatusKindNormal, 0, model.StatusKindNormal, ""},
		{"plugin expired beats host cooldown", model.AccountExpired, model.StatusKindRateLimited, now + 60, model.AccountExpired, "plugin"},
		{"plugin invalid", model.AccountInvalid, "", 0, model.AccountInvalid, "plugin"},
		{"active host cooldown", model.AccountOK, model.StatusKindRateLimited, now + 60, model.StatusKindRateLimited, "host"},
		{"expired host cooldown ignored", model.AccountOK, model.StatusKindSessionDead, now - 1, model.StatusKindNormal, ""},
		{"plugin rate limit without cooldown", model.AccountRateLimited, model.StatusKindNormal, 0, model.AccountRateLimited, "plugin"},
		{"unknown", model.AccountUnknown, "", 0, model.AccountUnknown, "plugin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acc := model.Account{ID: "a", Enabled: true, Status: tc.status}
			state, source, _ := accountState(acc, tc.hostKind, tc.until, "cooldown reason", now)
			if state != tc.wantState || source != tc.wantSource {
				t.Fatalf("state/source = %s/%s, want %s/%s", state, source, tc.wantState, tc.wantSource)
			}
		})
	}
}

// TestMergeAccountEntriesAttachesEgressHealth: an account with an explicit proxy
// assignment carries the verdict and its age; "follow default" accounts do not.
func TestMergeAccountEntriesAttachesEgressHealth(t *testing.T) {
	now := int64(1_700_000_000)
	accounts := []model.Account{
		{ID: "proxied", Enabled: true, Status: model.AccountOK},
		{ID: "plain", Enabled: true, Status: model.AccountOK},
	}
	entries := mergeAccountEntries("p", accounts, accountMergeOptions{
		Now: now,
		Health: func(pluginID, accountID string) (egress.Evidence, bool) {
			if accountID != "proxied" {
				return egress.Evidence{}, false
			}
			return egress.Evidence{
				Mode: model.EgressProxy, Status: egress.HealthFailed,
				ProxyID: "px", ProxyLabel: "公司代理", At: now - 90, Error: "timeout",
			}, true
		},
	})
	if len(entries) != 2 {
		t.Fatalf("entries = %d", len(entries))
	}
	if entries[0].State != model.StatusKindNormal || entries[0].StateSource != "" {
		t.Fatalf("state = %s/%s, want normal", entries[0].State, entries[0].StateSource)
	}
	health := entries[0].EgressHealth
	if health == nil || health.Status != egress.HealthFailed || health.AgeSeconds != 90 {
		t.Fatalf("egress health = %+v", health)
	}
	if entries[1].EgressHealth != nil {
		t.Fatalf("plain account must not carry egress health: %+v", entries[1].EgressHealth)
	}
}
