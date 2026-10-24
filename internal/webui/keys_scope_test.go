package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"aiworkgate/internal/model"
	"aiworkgate/internal/store"
)

// keyScopeFixture is the host side of the key editor: an in-memory list plus
// the callbacks the plan added.
type keyScopeFixture struct {
	keys    []model.ClientKey
	created []KeySpec
	updated []KeySpec
	deleted []string
}

func (f *keyScopeFixture) options() Options {
	return Options{
		Keys: func() []model.ClientKey { return f.keys },
		CreateKeySpec: func(spec KeySpec) (string, model.ClientKey, error) {
			f.created = append(f.created, spec)
			key := model.ClientKey{
				ID: "agk_new", Name: spec.Name, Prefix: "agk_newp", Enabled: spec.Enabled,
				ModelScope: spec.ModelScope, BindPool: spec.BindPool, DailyQuota: spec.DailyQuota,
				QuotaLeft: store.ClientKeyQuotaLeft(spec.DailyQuota, 0),
			}
			f.keys = append(f.keys, key)
			return "agk_secret_value", key, nil
		},
		UpdateKey: func(id string, spec KeySpec) (model.ClientKey, error) {
			f.updated = append(f.updated, spec)
			for i := range f.keys {
				if f.keys[i].ID != id {
					continue
				}
				f.keys[i].Name = spec.Name
				f.keys[i].Enabled = spec.Enabled
				f.keys[i].ModelScope = spec.ModelScope
				f.keys[i].BindPool = spec.BindPool
				f.keys[i].DailyQuota = spec.DailyQuota
				f.keys[i].QuotaLeft = store.ClientKeyQuotaLeft(spec.DailyQuota, f.keys[i].UsedToday)
				return f.keys[i], nil
			}
			return model.ClientKey{}, store.ErrNotFound
		},
		DeleteKey: func(id string) error {
			f.deleted = append(f.deleted, id)
			return nil
		},
		Plugins: func() []model.PluginStatus {
			return []model.PluginStatus{
				{Manifest: model.Manifest{ID: "trae"}},
				{Manifest: model.Manifest{ID: "buddy"}},
			}
		},
	}
}

func TestKeyCreateWithScope(t *testing.T) {
	fixture := &keyScopeFixture{}
	handler := New(fixture.options())

	recorder := doRequest(t, handler, http.MethodPost, "/api/keys",
		`{"name":"  ide  ","model_scope":["trae/*","gpt-4"],"bind_pool":"trae","daily_quota":50}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201 (body %s)", recorder.Code, recorder.Body.String())
	}
	if len(fixture.created) != 1 {
		t.Fatalf("CreateKeySpec calls = %d", len(fixture.created))
	}
	spec := fixture.created[0]
	if spec.Name != "ide" || spec.BindPool != "trae" || spec.DailyQuota != 50 || !spec.Enabled {
		t.Fatalf("created spec = %+v", spec)
	}
	if got := strings.Join(spec.ModelScope, ","); got != "trae/*,gpt-4" {
		t.Fatalf("model_scope = %q", got)
	}

	var created struct {
		Secret string          `json:"secret"`
		Key    model.ClientKey `json:"key"`
	}
	decodeJSONBody(t, recorder, &created)
	if created.Secret != "agk_secret_value" {
		t.Fatalf("secret = %q", created.Secret)
	}
	if created.Key.DailyQuota != 50 || created.Key.QuotaLeft != 50 || created.Key.BindPool != "trae" {
		t.Fatalf("created key = %+v", created.Key)
	}
}

func TestKeyCreateValidation(t *testing.T) {
	fixture := &keyScopeFixture{}
	handler := New(fixture.options())

	tooMany := make([]string, store.MaxClientKeyModelScope+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("m%d", i)
	}
	tooManyJSON, _ := json.Marshal(tooMany)
	longEntry, _ := json.Marshal([]string{strings.Repeat("x", store.MaxClientKeyScopeRunes+1)})

	cases := []struct {
		name string
		body string
	}{
		{"missing name", `{"model_scope":["a"]}`},
		{"empty scope entry", `{"name":"k","model_scope":["a"," "]} `},
		{"too many entries", fmt.Sprintf(`{"name":"k","model_scope":%s}`, tooManyJSON)},
		{"entry too long", fmt.Sprintf(`{"name":"k","model_scope":%s}`, longEntry)},
		{"negative quota", `{"name":"k","daily_quota":-1}`},
		{"quota too large", fmt.Sprintf(`{"name":"k","daily_quota":%d}`, store.MaxClientKeyDailyQuota+1)},
		{"unknown plugin", `{"name":"k","bind_pool":"nope"}`},
		{"scope not an array", `{"name":"k","model_scope":"trae/*"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := doRequest(t, handler, http.MethodPost, "/api/keys", tc.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("create = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
			}
		})
	}
	if len(fixture.created) != 0 {
		t.Fatalf("invalid requests reached the host: %+v", fixture.created)
	}
}

func TestKeyListCarriesUsageAndQuota(t *testing.T) {
	fixture := &keyScopeFixture{keys: []model.ClientKey{{
		ID: "agk_1", Name: "ide", Prefix: "agk_ab", Enabled: true,
		ModelScope: []string{"trae/*"}, BindPool: "trae", DailyQuota: 10,
		UsedToday: 3, QuotaLeft: 7,
	}}}
	handler := New(fixture.options())

	recorder := doRequest(t, handler, http.MethodGet, "/api/keys", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("list = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	var keys []model.ClientKey
	decodeJSONBody(t, recorder, &keys)
	if len(keys) != 1 {
		t.Fatalf("keys = %+v", keys)
	}
	key := keys[0]
	if key.UsedToday != 3 || key.QuotaLeft != 7 {
		t.Fatalf("usage = used %d left %d, want 3/7", key.UsedToday, key.QuotaLeft)
	}
	if key.BindPool != "trae" || key.DailyQuota != 10 || len(key.ModelScope) != 1 {
		t.Fatalf("scope = %+v", key)
	}
}

func TestKeyUpdate(t *testing.T) {
	fixture := &keyScopeFixture{keys: []model.ClientKey{{
		ID: "agk_1", Name: "ide", Prefix: "agk_ab", Enabled: true, DailyQuota: 10,
		UsedToday: 4,
	}}}
	handler := New(fixture.options())

	recorder := doRequest(t, handler, http.MethodPut, "/api/keys/agk_1",
		`{"name":"renamed","enabled":false,"model_scope":["buddy/*"],"bind_pool":"buddy","daily_quota":5}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("update = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if len(fixture.updated) != 1 {
		t.Fatalf("UpdateKey calls = %d", len(fixture.updated))
	}
	spec := fixture.updated[0]
	if spec.Name != "renamed" || spec.Enabled || spec.BindPool != "buddy" || spec.DailyQuota != 5 {
		t.Fatalf("updated spec = %+v", spec)
	}
	var payload struct {
		Key model.ClientKey `json:"key"`
	}
	decodeJSONBody(t, recorder, &payload)
	if payload.Key.Name != "renamed" || payload.Key.Enabled || payload.Key.QuotaLeft != 1 {
		t.Fatalf("echoed key = %+v", payload.Key)
	}

	// A partial edit keeps the stored fields: only the quota changes here.
	recorder = doRequest(t, handler, http.MethodPut, "/api/keys/agk_1", `{"daily_quota":0}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("partial update = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	last := fixture.updated[len(fixture.updated)-1]
	if last.Name != "renamed" || last.BindPool != "buddy" || len(last.ModelScope) != 1 || last.DailyQuota != 0 {
		t.Fatalf("partial spec = %+v", last)
	}

	recorder = doRequest(t, handler, http.MethodPut, "/api/keys/agk_missing", `{"name":"x"}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown key = %d, want 404", recorder.Code)
	}

	recorder = doRequest(t, handler, http.MethodPut, "/api/keys/agk_1", `{"bind_pool":"nope"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown plugin = %d, want 400", recorder.Code)
	}
}

func TestKeyDeleteAndScopesUnsupported(t *testing.T) {
	fixture := &keyScopeFixture{}
	handler := New(fixture.options())

	recorder := doRequest(t, handler, http.MethodDelete, "/api/keys/agk_1", "")
	if recorder.Code != http.StatusOK || len(fixture.deleted) != 1 {
		t.Fatalf("delete = %d, deleted = %+v", recorder.Code, fixture.deleted)
	}

	// A host that wired only the legacy callbacks keeps name-only creates and
	// refuses scope fields instead of silently dropping them.
	var legacyCreated int
	legacy := New(Options{
		Keys: func() []model.ClientKey { return nil },
		CreateKey: func(name string) (string, model.ClientKey, error) {
			legacyCreated++
			return "agk_legacy", model.ClientKey{ID: "agk_l", Name: name, Enabled: true}, nil
		},
		DeleteKey: func(string) error { return nil },
	})
	recorder = doRequest(t, legacy, http.MethodPost, "/api/keys", `{"name":"laptop"}`)
	if recorder.Code != http.StatusCreated || legacyCreated != 1 {
		t.Fatalf("legacy create = %d, calls = %d", recorder.Code, legacyCreated)
	}
	recorder = doRequest(t, legacy, http.MethodPost, "/api/keys", `{"name":"laptop","daily_quota":5}`)
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("legacy scope create = %d, want 501", recorder.Code)
	}
	recorder = doRequest(t, legacy, http.MethodPut, "/api/keys/agk_l", `{"name":"x"}`)
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("legacy update = %d, want 501", recorder.Code)
	}
}
