package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aiworkgate/internal/model"
	"aiworkgate/internal/scheduler"
)

// staticSnapshot returns a Snapshot function serving one fixed view.
func staticSnapshot(snap *scheduler.Snapshot) func(context.Context) (*scheduler.Snapshot, error) {
	return func(context.Context) (*scheduler.Snapshot, error) { return snap, nil }
}

func fixedPoolAccounts(ids ...string) []scheduler.Account {
	out := make([]scheduler.Account, 0, len(ids))
	for i, id := range ids {
		out = append(out, scheduler.Account{
			ID: id, Enabled: true, CredentialPresent: true, Priority: i + 1,
			Remaining: 100, Total: 100,
		})
	}
	return out
}

func singlePoolSnapshot(pluginID string, apiLevel int, accounts ...string) *scheduler.Snapshot {
	return &scheduler.Snapshot{
		InterPool: scheduler.InterPoolSmart,
		Bindings:  map[string]scheduler.Binding{},
		Pools: map[string]*scheduler.Pool{
			pluginID: {
				PluginID: pluginID, Name: pluginID, APIVersion: apiLevel,
				UpstreamEnabled: true, Strategy: scheduler.StrategyFixed,
				Accounts: fixedPoolAccounts(accounts...),
				Models: map[string]model.ModelInfo{
					"m1": {ID: "m1"},
				},
			},
		},
	}
}

func scheduledGateway(t *testing.T, reg *fakeRegistry, logs *fakeLogSink, snap *scheduler.Snapshot) *Gateway {
	t.Helper()
	return newTestGateway(t, Options{
		Plugins:  reg,
		Log:      logs,
		Schedule: scheduler.New(scheduler.Options{}),
		Snapshot: staticSnapshot(snap),
	})
}

func TestScheduledChatPinsAccountAndLogsOutcome(t *testing.T) {
	srv, rec := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(accountHeader, r.Header.Get(accountHeader))
		w.Header().Set(creditsHeader, "2.5")
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	logs := &fakeLogSink{}
	g := scheduledGateway(t, reg, logs, singlePoolSnapshot("p1", 2, "acc1", "acc2"))

	w := httptest.NewRecorder()
	g.ServeHTTP(w, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/m1","messages":[{"role":"user","content":"hi"}]}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if got := rec.count(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
	upstream := rec.last(t)
	if got := upstream.Header.Get(accountHeader); got != "acc1" {
		t.Fatalf("pinned account = %q, want acc1", got)
	}
	if got := upstream.Header.Get(requestIDHeader); got == "" {
		t.Fatal("request id header missing")
	}
	if got := upstream.Header.Get("Authorization"); got != "Bearer tok" {
		t.Fatalf("authorization = %q", got)
	}
	var body map[string]any
	if err := json.Unmarshal(upstream.Body, &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if got := body["model"]; got != "m1" {
		t.Fatalf("upstream model = %v, want m1", got)
	}

	rows := logs.wait(t, 1)
	row := rows[0]
	if row.Account != "acc1" {
		t.Fatalf("log account = %q, want acc1", row.Account)
	}
	if row.Credits == nil || *row.Credits != 2.5 {
		t.Fatalf("log credits = %v, want 2.5", row.Credits)
	}
	if row.Status != http.StatusOK {
		t.Fatalf("log status = %d", row.Status)
	}
	if row.FirstTokenAt == "" {
		t.Fatal("log first_token_at missing")
	}
}

func TestScheduledChatFailsOverBeforeFirstByte(t *testing.T) {
	srv, rec := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(accountHeader, r.Header.Get(accountHeader))
		if r.Header.Get(accountHeader) == "acc1" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	logs := &fakeLogSink{}
	g := scheduledGateway(t, reg, logs, singlePoolSnapshot("p1", 2, "acc1", "acc2"))

	w := httptest.NewRecorder()
	g.ServeHTTP(w, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p1/m1"}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	reqs := rec.all()
	// A 429 retries the same account three times (1s/2s/3s by default) before
	// switching, so acc1 owns the first four calls and acc2 the last one.
	wantAccounts := []string{"acc1", "acc1", "acc1", "acc1", "acc2"}
	if len(reqs) != len(wantAccounts) {
		t.Fatalf("upstream calls = %d, want %d (429 retries then fail-over)", len(reqs), len(wantAccounts))
	}
	for i, want := range wantAccounts {
		if got := reqs[i].Header.Get(accountHeader); got != want {
			t.Fatalf("attempt %d account = %q, want %s", i, got, want)
		}
	}
	rows := logs.wait(t, 1)
	if rows[0].Account != "acc2" || rows[0].Status != http.StatusOK {
		t.Fatalf("final log = %+v, want success on acc2", rows[0])
	}
}

func TestScheduledChatFailsOverOnUpstream404(t *testing.T) {
	srv, rec := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(accountHeader, r.Header.Get(accountHeader))
		if r.Header.Get(accountHeader) == "acc1" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"unknown model"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	logs := &fakeLogSink{}
	g := scheduledGateway(t, reg, logs, singlePoolSnapshot("p1", 2, "acc1", "acc2"))

	w := httptest.NewRecorder()
	g.ServeHTTP(w, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p1/m1"}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	reqs := rec.all()
	if len(reqs) != 2 {
		t.Fatalf("upstream calls = %d, want 2 (404 must switch accounts)", len(reqs))
	}
	if got := reqs[1].Header.Get(accountHeader); got != "acc2" {
		t.Fatalf("second attempt account = %q, want acc2", got)
	}
	if rows := logs.wait(t, 1); rows[0].Account != "acc2" || rows[0].Status != http.StatusOK {
		t.Fatalf("final log = %+v, want success on acc2", rows[0])
	}
}

func TestScheduledChatLogicalBindingCrossesPools(t *testing.T) {
	bad, badRec := newUpstreamServer(t, jsonResponder(http.StatusBadGateway, `{"error":"boom"}`))
	good, goodRec := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"ok":true}`))

	reg := newFakeRegistry().
		add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), bad.URL, "tok1").
		add(pluginStatus("p2", true, model.ModelInfo{ID: "m2"}), good.URL, "tok2")

	snap := &scheduler.Snapshot{
		InterPool: scheduler.InterPoolFixed,
		Bindings: map[string]scheduler.Binding{
			"alias": {
				Logical: "alias",
				Display: "Alias",
				Items: []scheduler.Source{
					{PluginID: "p1", Model: "m1"},
					{PluginID: "p2", Model: "m2"},
				},
			},
		},
		Pools: map[string]*scheduler.Pool{
			"p1": {
				PluginID: "p1", APIVersion: 2, UpstreamEnabled: true,
				Strategy: scheduler.StrategyFixed, Accounts: fixedPoolAccounts("a1"),
				Models: map[string]model.ModelInfo{"m1": {ID: "m1"}},
			},
			"p2": {
				PluginID: "p2", APIVersion: 2, UpstreamEnabled: true,
				Strategy: scheduler.StrategyFixed, Accounts: fixedPoolAccounts("b1"),
				Models: map[string]model.ModelInfo{"m2": {ID: "m2"}},
			},
		},
	}

	logs := &fakeLogSink{}
	g := scheduledGateway(t, reg, logs, snap)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"alias"}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	// The 502 policy is one 500ms retry on the same account before switching,
	// so p1 sees the original call plus the retry.
	if badRec.count() != 2 || goodRec.count() != 1 {
		t.Fatalf("calls p1=%d p2=%d, want 2/1", badRec.count(), goodRec.count())
	}
	if got := goodRec.last(t).Header.Get(accountHeader); got != "b1" {
		t.Fatalf("pinned account = %q, want b1", got)
	}
	var body map[string]any
	if err := json.Unmarshal(goodRec.last(t).Body, &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if got := body["model"]; got != "m2" {
		t.Fatalf("upstream model = %v, want m2", got)
	}
	row := logs.wait(t, 1)[0]
	if row.LogicalModel != "alias" || row.Model != "alias" {
		t.Fatalf("log models = logical %q, model %q", row.LogicalModel, row.Model)
	}
	if row.Account != "b1" {
		t.Fatalf("log account = %q, want b1", row.Account)
	}
}

func TestScheduledChatAppliesDowngradeAndDefaultReasoning(t *testing.T) {
	srv, rec := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"ok":true}`))
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")

	snap := singlePoolSnapshot("p1", 2, "acc1")
	snap.Pools["p1"].BackgroundDowngrade = true
	snap.Pools["p1"].DefaultReasoning = "low"
	snap.Pools["p1"].Models = map[string]model.ModelInfo{
		"m1":       {ID: "m1", Multiplier: 2},
		"m1-flash": {ID: "m1-flash", Multiplier: 0.5},
	}

	logs := &fakeLogSink{}
	g := scheduledGateway(t, reg, logs, snap)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/m1","max_tokens":64,"messages":[{"role":"user","content":"ping"}]}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get(downgradedHeader); got != scheduler.DowngradeBackground {
		t.Fatalf("downgraded header = %q, want %q", got, scheduler.DowngradeBackground)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.last(t).Body, &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if got := body["model"]; got != "m1-flash" {
		t.Fatalf("upstream model = %v, want m1-flash", got)
	}
	if got := body["reasoning_effort"]; got != "low" {
		t.Fatalf("reasoning_effort = %v, want low", got)
	}
}

func TestScheduledChatAPILevelOneLeavesAccountToPlugin(t *testing.T) {
	srv, rec := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"ok":true}`))
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	logs := &fakeLogSink{}
	g := scheduledGateway(t, reg, logs, singlePoolSnapshot("p1", 1, "acc1"))

	w := httptest.NewRecorder()
	g.ServeHTTP(w, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p1/m1"}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if got := rec.last(t).Header.Get(accountHeader); got != "" {
		t.Fatalf("api_level 1 request carried account %q", got)
	}
	row := logs.wait(t, 1)[0]
	if row.Account != "" {
		t.Fatalf("api_level 1 log account = %q, want empty", row.Account)
	}
}

func TestScheduledChatHedgesSlowStream(t *testing.T) {
	srv, rec := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.Header.Get(accountHeader) == "acc1" {
			time.Sleep(1500 * time.Millisecond)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set(accountHeader, r.Header.Get(accountHeader))
		_, _ = io.WriteString(w, "data: {\"delta\":\""+r.Header.Get(accountHeader)+"\"}\n\n")
	})
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	snap := singlePoolSnapshot("p1", 2, "acc1", "acc2")
	snap.Pools["p1"].HedgeMS = 1000

	logs := &fakeLogSink{}
	g := scheduledGateway(t, reg, logs, snap)

	started := time.Now()
	w := httptest.NewRecorder()
	g.ServeHTTP(w, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p1/m1","stream":true}`))
	elapsed := time.Since(started)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if elapsed >= 1400*time.Millisecond {
		t.Fatalf("elapsed = %v, hedge did not beat the slow account", elapsed)
	}
	if !strings.Contains(w.Body.String(), "acc2") {
		t.Fatalf("body = %q, want the fast hedge winner", w.Body.String())
	}
	if got := w.Header().Get(accountHeader); got != "acc2" {
		t.Fatalf("response account = %q, want acc2", got)
	}
	reqs := rec.all()
	if len(reqs) != 2 {
		t.Fatalf("upstream calls = %d, want 2", len(reqs))
	}
	if got := reqs[1].Header.Get(hedgeHeader); got != "1" {
		t.Fatalf("hedge header = %q, want 1", got)
	}
	row := logs.wait(t, 1)[0]
	if row.Account != "acc2" {
		t.Fatalf("log account = %q, want acc2", row.Account)
	}
}

func TestModelsListsLogicalBindings(t *testing.T) {
	srv, _ := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"ok":true}`))
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	snap := singlePoolSnapshot("p1", 2, "acc1")
	snap.Bindings["alias"] = scheduler.Binding{
		Logical: "alias", Display: "Alias",
		Items: []scheduler.Source{{PluginID: "p1", Model: "m1"}},
	}
	g := scheduledGateway(t, reg, &fakeLogSink{}, snap)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+clientKey)
	g.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var list modelsList
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, entry := range list.Data {
		if entry.ID == "alias" {
			if entry.OwnedBy != "scheduler" || entry.Name != "Alias" {
				t.Fatalf("binding entry = %+v", entry)
			}
			return
		}
	}
	t.Fatalf("logical model alias missing from %+v", list.Data)
}

// A plugin that declares the upstream capability but publishes no accounts is a
// forwarding plugin: it owns its upstream, so there is nothing to schedule. The
// request must reach it instead of failing as "no candidate".
//
// This assertion was REVERSED from 503/no-call when the forwarding-plugin rule
// landed. The old expectation encoded "zero accounts means unschedulable"; that
// rule made every zero-account upstream plugin unusable (the scheduler answered
// 503 before the plugin was ever called). The 503 path still exists and is
// pinned by TestScheduledPluginWithoutUpstreamCapabilityStaysUnscheduled, which
// builds a pool with UpstreamEnabled=false.
func TestScheduledForwardingPluginIsNotAnsweredNoCandidate(t *testing.T) {
	srv, rec := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"ok":true}`))
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	snap := singlePoolSnapshot("p1", 2) // no accounts, upstream capability on
	g := scheduledGateway(t, reg, &fakeLogSink{}, snap)

	w := httptest.NewRecorder()
	g.ServeHTTP(w, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p1/m1"}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if rec.count() != 1 {
		t.Fatalf("upstream calls = %d, want 1", rec.count())
	}
}

// The no-candidate failure itself must still exist for a pool that cannot serve
// anything: no upstream capability and no accounts.
func TestScheduledChatReportsNoCandidate(t *testing.T) {
	srv, rec := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"ok":true}`))
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	snap := singlePoolSnapshot("p1", 2)
	snap.Pools["p1"].UpstreamEnabled = false // cannot forward on its own, and has no accounts
	g := scheduledGateway(t, reg, &fakeLogSink{}, snap)

	w := httptest.NewRecorder()
	g.ServeHTTP(w, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p1/m1"}`))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", w.Code, w.Body.String())
	}
	if rec.count() != 0 {
		t.Fatalf("upstream calls = %d, want 0", rec.count())
	}
	if got := w.Header().Get("Retry-After"); got == "" {
		t.Fatal("Retry-After missing")
	}
}

// A scheduled request must count as an in-flight call for the whole handler;
// otherwise the plugin manager's idle reaper can stop a plugin mid-stream.
func TestScheduledChatKeepsPluginInFlight(t *testing.T) {
	srv, _ := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"ok":true}`))
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	g := scheduledGateway(t, reg, &fakeLogSink{}, singlePoolSnapshot("p1", 2, "acc1"))

	w := httptest.NewRecorder()
	g.ServeHTTP(w, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p1/m1"}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	began, ended := reg.callCounts("p1")
	if began != 1 {
		t.Fatalf("BeginCall calls = %d, want 1", began)
	}
	if ended != 1 {
		t.Fatalf("releases = %d, want 1", ended)
	}
}

// A binding whose logical name contains "/" must stay routable. The prefix
// ("vendor") is not an installed plugin, so the request belongs to the
// scheduler, not to an unknown plugin.
func TestScheduledChatRoutesSlashLogicalBinding(t *testing.T) {
	srv, rec := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"ok":true}`))
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	snap := singlePoolSnapshot("p1", 2, "acc1")
	snap.Bindings["vendor/model"] = scheduler.Binding{
		Logical: "vendor/model",
		Items:   []scheduler.Source{{PluginID: "p1", Model: "m1"}},
	}
	g := scheduledGateway(t, reg, &fakeLogSink{}, snap)

	w := httptest.NewRecorder()
	g.ServeHTTP(w, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"vendor/model"}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if rec.count() != 1 {
		t.Fatalf("upstream calls = %d, want 1", rec.count())
	}
	upstream := rec.last(t)
	var body map[string]any
	if err := json.Unmarshal(upstream.Body, &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if got := body["model"]; got != "m1" {
		t.Fatalf("upstream model = %v, want m1", got)
	}
}

// The host accepts several credential header spellings from its own clients.
// None of them may be forwarded upstream, where a plugin can interpret them as
// its own credential.
func TestGatewayCredentialHeadersAreNotForwarded(t *testing.T) {
	srv, rec := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"ok":true}`))
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	g := scheduledGateway(t, reg, &fakeLogSink{}, singlePoolSnapshot("p1", 2, "acc1"))

	req := newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p1/m1"}`)
	req.Header.Set("X-API-Key", "gw-client-key")
	req.Header.Set("Api-Key", "gw-client-key")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	upstream := rec.last(t)
	if got := upstream.Header.Get("X-API-Key"); got != "" {
		t.Fatalf("X-API-Key leaked upstream: %q", got)
	}
	if got := upstream.Header.Get("Api-Key"); got != "" {
		t.Fatalf("Api-Key leaked upstream: %q", got)
	}
	if got := upstream.Header.Get("Authorization"); got != "Bearer tok" {
		t.Fatalf("authorization = %q, want the plugin token", got)
	}
}
