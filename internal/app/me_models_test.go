package app

// P14-4: the /me model selector lists model_scope ∩ the gateway's published
// models, and POST /me/api/models/check is the minimal really callable
// operation the acceptance's "越权 403" runs against (it is a scope test only;
// it never touches an upstream).

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"aiworkgate/internal/gateway"
	"aiworkgate/internal/model"
	"aiworkgate/internal/scheduler"
	"aiworkgate/internal/secrets"
	"aiworkgate/internal/store"
)

// meModelSnapshot is the fake /v1/models universe the endpoint tests read: two
// upstream-enabled pools, one switched-off pool (its models must never leak),
// and one logical binding published as a bare name.
func meModelSnapshot() *scheduler.Snapshot {
	snap := &scheduler.Snapshot{
		Pools:    map[string]*scheduler.Pool{},
		Bindings: map[string]scheduler.Binding{},
	}
	snap.Pools["p1"] = &scheduler.Pool{PluginID: "p1", UpstreamEnabled: true, Models: map[string]model.ModelInfo{
		"m1": {ID: "m1"}, "M2": {ID: "M2"},
	}}
	snap.Pools["p2"] = &scheduler.Pool{PluginID: "p2", UpstreamEnabled: true, Models: map[string]model.ModelInfo{
		"m3": {ID: "m3"},
	}}
	snap.Pools["p4"] = &scheduler.Pool{PluginID: "p4", UpstreamEnabled: false, Models: map[string]model.ModelInfo{
		"m9": {ID: "m9"},
	}}
	snap.Bindings["alias1"] = scheduler.Binding{Logical: "alias1", Display: "Alias 1"}
	return snap
}

func meUseSnapshot(t *testing.T, snap *scheduler.Snapshot, err error) {
	t.Helper()
	old := meSnapshotFor
	meSnapshotFor = func(a *App, ctx context.Context) (*scheduler.Snapshot, error) {
		return snap, err
	}
	t.Cleanup(func() { meSnapshotFor = old })
}

// meAddScopedKey creates one extra client key so tests can vary the scope
// without touching the fixture key of newMeTestApp.
func meAddScopedKey(t *testing.T, app *App, name string, scope store.ClientKeyScope) string {
	t.Helper()
	plain, id, prefix, hash := secrets.NewClientKey()
	if err := app.secrets.AddClientKey(id, hash); err != nil {
		t.Fatalf("AddClientKey: %v", err)
	}
	if err := app.store.UpsertClientKeyScoped(id, name, prefix, hash, true, 100, scope); err != nil {
		t.Fatalf("UpsertClientKeyScoped: %v", err)
	}
	return plain
}

func meDecodeModels(t *testing.T, body string) []string {
	t.Helper()
	var payload struct {
		Models []string `json:"models"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("decode models: %v (%s)", err, body)
	}
	return payload.Models
}

func TestMeModelsEndpointIntersectsScopeWithUniverse(t *testing.T) {
	_, h, plain, _ := newMeTestApp(t) // fixture key scope: p1/*
	meUseSnapshot(t, meModelSnapshot(), nil)
	cookie := meLogin(t, h, plain)

	rec := meRequest(t, h, http.MethodGet, "/me/api/models", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("models = %d (%s)", rec.Code, rec.Body.String())
	}
	got := meDecodeModels(t, rec.Body.String())
	// p2 and p4 models and the bare binding stay out: p2 is outside p1/*, p4 is
	// not upstream-enabled, and the bare alias does not match the wildcard.
	want := []string{"p1/M2", "p1/m1"}
	if len(got) != len(want) {
		t.Fatalf("models = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("models = %v, want %v", got, want)
		}
	}
}

func TestMeModelsEmptyScopeMeansEveryPublishedModel(t *testing.T) {
	app, h, _, _ := newMeTestApp(t)
	meUseSnapshot(t, meModelSnapshot(), nil)
	plain := meAddScopedKey(t, app, "all", store.ClientKeyScope{})
	cookie := meLogin(t, h, plain)

	rec := meRequest(t, h, http.MethodGet, "/me/api/models", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("models = %d (%s)", rec.Code, rec.Body.String())
	}
	got := meDecodeModels(t, rec.Body.String())
	want := []string{"alias1", "p1/M2", "p1/m1", "p2/m3"}
	if len(got) != len(want) {
		t.Fatalf("models = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("models = %v, want %v", got, want)
		}
	}
}

func TestMeModelsBindPoolRestrictsListing(t *testing.T) {
	app, h, _, _ := newMeTestApp(t)
	meUseSnapshot(t, meModelSnapshot(), nil)
	// Exact-id scope on top of the pool binding: only p1/m1 survives.
	plain := meAddScopedKey(t, app, "bound", store.ClientKeyScope{BindPool: "p1", ModelScope: []string{"p1/m1"}})
	cookie := meLogin(t, h, plain)

	rec := meRequest(t, h, http.MethodGet, "/me/api/models", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("models = %d (%s)", rec.Code, rec.Body.String())
	}
	got := meDecodeModels(t, rec.Body.String())
	if len(got) != 1 || got[0] != "p1/m1" {
		t.Fatalf("models = %v, want [p1/m1]", got)
	}
}

func TestMeModelsFallbackToEnabledCatalog(t *testing.T) {
	app, h, _, _ := newMeTestApp(t) // fixture key scope: p1/*
	// The pool is upstream-enabled but its live model list is empty (the case
	// where the gateway skips its catalog filter): /me falls back to the
	// persisted enabled catalog rows of that pool.
	snap := &scheduler.Snapshot{Pools: map[string]*scheduler.Pool{}, Bindings: map[string]scheduler.Binding{}}
	snap.Pools["p1"] = &scheduler.Pool{PluginID: "p1", UpstreamEnabled: true}
	snap.Pools["p2"] = &scheduler.Pool{PluginID: "p2", UpstreamEnabled: true}
	meUseSnapshot(t, snap, nil)
	if err := app.store.ReplaceModelCatalog("p1", []model.ModelCatalogEntry{
		{PluginID: "p1", ModelID: "m1"}, {PluginID: "p1", ModelID: "m2"},
	}); err != nil {
		t.Fatalf("ReplaceModelCatalog p1: %v", err)
	}
	// m2 was switched off on the resource page; p2's rows are out of scope.
	if err := app.store.SetModelCatalogEnabled("p1", "m2", false); err != nil {
		t.Fatalf("SetModelCatalogEnabled: %v", err)
	}
	if err := app.store.ReplaceModelCatalog("p2", []model.ModelCatalogEntry{
		{PluginID: "p2", ModelID: "m3"},
	}); err != nil {
		t.Fatalf("ReplaceModelCatalog p2: %v", err)
	}
	cookie := meLogin(t, h, meFixturePlain)

	rec := meRequest(t, h, http.MethodGet, "/me/api/models", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("models = %d (%s)", rec.Code, rec.Body.String())
	}
	got := meDecodeModels(t, rec.Body.String())
	if len(got) != 1 || got[0] != "p1/m1" {
		t.Fatalf("models = %v, want [p1/m1] (m2 disabled, p2 out of scope)", got)
	}
}

func TestMeModelsSnapshotFailureDegradesToCatalog(t *testing.T) {
	app, h, _, _ := newMeTestApp(t) // fixture key scope: p1/*
	meUseSnapshot(t, nil, context.DeadlineExceeded)
	if err := app.store.ReplaceModelCatalog("p1", []model.ModelCatalogEntry{
		{PluginID: "p1", ModelID: "m1"},
	}); err != nil {
		t.Fatalf("ReplaceModelCatalog: %v", err)
	}
	if err := app.store.ReplaceModelCatalog("p2", []model.ModelCatalogEntry{
		{PluginID: "p2", ModelID: "m3"},
	}); err != nil {
		t.Fatalf("ReplaceModelCatalog: %v", err)
	}
	cookie := meLogin(t, h, meFixturePlain)

	rec := meRequest(t, h, http.MethodGet, "/me/api/models", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("models = %d (%s)", rec.Code, rec.Body.String())
	}
	var payload struct {
		Models   []string `json:"models"`
		Degraded bool     `json:"degraded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !payload.Degraded {
		t.Fatalf("snapshot failure must mark the listing degraded")
	}
	if len(payload.Models) != 1 || payload.Models[0] != "p1/m1" {
		t.Fatalf("models = %v, want [p1/m1]", payload.Models)
	}
}

func TestMeModelCheckAllowsInScopeAndRejectsOutside(t *testing.T) {
	_, h, _, _ := newMeTestApp(t) // fixture key scope: p1/*
	meUseSnapshot(t, meModelSnapshot(), nil)
	cookie := meLogin(t, h, meFixturePlain)

	rec := meRequest(t, h, http.MethodPost, "/me/api/models/check", `{"model":"p1/m1"}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("in-scope check = %d (%s)", rec.Code, rec.Body.String())
	}
	var ok struct {
		Allowed bool   `json:"allowed"`
		Model   string `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ok); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !ok.Allowed || ok.Model != "p1/m1" {
		t.Fatalf("allowed = %+v", ok)
	}

	// 越权: p2 lives outside the p1/* scope -> 403 model_not_allowed.
	rec = meRequest(t, h, http.MethodPost, "/me/api/models/check", `{"model":"p2/m3"}`, cookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("out-of-scope check = %d (%s), want 403", rec.Code, rec.Body.String())
	}
	var denied struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &denied); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if denied.Error.Code != "model_not_allowed" || denied.Error.Message == "" {
		t.Fatalf("error = %+v", denied.Error)
	}

	// A case/padding variant of an out-of-scope model must not slip past the
	// canonical comparison.
	rec = meRequest(t, h, http.MethodPost, "/me/api/models/check", `{"model":"  P2/M3 "}`, cookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("variant check = %d (%s), want 403", rec.Code, rec.Body.String())
	}

	if rec := meRequest(t, h, http.MethodPost, "/me/api/models/check", `{"model":""}`, cookie); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty model = %d, want 400", rec.Code)
	}
	if rec := meRequest(t, h, http.MethodPost, "/me/api/models/check", `not-json`, cookie); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body = %d, want 400", rec.Code)
	}
}

func TestMeModelCheckHonorsBindPool(t *testing.T) {
	app, h, _, _ := newMeTestApp(t)
	meUseSnapshot(t, meModelSnapshot(), nil)
	plain := meAddScopedKey(t, app, "bound", store.ClientKeyScope{BindPool: "p1", ModelScope: []string{"p1/*"}})
	cookie := meLogin(t, h, plain)

	// A bare name is rewritten to the bound pool, exactly like keyRouteModel.
	rec := meRequest(t, h, http.MethodPost, "/me/api/models/check", `{"model":"m1"}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("bare check = %d (%s)", rec.Code, rec.Body.String())
	}
	var ok struct {
		Allowed bool   `json:"allowed"`
		Model   string `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ok); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !ok.Allowed || ok.Model != "p1/m1" {
		t.Fatalf("allowed = %+v", ok)
	}

	// A foreign prefix is refused even though the scope alone would allow it.
	rec = meRequest(t, h, http.MethodPost, "/me/api/models/check", `{"model":"p2/m3"}`, cookie)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "p2") {
		t.Fatalf("foreign prefix = %d (%s), want 403 naming p2", rec.Code, rec.Body.String())
	}
}

func TestMeModelCheckRequiresSession(t *testing.T) {
	_, h, _, _ := newMeTestApp(t)
	if rec := meRequest(t, h, http.MethodPost, "/me/api/models/check", `{"model":"p1/m1"}`, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated check = %d, want 401", rec.Code)
	}
	if rec := meRequest(t, h, http.MethodGet, "/me/api/models", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated models = %d, want 401", rec.Code)
	}
}

// TestMeModelInScopeMatchesGatewaySemantics pins the matcher that mirrors
// gateway.modelInScope (internal/gateway/keys_scope.go). The vectors are the
// shared semantics: empty scope = everything, exact ids (case-insensitive via
// gateway.CanonicalModelID, the exported comparison key both sides use) and
// the "plugin/*" prefix wildcard that never matches the bare plugin id.
func TestMeModelInScopeMatchesGatewaySemantics(t *testing.T) {
	cases := []struct {
		name  string
		scope []string
		in    string
		want  bool
	}{
		{"empty scope allows everything", nil, "anything/at:all", true},
		// A non-empty scope of only blank entries allows nothing, exactly like
		// the gateway (blank entries are skipped, the scope stays restrictive);
		// in practice store decoding normalises such a scope away first.
		{"blank-only scope allows nothing", []string{" ", ""}, "m", false},
		{"exact id", []string{"p1/m1"}, "p1/m1", true},
		{"exact id is case-insensitive", []string{"P1/M1"}, "p1/m1", true},
		{"exact id tolerates padding", []string{" p1/m1 "}, "p1/m1", true},
		{"exact id mismatch", []string{"p1/m1"}, "p1/m2", false},
		{"wildcard matches own plugin", []string{"p1/*"}, "p1/m1", true},
		{"wildcard is case-insensitive", []string{"P1/*"}, "P1/Other", true},
		{"wildcard rejects other plugin", []string{"p1/*"}, "p2/m3", false},
		{"wildcard never matches the bare plugin id", []string{"p1/*"}, "p1", false},
		{"exact id needs the full name", []string{"p1/m1"}, "p1/m10", false},
		{"second entry matches", []string{"a", "p2/m3"}, "p2/m3", true},
		{"bare plugin prefix is not an exact entry", []string{"p1"}, "p1/m1", false},
	}
	for _, tc := range cases {
		if got := meModelInScope(tc.scope, tc.in); got != tc.want {
			t.Fatalf("%s: meModelInScope(%q, %q) = %v, want %v", tc.name, tc.scope, tc.in, got, tc.want)
		}
	}

	// Cross-check the canonicalisation the matcher leans on against the
	// gateway's exported comparison key, so a drift there is caught here too.
	for _, id := range []string{"P1/M1", " p1/m1 ", "p1/m1", "P1/*"} {
		if meModelInScope([]string{id}, strings.TrimSpace(id)) != (gateway.CanonicalModelID(id) == gateway.CanonicalModelID(strings.TrimSpace(id))) {
			t.Fatalf("canonical cross-check failed for %q", id)
		}
		if gateway.CanonicalModelID(id) != gateway.CanonicalModelID(strings.ToLower(strings.TrimSpace(id))) {
			t.Fatalf("CanonicalModelID drift for %q", id)
		}
	}

	// The wildcard edge the gateway test also guards: "p1/*" admits "p1/x" but
	// never "p1/" (nothing after the slash) nor "p10/x".
	if meModelInScope([]string{"p1/*"}, "p1/") {
		t.Fatalf("p1/* must not match the bare p1/ prefix")
	}
	if meModelInScope([]string{"p1/*"}, "p10/x") {
		t.Fatalf("p1/* must not match p10/x")
	}
	if !meModelInScope([]string{"p1/*"}, "p1/", "p1/x") {
		t.Fatalf("p1/* must match p1/x")
	}
}

// meFixturePlain holds the plain secret of the key newMeTestApp creates, so
// tests that replaced its scope can still log in. Set by newMeTestApp (me_test.go).
var meFixturePlain string
