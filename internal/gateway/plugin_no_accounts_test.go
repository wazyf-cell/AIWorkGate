package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"aiworkgate/internal/model"
	"aiworkgate/internal/scheduler"
)

// A plugin that forwards to a single built-in upstream publishes no accounts:
// there is nothing to load balance across. The scheduler used to treat every
// "<id>/<model>" hit as a schedulable candidate anyway, find zero candidates and
// answer 503 before the plugin was ever called, which made such a plugin
// unusable no matter how correct it was. The request must reach the plugin.
func TestScheduledPluginWithoutAccountsForwardsDirectly(t *testing.T) {
	srv, rec := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	// No accounts: the shape a forwarding plugin has.
	g := scheduledGateway(t, reg, &fakeLogSink{}, singlePoolSnapshot("p1", 2))

	w := httptest.NewRecorder()
	g.ServeHTTP(w, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/m1","messages":[{"role":"user","content":"hi"}]}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if got := rec.count(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (the plugin must be reached)", got)
	}
}

// The escape hatch must stay narrow. A plugin that DOES publish accounts keeps
// the account-routing path, otherwise quota enforcement, stickiness and failover
// would silently stop applying to every existing plugin.
func TestScheduledPluginWithAccountsStillUsesScheduler(t *testing.T) {
	srv, rec := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(accountHeader, r.Header.Get(accountHeader))
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	g := scheduledGateway(t, reg, &fakeLogSink{}, singlePoolSnapshot("p1", 2, "acc1"))

	w := httptest.NewRecorder()
	g.ServeHTTP(w, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/m1","messages":[{"role":"user","content":"hi"}]}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if got := rec.count(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
	// The scheduler pins an account; the direct plugin path does not.
	if got := rec.last(t).Header.Get(accountHeader); got != "acc1" {
		t.Fatalf("pinned account = %q, want acc1 (account routing must still apply)", got)
	}
}

// A plugin without the upstream capability is not a forwarding target: an empty
// account list there means "nothing to serve", not "forwards on its own". The
// suggestion must not fire, so the scheduler still reports no candidate.
func TestScheduledPluginWithoutUpstreamCapabilityStaysUnscheduled(t *testing.T) {
	srv, rec := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	snap := singlePoolSnapshot("p1", 2)
	snap.Pools["p1"].UpstreamEnabled = false
	g := scheduledGateway(t, reg, &fakeLogSink{}, snap)

	w := httptest.NewRecorder()
	g.ServeHTTP(w, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/m1","messages":[{"role":"user","content":"hi"}]}`))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (no accounts and no upstream capability); body = %s",
			w.Code, w.Body.String())
	}
	if got := rec.count(); got != 0 {
		t.Fatalf("upstream calls = %d, want 0", got)
	}
}

// pluginWithoutAccounts is the whole decision, so pin its four conditions.
func TestPluginWithoutAccountsConditions(t *testing.T) {
	snap := singlePoolSnapshot("p1", 2)
	snap.Pools["p2"] = &scheduler.Pool{PluginID: "p2", UpstreamEnabled: true, Accounts: nil}
	snap.Pools["p3"] = &scheduler.Pool{PluginID: "p3", UpstreamEnabled: false, Accounts: nil}
	snap.Pools["p4"] = &scheduler.Pool{
		PluginID: "p4", UpstreamEnabled: true,
		Accounts: fixedPoolAccounts("acc1"),
	}
	g := newTestGateway(t, Options{
		Plugins:  newFakeRegistry(),
		Log:      &fakeLogSink{},
		Schedule: scheduler.New(scheduler.Options{}),
		Snapshot: staticSnapshot(snap),
	})

	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"p1", true},  // upstream + zero accounts
		{"p2", true},  // same, explicit nil
		{"p3", false}, // no upstream capability
		{"p4", false}, // has accounts
		{"", false},   // no id
		{"missing", false},
	} {
		if got := g.pluginWithoutAccounts(nil, tc.id); got != tc.want {
			t.Fatalf("pluginWithoutAccounts(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}

	// A nil snapshot must not be read as "zero accounts".
	noSnap := newTestGateway(t, Options{
		Plugins:  newFakeRegistry(),
		Log:      &fakeLogSink{},
		Schedule: scheduler.New(scheduler.Options{}),
	})
	if noSnap.pluginWithoutAccounts(nil, "p1") {
		t.Fatal("pluginWithoutAccounts returned true without a snapshot")
	}
}
