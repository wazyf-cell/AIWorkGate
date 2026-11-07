package app

// D-45 冷却结构化 + 手动解除的宿主侧测试：调度器冷却的 Why 编解码适配与
// accountCooldownClear 的接线（清除后账号立即重新可选）。

import (
	"errors"
	"testing"
	"time"

	"aiworkgate/internal/logging"
	"aiworkgate/internal/scheduler"
	"aiworkgate/internal/webui"
)

// TestCooldownWhyRoundTrip locks the durable encoding of the D-45 reason class
// inside the reason column: new rows round-trip, legacy rows load unchanged.
func TestCooldownWhyRoundTrip(t *testing.T) {
	if got := encodeCooldownWhy(scheduler.CooldownRateLimit, "status 429"); got != "[rate_limit] status 429" {
		t.Fatalf("encode = %q", got)
	}
	if got := encodeCooldownWhy("", "status 500"); got != "status 500" {
		t.Fatalf("encode without why = %q", got)
	}
	why, rest := decodeCooldownWhy("[quota_exhausted] quota exhausted")
	if why != scheduler.CooldownQuotaExhausted || rest != "quota exhausted" {
		t.Fatalf("decode = %q/%q", why, rest)
	}
	// Legacy rows and degenerate markers must not invent a class.
	for _, reason := range []string{"status 429", "[broken", "[] x", "[ ] x"} {
		why, rest := decodeCooldownWhy(reason)
		if why != "" || rest != reason {
			t.Fatalf("decode(%q) = %q/%q, want unchanged", reason, why, rest)
		}
	}
}

// TestAccountCooldownClearClearsDispatchCooldowns wires the endpoint callback
// to the scheduling core: a 5xx-cooled account becomes selectable again right
// after the clear, and an unknown plugin is rejected with the transport error.
func TestAccountCooldownClearClearsDispatchCooldowns(t *testing.T) {
	dir := t.TempDir()
	logger, err := logging.NewLogger(dir)
	if err != nil {
		t.Fatalf("logging.NewLogger: %v", err)
	}
	t.Cleanup(func() { _ = logger.Close() })
	clk := &clock{at: testNow}
	d := scheduler.New(scheduler.Options{Now: clk.now})
	// pluginMetaApp installs the "mock" plugin the plugin check looks for; the
	// clear itself never calls the plugin, so a stopped plugin is fine here.
	a := pluginMetaApp(t)
	a.dispatch = d
	a.logger = logger

	snap := &scheduler.Snapshot{Pools: map[string]*scheduler.Pool{
		"mock": {
			PluginID: "mock", UpstreamEnabled: true,
			Accounts: []scheduler.Account{{ID: "acc", Label: "acc", Enabled: true, CredentialPresent: true}},
		},
	}}
	key := scheduler.AccountKey{PluginID: "mock", AccountID: "acc"}
	d.Report(key, "", 500, 0, 0)
	if selectable, _ := d.SelectableCount(snap); selectable != 0 {
		t.Fatalf("selectable after 5xx = %d, want 0", selectable)
	}

	// Unknown plugin: the transport maps this onto 404.
	if err := a.accountCooldownClear("ghost", "acc"); !errors.Is(err, webui.ErrPluginNotFound) {
		t.Fatalf("unknown plugin err = %v, want ErrPluginNotFound", err)
	}

	if err := a.accountCooldownClear("mock", "acc"); err != nil {
		t.Fatalf("clear = %v", err)
	}
	if selectable, _ := d.SelectableCount(snap); selectable != 1 {
		t.Fatalf("selectable after clear = %d, want 1", selectable)
	}

	// The clear is idempotent and requires an account id.
	if err := a.accountCooldownClear("mock", "acc"); err != nil {
		t.Fatalf("second clear = %v", err)
	}
	if err := a.accountCooldownClear("mock", ""); !errors.Is(err, webui.ErrInvalidRequest) {
		t.Fatalf("empty account err = %v, want ErrInvalidRequest", err)
	}
}

// clock is the app-package test clock (kept local; the scheduler tests own the
// scheduler one).
type clock struct{ at int64 }

func (c *clock) now() time.Time { return time.Unix(c.at, 0) }

const testNow = int64(1_700_000_000)
