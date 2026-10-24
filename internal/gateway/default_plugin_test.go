package gateway

// The default-plugin slot (plan 2026-10-21 S9a): a bare model name is no longer
// hard-wired to A. It resolves through `gateway.default_plugin` when that is
// set, and A keeps serving it only while `gateway.legacy_a_fallback` is on.
// These tests are the zero-code-deletion acceptance: the same binary answers 404
// with no default plugin and with the legacy fallback off, without a code change.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"aiworkgate/internal/model"
	"aiworkgate/internal/scheduler"
)

// newDefaultPluginGateway builds a gateway with the default-plugin switch under
// explicit test control. newTestGateway forces LegacyA=true to keep the legacy
// suites honest, so this test needs the raw constructor.
func newDefaultPluginGateway(t *testing.T, opts Options) *Gateway {
	t.Helper()
	if opts.Keys == nil {
		opts.Keys = newFakeKeys()
	}
	g := newGateway(opts)
	g.sleep = func(context.Context, time.Duration) bool { return true }
	t.Cleanup(g.CloseIdleConnections)
	return g
}

func TestBareModelTargetResolution(t *testing.T) {
	oc := pluginStatus("oc", true, model.ModelInfo{ID: "gpt-4o"})
	reg := newFakeRegistry().add(oc, "http://127.0.0.1:1", "tok")

	cases := []struct {
		name       string
		plugins    PluginRegistry
		defaultPl  string
		legacyA    bool
		wantOK     bool
		wantErr    error
		wantErrHas string
		wantInner  string
	}{
		{
			name: "default plugin serves the bare name",
			// The inner model is the bare name itself: there was no prefix to
			// strip, so the plugin receives exactly what the client asked for.
			plugins: reg, defaultPl: "oc", wantOK: true, wantInner: "gpt-4o",
		},
		{
			name: "default plugin missing",
			// A configured default that is not installed must fail loudly rather
			// than silently falling back to A.
			plugins: newFakeRegistry(), defaultPl: "oc", wantErrHas: "oc",
		},
		{
			name: "no default, legacy fallback on",
			// ok=false with a nil error is the signal for "keep the old A path".
			plugins: reg, defaultPl: "", legacyA: true,
		},
		{
			name: "no default, legacy fallback off",
			// This is the zero-code-deletion switch: nothing answers, so callers
			// write 404 model_not_found.
			plugins: reg, defaultPl: "", legacyA: false, wantErr: errNoDefaultRoute,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newDefaultPluginGateway(t, Options{
				Plugins:       tc.plugins,
				DefaultPlugin: tc.defaultPl,
				LegacyA:       tc.legacyA,
			})
			tgt, ok, err := g.bareModelTarget("gpt-4o")
			if tc.wantErr != nil {
				if err == nil || !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
			}
			if tc.wantErrHas != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrHas) {
					t.Fatalf("err = %v, want it to mention %q", err, tc.wantErrHas)
				}
			}
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (err %v)", ok, tc.wantOK, err)
			}
			if !tc.wantOK {
				return
			}
			if tc.wantErr != nil || tc.wantErrHas != "" {
				t.Fatal("ok=true must not carry an error")
			}
			if got := tgt.status.Manifest.ID; got != tc.defaultPl {
				t.Fatalf("plugin = %q, want %q", got, tc.defaultPl)
			}
			if tgt.inner != tc.wantInner {
				t.Fatalf("inner model = %q, want %q", tgt.inner, tc.wantInner)
			}
		})
	}

	// An empty model name has no default route: resolving it would produce
	// "<id>/" and bounce between the scheduler and this helper forever.
	empty := newDefaultPluginGateway(t, Options{Plugins: reg, DefaultPlugin: "oc"})
	if _, ok, err := empty.bareModelTarget(""); ok || err == nil {
		t.Fatalf("empty name = (ok %v, err %v), want a failure", ok, err)
	}
}

// TestDefaultPluginServesBareName is acceptance case 1: DefaultPlugin="oc" with
// that plugin installed sends a bare model name to the plugin, on the same path
// an explicit "oc/model" request uses.
func TestDefaultPluginServesBareName(t *testing.T) {
	pSrv, pCalls := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"ok":true}`))
	aSrv, aCalls := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"a":true}`))
	reg := newFakeRegistry().add(
		pluginStatus("oc", true, model.ModelInfo{ID: "gpt-4o"}), pSrv.URL, "oc-token")
	logs := &fakeLogSink{}
	g := newDefaultPluginGateway(t, Options{
		Plugins:       reg,
		A:             &fakeA{base: aSrv.URL, key: "sk-a"},
		Log:           logs,
		DefaultPlugin: "oc",
		LegacyA:       false,
	})

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	if got := pCalls.count(); got != 1 {
		t.Fatalf("plugin calls = %d, want 1", got)
	}
	if got := aCalls.count(); got != 0 {
		t.Fatalf("A calls = %d, want 0 (a default plugin must not touch A)", got)
	}

	upstream := pCalls.last(t)
	if upstream.Path != "/v1/chat/completions" {
		t.Fatalf("plugin path = %q, want /v1/chat/completions", upstream.Path)
	}
	if upstream.Authorization != "Bearer oc-token" {
		t.Fatalf("plugin authorization = %q, want the plugin token", upstream.Authorization)
	}
	var body map[string]any
	if err := json.Unmarshal(upstream.Body, &body); err != nil {
		t.Fatalf("plugin body: %v", err)
	}
	// The bare name is the plugin's own model id; the gateway must not invent a
	// "<plugin>/" spelling for the upstream.
	if got := body["model"]; got != "gpt-4o" {
		t.Fatalf("upstream model = %v, want gpt-4o", got)
	}
	if got := rr.Header().Get(sourceHeader); got != "plugin:oc" {
		t.Fatalf("source header = %q, want plugin:oc", got)
	}

	rows := logs.wait(t, 1)
	if rows[0].Source != "plugin:oc" || rows[0].Model != "gpt-4o" {
		t.Fatalf("unexpected log row: %+v", rows[0])
	}
}

// TestDefaultPluginMissingIsUnavailable is acceptance case 2: the configured
// plugin is not installed, so the bare request fails with 503 plugin_unavailable
// naming the plugin instead of quietly reaching A.
func TestDefaultPluginMissingIsUnavailable(t *testing.T) {
	aSrv, aCalls := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"a":true}`))
	logs := &fakeLogSink{}
	g := newDefaultPluginGateway(t, Options{
		Plugins:       newFakeRegistry(),
		A:             &fakeA{base: aSrv.URL, key: "sk-a"},
		Log:           logs,
		DefaultPlugin: "oc",
	})

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"gpt-4o"}`))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", rr.Code, rr.Body.String())
	}
	payload := decodeError(t, rr)
	if payload.Error.Type != "plugin_unavailable" || payload.Error.Code != "plugin_unavailable" {
		t.Fatalf("unexpected error: %+v", payload.Error)
	}
	if !strings.Contains(payload.Error.Message, "oc") {
		t.Fatalf("message = %q, want it to name the missing plugin", payload.Error.Message)
	}
	if got := aCalls.count(); got != 0 {
		t.Fatalf("A calls = %d, want 0", got)
	}
	rows := logs.wait(t, 1)
	if rows[0].Status != http.StatusServiceUnavailable {
		t.Fatalf("unexpected log row: %+v", rows[0])
	}
}

// TestNoDefaultPluginAndNoLegacyAIsModelNotFound is acceptance case 3: with the
// slot empty and the legacy fallback off, a bare name is a 404 and A is never
// contacted. This is what clearing one config value must achieve.
func TestNoDefaultPluginAndNoLegacyAIsModelNotFound(t *testing.T) {
	aSrv, aCalls := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"a":true}`))
	fa := &fakeA{base: aSrv.URL, key: "sk-a"}
	pSrv, pCalls := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"p":true}`))
	reg := newFakeRegistry().add(
		pluginStatus("oc", true, model.ModelInfo{ID: "gpt-4o"}), pSrv.URL, "tok")
	logs := &fakeLogSink{}
	g := newDefaultPluginGateway(t, Options{
		Plugins: reg,
		A:       fa,
		Log:     logs,
		// DefaultPlugin empty, LegacyA false: the shipped product with the
		// compatibility switch turned off.
	})

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"gpt-4o"}`))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rr.Code, rr.Body.String())
	}
	payload := decodeError(t, rr)
	if payload.Error.Code != "model_not_found" {
		t.Fatalf("error code = %q, want model_not_found", payload.Error.Code)
	}
	if got := aCalls.count(); got != 0 {
		t.Fatalf("A requests = %d, want 0 (A must not be reachable this way)", got)
	}
	if got := pCalls.count(); got != 0 {
		t.Fatalf("plugin requests = %d, want 0", got)
	}
	if got := fa.calls(); got != 0 {
		t.Fatalf("A model fetches = %d, want 0", got)
	}
	rows := logs.wait(t, 1)
	if rows[0].Status != http.StatusNotFound {
		t.Fatalf("unexpected log row: %+v", rows[0])
	}

	// The same switch applies to the translated endpoints, which historically
	// forwarded bare names to A unchanged.
	for _, path := range []string{"/v1/completions", "/v1/responses", "/v1/messages", "/v1/embeddings"} {
		t.Run(path, func(t *testing.T) {
			beforeA := aCalls.count()
			rr := serve(g, newChatRequest(t, path, clientKey, `{"model":"gpt-4o","input":"x"}`))
			if rr.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 (body %s)", rr.Code, rr.Body.String())
			}
			if got := decodeError(t, rr).Error.Code; got != "model_not_found" {
				t.Fatalf("error code = %q, want model_not_found", got)
			}
			if aCalls.count() != beforeA {
				t.Fatal("A must not be contacted for a bare name")
			}
		})
	}
}

// TestLegacyAFallbackKeepsBareNamesOnA is acceptance case 4: the shipped
// default (LegacyAFallback=true) must not change behaviour.
func TestLegacyAFallbackKeepsBareNamesOnA(t *testing.T) {
	aSrv, aCalls := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"a":true}`))
	logs := &fakeLogSink{}
	g := newDefaultPluginGateway(t, Options{
		Plugins: newFakeRegistry(),
		A:       &fakeA{base: aSrv.URL, key: "sk-a"},
		Log:     logs,
		LegacyA: true,
	})

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"gpt-4o"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Body.String(); got != `{"a":true}` {
		t.Fatalf("body = %q, want A's reply", got)
	}
	if got := rr.Header().Get(sourceHeader); got != "a" {
		t.Fatalf("source header = %q, want a", got)
	}
	upstream := aCalls.last(t)
	if upstream.Path != "/v1/chat/completions" || upstream.Authorization != "Bearer sk-a" {
		t.Fatalf("A request = %s auth %q", upstream.Path, upstream.Authorization)
	}
	rows := logs.wait(t, 1)
	if rows[0].Source != "a" {
		t.Fatalf("unexpected log row: %+v", rows[0])
	}
}

// TestDefaultPluginDoesNotChangePrefixedRouting is acceptance case 5: an
// explicit "oc/model" request behaves exactly as before, and another plugin's
// prefix still wins over the default.
func TestDefaultPluginDoesNotChangePrefixedRouting(t *testing.T) {
	ocSrv, ocCalls := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"oc":true}`))
	p2Srv, p2Calls := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"p2":true}`))
	aSrv, aCalls := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"a":true}`))
	reg := newFakeRegistry().
		add(pluginStatus("oc", true, model.ModelInfo{ID: "gpt-4o"}), ocSrv.URL, "oc-token").
		add(pluginStatus("p2", true, model.ModelInfo{ID: "m2"}), p2Srv.URL, "p2-token")
	g := newDefaultPluginGateway(t, Options{
		Plugins:       reg,
		A:             &fakeA{base: aSrv.URL, key: "sk-a"},
		DefaultPlugin: "oc",
		LegacyA:       true, // the default plugin must win even when A is enabled
	})

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"oc/gpt-4o"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	if got := ocCalls.count(); got != 1 {
		t.Fatalf("oc calls = %d, want 1", got)
	}
	if got := aCalls.count(); got != 0 {
		t.Fatalf("A calls = %d, want 0", got)
	}
	if got := rr.Header().Get(sourceHeader); got != "plugin:oc" {
		t.Fatalf("source header = %q, want plugin:oc", got)
	}

	// A different installed prefix is unrelated to the default plugin.
	rr = serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p2/m2"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	if got := p2Calls.count(); got != 1 || ocCalls.count() != 1 {
		t.Fatalf("plugin calls = p2:%d oc:%d, want 1 and 1", p2Calls.count(), ocCalls.count())
	}

	// An unknown prefix is not an installed plugin id, so matchPlugin leaves it
	// to the bare-name route. With a default plugin configured, that slot wins
	// over the legacy A fallback and the plugin itself rejects the model.
	rr = serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"other/x"}`))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 from the default plugin (body %s)", rr.Code, rr.Body.String())
	}
	payload := decodeError(t, rr)
	if payload.Error.Code != "model_not_found" || !strings.Contains(payload.Error.Message, "oc") {
		t.Fatalf("unexpected error: %+v", payload.Error)
	}
	if got := aCalls.count(); got != 0 {
		t.Fatalf("A calls = %d, want 0 (the default plugin owns bare names)", got)
	}
	if got := ocCalls.count(); got != 1 {
		t.Fatalf("oc calls = %d, want 1 (an unknown model is rejected before the call)", got)
	}
}

// TestDefaultPluginBareNameKeepsScheduling pins the dispatch.go change: a bare
// name served by the default plugin must come back through the scheduler (an
// account is pinned), not bypass account routing.
func TestDefaultPluginBareNameKeepsScheduling(t *testing.T) {
	pSrv, pCalls := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(accountHeader, r.Header.Get(accountHeader))
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	reg := newFakeRegistry().add(
		pluginStatus("oc", true, model.ModelInfo{ID: "gpt-4o"}), pSrv.URL, "oc-token")
	g := newDefaultPluginGateway(t, Options{
		Plugins:       reg,
		Schedule:      scheduler.New(scheduler.Options{}),
		Snapshot:      staticSnapshot(bareSnapshot("oc", 2, "acc1", "acc2")),
		DefaultPlugin: "oc",
	})

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	upstream := pCalls.last(t)
	if got := upstream.Header.Get(accountHeader); got != "acc1" {
		t.Fatalf("pinned account = %q, want acc1 (the scheduler must pick it)", got)
	}
	if got := rr.Header().Get(sourceHeader); got != "plugin:oc" {
		t.Fatalf("source header = %q, want plugin:oc", got)
	}
	var body map[string]any
	if err := json.Unmarshal(upstream.Body, &body); err != nil {
		t.Fatalf("plugin body: %v", err)
	}
	if got := body["model"]; got != "gpt-4o" {
		t.Fatalf("upstream model = %v, want gpt-4o", got)
	}
}

// TestDefaultPluginServesTranslatedEndpoints covers the two handlers that fold
// the default-plugin answer into their existing plugin branch: /v1/messages and
// /v1/completions must translate a bare name exactly like "oc/gpt-4o", not fall
// back to A.
func TestDefaultPluginServesTranslatedEndpoints(t *testing.T) {
	const chatReply = `{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	pSrv, pCalls := newUpstreamServer(t, jsonResponder(http.StatusOK, chatReply))
	aSrv, aCalls := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"a":true}`))
	reg := newFakeRegistry().add(
		pluginStatus("oc", true, model.ModelInfo{ID: "gpt-4o"}), pSrv.URL, "oc-token")
	g := newDefaultPluginGateway(t, Options{
		Plugins:       reg,
		A:             &fakeA{base: aSrv.URL, key: "sk-a"},
		DefaultPlugin: "oc",
	})

	// /v1/messages is translated into a chat request for the plugin model.
	rr := serve(g, newChatRequest(t, "/v1/messages", clientKey,
		`{"model":"gpt-4o","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("/v1/messages status = %d (body %s)", rr.Code, rr.Body.String())
	}
	up := pCalls.last(t)
	if up.Path != "/v1/chat/completions" {
		t.Fatalf("plugin path = %q, want /v1/chat/completions", up.Path)
	}
	var chat map[string]any
	if err := json.Unmarshal(up.Body, &chat); err != nil {
		t.Fatalf("plugin body: %v", err)
	}
	if got := chat["model"]; got != "gpt-4o" {
		t.Fatalf("plugin model = %v, want gpt-4o", got)
	}

	// /v1/completions is translated the same way.
	rr = serve(g, newChatRequest(t, "/v1/completions", clientKey,
		`{"model":"gpt-4o","prompt":"hi"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("/v1/completions status = %d (body %s)", rr.Code, rr.Body.String())
	}
	if got := pCalls.count(); got != 2 {
		t.Fatalf("plugin calls = %d, want 2", got)
	}
	if got := aCalls.count(); got != 0 {
		t.Fatalf("A calls = %d, want 0 (translated endpoints must use the plugin)", got)
	}
}

// bareSnapshot is singlePoolSnapshot with the bare model adopted into the pool
// catalog, so the scheduled path accepts it.
func bareSnapshot(pluginID string, apiLevel int, accounts ...string) *scheduler.Snapshot {
	snap := singlePoolSnapshot(pluginID, apiLevel, accounts...)
	snap.Pools[pluginID].Models["gpt-4o"] = model.ModelInfo{ID: "gpt-4o"}
	return snap
}
