package gateway

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"aiworkgate/internal/model"
	"aiworkgate/internal/scheduler"
)

// catalogStateHook is the D-33 store hook: a map from "<plugin>/<model>" to
// whether the catalog row exists and whether 接入 is on. Missing ids report
// "not in catalog" (the D-60 404 territory).
func catalogStateHook(rows map[string][2]bool) func(string, string) (bool, bool, error) {
	return func(pluginID, modelID string) (found, enabled bool, err error) {
		if row, ok := rows[pluginID+"/"+modelID]; ok {
			return row[0], row[1], nil
		}
		return false, false, nil
	}
}

// catalogPoolSnapshot builds a pool whose manifest still lists both models
// (so the plugin-level check passes) while the pool's own exposed set carries
// only the enabled one — exactly what roundModels produces for a catalog with
// one 接入-off row. models may be nil to pin the D-33 "empty pool" case.
func catalogPoolSnapshot(pluginID string, models map[string]model.ModelInfo) *scheduler.Snapshot {
	return &scheduler.Snapshot{
		InterPool: scheduler.InterPoolSmart,
		Bindings:  map[string]scheduler.Binding{},
		Pools: map[string]*scheduler.Pool{
			pluginID: {
				PluginID: pluginID, Name: pluginID, APIVersion: 2,
				UpstreamEnabled: true, Strategy: scheduler.StrategyFixed,
				Accounts: fixedPoolAccounts("acc1"),
				Models:   models,
			},
		},
	}
}

func disabledGateway(t *testing.T, snap *scheduler.Snapshot, state func(string, string) (bool, bool, error)) (*Gateway, *upstreamRecorder) {
	srv, rec := newUpstreamServer(t, jsonResponder(http.StatusOK,
		`{"model":"m1","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	reg := newFakeRegistry().add(pluginStatus("p1", true,
		model.ModelInfo{ID: "m1"}, model.ModelInfo{ID: "m2"}), srv.URL, "tok")
	g := newTestGateway(t, Options{
		Plugins:           reg,
		Schedule:          scheduler.New(scheduler.Options{}),
		Snapshot:          staticSnapshot(snap),
		ModelCatalogState: state,
	})
	return g, rec
}

// D-33 (2026-11-07 拍板): a model the catalog still knows with 接入 off is a
// client error with a dedicated code, not a 404 — the admin disabled it and
// can re-enable it.
func TestDispatchDisabledModelGets400(t *testing.T) {
	g, rec := disabledGateway(t,
		catalogPoolSnapshot("p1", map[string]model.ModelInfo{"m1": {ID: "m1"}}),
		catalogStateHook(map[string][2]bool{"p1/m2": {true, false}}))

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/m2","messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rr.Code, rr.Body.String())
	}
	errBody := decodeError(t, rr)
	if errBody.Error.Code != "model_disabled" || errBody.Error.Type != "invalid_request_error" {
		t.Fatalf("error = %+v, want invalid_request_error/model_disabled", errBody)
	}
	if !strings.Contains(errBody.Error.Message, "该模型已被管理员禁用") {
		t.Fatalf("message = %q, want 该模型已被管理员禁用", errBody.Error.Message)
	}
	if got := rec.count(); got != 0 {
		t.Fatalf("upstream calls = %d, want 0 (disabled model must not forward)", got)
	}
}

// The unknown half of the same 拍板: a model the catalog has no row for keeps
// the D-60 404 with model_not_found.
func TestDispatchUnknownModelStill404(t *testing.T) {
	g, rec := disabledGateway(t,
		catalogPoolSnapshot("p1", map[string]model.ModelInfo{"m1": {ID: "m1"}}),
		catalogStateHook(map[string][2]bool{}))

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/never-synced","messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rr.Code, rr.Body.String())
	}
	errBody := decodeError(t, rr)
	if errBody.Error.Code != "model_not_found" {
		t.Fatalf("error code = %q, want model_not_found", errBody.Error.Code)
	}
	if got := rec.count(); got != 0 {
		t.Fatalf("upstream calls = %d, want 0", got)
	}
}

// Re-enabling the model (接入 back on) lets the same request through again.
func TestDispatchDisabledModelRecoversAfterEnable(t *testing.T) {
	g, rec := disabledGateway(t,
		catalogPoolSnapshot("p1", map[string]model.ModelInfo{"m1": {ID: "m1"}}),
		catalogStateHook(map[string][2]bool{"p1/m2": {true, true}}))

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/m2","messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after re-enable (body %s)", rr.Code, rr.Body.String())
	}
	if got := rec.count(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
}

// Removing the len(pool.Models) > 0 premise: disabling the LAST model empties
// pool.Models, and with the old premise the guard skipped and disabled models
// forwarded anyway. Both requests must now be classified.
func TestDispatchEmptyPoolClassifiesByCatalog(t *testing.T) {
	state := catalogStateHook(map[string][2]bool{
		"p1/m1": {true, false}, // disabled row in an all-disabled catalog
		"p1/m2": {false, false},
	})
	gDisabled, recDisabled := disabledGateway(t, catalogPoolSnapshot("p1", nil), state)
	rr := serve(gDisabled, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/m1","messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("disabled-in-empty-pool status = %d, want 400 (body %s)", rr.Code, rr.Body.String())
	}
	if got := recDisabled.count(); got != 0 {
		t.Fatalf("upstream calls = %d, want 0", got)
	}

	gUnknown, recUnknown := disabledGateway(t, catalogPoolSnapshot("p1", nil), state)
	rr = serve(gUnknown, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/m2","messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown-in-empty-pool status = %d, want 404 (body %s)", rr.Code, rr.Body.String())
	}
	if got := recUnknown.count(); got != 0 {
		t.Fatalf("upstream calls = %d, want 0", got)
	}
}

// A store error must fail open (D-46 选路层 fail-open): the request keeps the
// pre-D-33 behaviour instead of being rejected on a database hiccup.
func TestDispatchCatalogStateErrorFailsOpen(t *testing.T) {
	g, rec := disabledGateway(t,
		catalogPoolSnapshot("p1", map[string]model.ModelInfo{"m1": {ID: "m1"}}),
		func(string, string) (bool, bool, error) { return false, false, errStoreDown })

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/m2","messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 on store failure (body %s)", rr.Code, rr.Body.String())
	}
	if got := rec.count(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
}


var errStoreDown = errors.New("store unavailable")
