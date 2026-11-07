package scheduler

// D-24 冷却按原因分级与 D-45 冷却结构化 + 手动解除的测试锁定。全部使用注入
// 时钟（Options.Now + clock），不真实 sleep。

import (
	"testing"
	"time"
)

// TestCooldownTiersByCause locks the D-24 tier table: every classified cause
// parks the account for its documented duration, unclassified failures keep
// parking nothing, and the pool circuit keeps its pre-D-24 condition.
func TestCooldownTiersByCause(t *testing.T) {
	clk := &clock{at: testBase}
	s := New(Options{Now: clk.now})
	key := AccountKey{PluginID: "p", AccountID: "a"}

	// Account-scoped tiers via plain Report (status-code attribution only).
	tiers := []struct {
		name   string
		status int
		want   int64
	}{
		{"401 auth_expired", 401, cooldownAuthSeconds},
		{"503 upstream_error", 503, cooldownServerErr},
		{"408 upstream_error", 408, cooldownServerErr},
		{"409 upstream_error", 409, cooldownServerErr},
		{"0 network", 0, cooldownNetSeconds},
	}
	for _, tc := range tiers {
		s.Report(key, "m1", tc.status, 0, 0)
		if got := s.accountCD[key]; got != clk.at+tc.want {
			t.Fatalf("%s: account cooldown until %d, want %d (+%ds)", tc.name, got, clk.at+tc.want, tc.want)
		}
	}

	// Unclassified failures keep the legacy no-cooldown behavior.
	s.Report(AccountKey{PluginID: "p", AccountID: "plain"}, "m1", 400, 0, 0)
	if got := s.accountCD[AccountKey{PluginID: "p", AccountID: "plain"}]; got != 0 {
		t.Fatalf("unclassified 400 parked the account until %d, want no cooldown", got)
	}

	// Message-driven tiers via ReportCause: quota text outranks the 429 that
	// the gateway remaps credit failures to, so the account tier (not the 429
	// doubling ladder) applies.
	s.ReportCause(key, "m1", FailureCause{Status: 429, Message: "insufficient quota"}, 0, 0)
	if got := s.accountCD[key]; got != clk.at+cooldownQuotaSeconds {
		t.Fatalf("quota text: cooldown until %d, want +%d", got, cooldownQuotaSeconds)
	}
	if _, ok := s.modelCD[modelCooldownKey{Key: key, Model: "m1"}]; ok {
		t.Fatal("quota exhaustion must park the account, not the model pair")
	}

	// Egress policy rejections park for 5 minutes regardless of status.
	s.ReportCause(key, "m1", FailureCause{Status: 403, Message: "EGRESS_POLICY_DENIED"}, 0, 0)
	if got := s.accountCD[key]; got != clk.at+cooldownEgressSeconds {
		t.Fatalf("egress: cooldown until %d, want +%d", got, cooldownEgressSeconds)
	}

	// quota_exhausted cools until the window reset, capped at 30 minutes.
	s.ReportCause(AccountKey{PluginID: "p", AccountID: "reset"}, "m1",
		FailureCause{Status: 402, Message: "额度不足", ResetAt: clk.at + 600}, 0, 0)
	if got := s.accountCD[AccountKey{PluginID: "p", AccountID: "reset"}]; got != clk.at+600 {
		t.Fatalf("quota with sooner reset: cooldown until %d, want +%d", got, 600)
	}
	s.ReportCause(AccountKey{PluginID: "p", AccountID: "reset2"}, "m1",
		FailureCause{Status: 402, Message: "credits used up", ResetAt: clk.at + 90*60}, 0, 0)
	if got := s.accountCD[AccountKey{PluginID: "p", AccountID: "reset2"}]; got != clk.at+cooldownQuotaSeconds {
		t.Fatalf("quota with later reset: cooldown until %d, want the %ds cap", got, cooldownQuotaSeconds)
	}
	s.ReportCause(AccountKey{PluginID: "p", AccountID: "reset3"}, "m1",
		FailureCause{Status: 402, Message: "余额不足", ResetAt: clk.at - 5}, 0, 0)
	if got := s.accountCD[AccountKey{PluginID: "p", AccountID: "reset3"}]; got != clk.at+cooldownQuotaSeconds {
		t.Fatalf("quota with past reset: cooldown until %d, want the %ds floor", got, cooldownQuotaSeconds)
	}

	// other: a message-bearing failure that matches no specific tier.
	s.ReportCause(AccountKey{PluginID: "p", AccountID: "other"}, "m1",
		FailureCause{Status: 418, Message: "teapot"}, 0, 0)
	if got := s.accountCD[AccountKey{PluginID: "p", AccountID: "other"}]; got != clk.at+cooldownOtherSeconds {
		t.Fatalf("other: cooldown until %d, want +%d", got, cooldownOtherSeconds)
	}

	// The circuit breaker keeps its pre-D-24 condition: only 5xx/0/408 streaks
	// open it; a burst of 401s parks accounts without blaming the pool.
	for i := 0; i < circuitThreshold; i++ {
		s.Report(AccountKey{PluginID: "p", AccountID: "auth" + string(rune('a'+i))}, "m1", 401, 0, 0)
	}
	if s.CircuitOpen("p") {
		t.Fatal("401 burst opened the pool circuit; only 5xx/0/408 may")
	}
	s.pool5xx["p"] = 0
}

// TestRateLimitKeepsBackoffAndRespectsRetryAfter locks the rate_limit tier:
// the pre-D-24 30s doubling capped at 10 minutes stays, and an upstream
// Retry-After hint overrides it with a 1-hour trust cap.
func TestRateLimitKeepsBackoffAndRespectsRetryAfter(t *testing.T) {
	clk := &clock{at: testBase}
	s := New(Options{Now: clk.now})
	key := AccountKey{PluginID: "p", AccountID: "a"}

	// Headless 429s keep doubling: 30s → 60s → ... → 600s cap.
	s.Report(key, "m1", 429, 0, 0)
	if state := s.modelCD[modelCooldownKey{Key: key, Model: "m1"}]; state.Until != clk.at+30 {
		t.Fatalf("first 429 until = %d, want +30", state.Until)
	}
	clk.advance(31)
	s.Report(key, "m1", 429, 0, 0)
	if state := s.modelCD[modelCooldownKey{Key: key, Model: "m1"}]; state.Until != clk.at+60 {
		t.Fatalf("second 429 until = %d, want +60", state.Until)
	}
	for i := 0; i < 10; i++ {
		s.Report(key, "m1", 429, 0, 0)
	}
	if state := s.modelCD[modelCooldownKey{Key: key, Model: "m1"}]; state.Until != clk.at+cooldown429Max {
		t.Fatalf("capped 429 until = %d, want +%d", state.Until, cooldown429Max)
	}

	// A Retry-After hint overrides the ladder and is capped at 1 hour.
	other := AccountKey{PluginID: "p", AccountID: "b"}
	s.ReportCause(other, "m1", FailureCause{Status: 429, RetryAfter: 90 * time.Second}, 0, 0)
	if state := s.modelCD[modelCooldownKey{Key: other, Model: "m1"}]; state.Until != clk.at+90 {
		t.Fatalf("429 with Retry-After until = %d, want +90", state.Until)
	}
	third := AccountKey{PluginID: "p", AccountID: "c"}
	s.ReportCause(third, "m1", FailureCause{Status: 429, RetryAfter: 5 * time.Hour}, 0, 0)
	if state := s.modelCD[modelCooldownKey{Key: third, Model: "m1"}]; state.Until != clk.at+cooldownRetryCapSeconds {
		t.Fatalf("429 with oversized Retry-After until = %d, want the %ds cap", state.Until, cooldownRetryCapSeconds)
	}
}

// TestCooldownRecordsCarryWhyAndBy locks the D-45 structured fields on the
// persisted records (the store adapter folds Why into the reason text).
func TestCooldownRecordsCarryWhyAndBy(t *testing.T) {
	clk := &clock{at: testBase}
	store := newFakeCooldowns()
	s := New(Options{Now: clk.now, Cooldowns: store})
	defer s.Close()
	key := AccountKey{PluginID: "p", AccountID: "a"}

	s.Report(key, "m1", 429, 0, 0)
	s.Report(key, "", 500, 0, 0)
	s.ReportCause(AccountKey{PluginID: "p", AccountID: "b"}, "m1",
		FailureCause{Status: 402, Message: "额度不足"}, 0, 0)
	s.FlushCooldowns()

	row, ok := store.row("p", "a", "m1")
	if !ok || row.Reason != "status 429" || row.Why != CooldownRateLimit || row.By != "p/a" {
		t.Fatalf("429 record = %+v", row)
	}
	row, ok = store.row("p", "a", "")
	if !ok || row.Reason != "status 500" || row.Why != CooldownUpstreamError || row.By != "p/a" {
		t.Fatalf("5xx record = %+v", row)
	}
	row, ok = store.row("p", "b", "")
	if !ok || row.Model != "" || row.Why != CooldownQuotaExhausted || row.By != "p/b" {
		t.Fatalf("quota record = %+v (present %v)", row, ok)
	}
}

// TestClearCooldownRestoresSelectionImmediately locks the D-45 manual clear:
// after ClearCooldown the account is selectable again at once and both the
// account-wide and the per-model store rows are deleted.
func TestClearCooldownRestoresSelectionImmediately(t *testing.T) {
	clk := &clock{at: testBase}
	store := newFakeCooldowns()
	s := New(Options{Now: clk.now, Cooldowns: store})
	a, b := account("a"), account("b")
	p := pool("p", a, b)
	snap := snapOf(p)
	key := AccountKey{PluginID: "p", AccountID: "a"}

	s.Report(key, "m1", 429, 0, 0)
	s.Report(key, "", 500, 0, 0)
	if got := pick(t, s, snap, "p/m1"); got.AccountID != "b" {
		t.Fatalf("pick while cooling = %s, want b", got.AccountID)
	}

	s.ClearCooldown("p", "a")
	// Clearing again is a silent no-op.
	s.ClearCooldown("p", "ghost")
	if got := pick(t, s, snap, "p/m1"); got.AccountID != "a" {
		t.Fatalf("pick after clear = %s, want a (immediately re-selectable)", got.AccountID)
	}
	s.FlushCooldowns()
	if _, ok := store.row("p", "a", "m1"); ok {
		t.Fatal("per-model cooldown row survived the manual clear")
	}
	if _, ok := store.row("p", "a", ""); ok {
		t.Fatal("account-wide cooldown row survived the manual clear")
	}
	if store.deletes == 0 {
		t.Fatal("the clear produced no store deletes")
	}

	// An unknown account never regresses the others.
	s.Report(AccountKey{PluginID: "p", AccountID: "b"}, "m1", 429, 0, 0)
	s.ClearCooldown("p", "ghost")
	if _, ok := s.modelCD[modelCooldownKey{Key: AccountKey{PluginID: "p", AccountID: "b"}, Model: "m1"}]; !ok {
		t.Fatal("clearing an unknown account dropped another account's cooldown")
	}
}
