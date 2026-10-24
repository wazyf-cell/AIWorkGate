package webui

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"aiworkgate/internal/store"
)

// newOverrideTestServer wires the admin handler to a throwaway database
// through the same Options callbacks the host uses.
func newOverrideTestServer(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	return New(Options{
		ModelOverrides:      st.GetModelOverrides,
		PutModelOverride:    st.PutModelOverride,
		DeleteModelOverride: st.DeleteModelOverride,
	}), st
}

// decodeOverrideView reads one model entry out of a GET response.
func decodeOverrideView(t *testing.T, recorderBody []byte, model string) map[string]any {
	t.Helper()
	var payload struct {
		Overrides map[string]map[string]any `json:"overrides"`
	}
	if err := json.Unmarshal(recorderBody, &payload); err != nil {
		t.Fatalf("decode overrides: %v (%s)", err, recorderBody)
	}
	entry, ok := payload.Overrides[model]
	if !ok {
		t.Fatalf("model %q missing from %v", model, payload.Overrides)
	}
	return entry
}

func TestModelOverridesListEmpty(t *testing.T) {
	handler, _ := newOverrideTestServer(t)
	recorder := doRequest(t, handler, http.MethodGet, "/api/pools/officeace/models/overrides", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		PluginID  string                    `json:"plugin_id"`
		Overrides map[string]map[string]any `json:"overrides"`
	}
	decodeJSONBody(t, recorder, &payload)
	if payload.PluginID != "officeace" || len(payload.Overrides) != 0 {
		t.Fatalf("payload = %+v, want an empty layer for officeace", payload)
	}
}

func TestModelOverridePutPatchAndClear(t *testing.T) {
	handler, st := newOverrideTestServer(t)
	target := "/api/pools/officeace/models/overrides/glm-5"

	// Create: three pinned fields, thinking tiers are trimmed and deduped.
	recorder := doRequest(t, handler, http.MethodPut, target,
		`{"display_name":"  人工名 ","context_length":200000,"vision":true,"thinking":[" low ","high","low"]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var saved struct {
		OK       bool           `json:"ok"`
		Model    string         `json:"model"`
		Override map[string]any `json:"override"`
	}
	decodeJSONBody(t, recorder, &saved)
	if !saved.OK || saved.Model != "glm-5" || saved.Override == nil {
		t.Fatalf("PUT payload = %+v", saved)
	}
	if saved.Override["display_name"] != "人工名" || saved.Override["context_length"] != float64(200000) {
		t.Fatalf("PUT override = %+v", saved.Override)
	}
	levels, _ := saved.Override["thinking"].([]any)
	if len(levels) != 2 || levels[0] != "low" || levels[1] != "high" {
		t.Fatalf("PUT thinking = %v, want [low high]", saved.Override["thinking"])
	}
	if overridden, _ := saved.Override["overridden"].([]any); len(overridden) != 4 {
		t.Fatalf("PUT overridden = %v, want 4 fields", saved.Override["overridden"])
	}

	// Patch: only vendor is sent, everything else must stay.
	recorder = doRequest(t, handler, http.MethodPut, target, `{"vendor":"智谱"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT vendor = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	recorder = doRequest(t, handler, http.MethodGet, "/api/pools/officeace/models/overrides", "")
	entry := decodeOverrideView(t, recorder.Body.Bytes(), "glm-5")
	if entry["display_name"] != "人工名" || entry["vendor"] != "智谱" {
		t.Fatalf("after vendor patch = %+v", entry)
	}
	if entry["multiplier"] != nil || entry["free"] != nil {
		t.Fatalf("unset fields must be null: %+v", entry)
	}
	overridden, _ := entry["overridden"].([]any)
	want := []string{"display_name", "vendor", "context_length", "vision", "thinking"}
	if len(overridden) != len(want) {
		t.Fatalf("overridden = %v, want %v", overridden, want)
	}
	for i, name := range want {
		if overridden[i] != name {
			t.Fatalf("overridden = %v, want %v", overridden, want)
		}
	}

	// Explicit null clears one field only.
	recorder = doRequest(t, handler, http.MethodPut, target, `{"display_name":null}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT null = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	entry = decodeOverrideView(t, doRequest(t, handler, http.MethodGet,
		"/api/pools/officeace/models/overrides", "").Body.Bytes(), "glm-5")
	if entry["display_name"] != nil || entry["vendor"] != "智谱" {
		t.Fatalf("after nulling display_name = %+v", entry)
	}

	// An explicit false is a real override, not a clear.
	recorder = doRequest(t, handler, http.MethodPut, target, `{"free":false}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT free=false = %d", recorder.Code)
	}
	if overrides, err := st.GetModelOverrides("officeace"); err != nil || overrides["glm-5"].Free == nil ||
		*overrides["glm-5"].Free {
		t.Fatalf("stored free = %+v, %v", overrides["glm-5"].Free, err)
	}

	// Clearing every field removes the row.
	recorder = doRequest(t, handler, http.MethodPut, target,
		`{"display_name":null,"vendor":null,"context_length":null,"vision":null,"thinking":null,"multiplier":null,"free":null}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT clear-all = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	var cleared struct {
		Override any `json:"override"`
	}
	decodeJSONBody(t, recorder, &cleared)
	if cleared.Override != nil {
		t.Fatalf("clear-all override = %v, want null", cleared.Override)
	}
	if overrides, _ := st.GetModelOverrides("officeace"); len(overrides) != 0 {
		t.Fatalf("row survived clear-all: %v", overrides)
	}
}

func TestModelOverrideDeleteIsIdempotent(t *testing.T) {
	handler, _ := newOverrideTestServer(t)
	target := "/api/pools/officeace/models/overrides/glm-5"

	if recorder := doRequest(t, handler, http.MethodPut, target, `{"multiplier":0}`); recorder.Code != http.StatusOK {
		t.Fatalf("PUT = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	recorder := doRequest(t, handler, http.MethodDelete, target, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("DELETE = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	var removed struct {
		OK      bool `json:"ok"`
		Removed bool `json:"removed"`
	}
	decodeJSONBody(t, recorder, &removed)
	if !removed.OK || !removed.Removed {
		t.Fatalf("DELETE payload = %+v, want removed", removed)
	}

	recorder = doRequest(t, handler, http.MethodDelete, target, "")
	decodeJSONBody(t, recorder, &removed)
	if !removed.OK || removed.Removed {
		t.Fatalf("second DELETE payload = %+v, want removed=false", removed)
	}
}

func TestModelOverrideValidation(t *testing.T) {
	handler, st := newOverrideTestServer(t)
	base := "/api/pools/officeace/models/overrides/glm-5"
	cases := []struct {
		name string
		body string
	}{
		{"empty object", `{}`},
		{"unknown only", `{"bogus":1}`},
		{"zero context", `{"context_length":0}`},
		{"negative context", `{"context_length":-5}`},
		{"fractional context", `{"context_length":1.5}`},
		{"negative multiplier", `{"multiplier":-0.5}`},
		{"string multiplier", `{"multiplier":"1"}`},
		{"numeric display name", `{"display_name":42}`},
		{"display name too long", `{"display_name":"` + overrideLongString(maxModelOverrideNameRunes+1) + `"}`},
		{"vendor too long", `{"vendor":"` + overrideLongString(maxModelOverrideVendorRunes+1) + `"}`},
		{"thinking not a list", `{"thinking":"low,high"}`},
		{"empty thinking clears", `{"thinking":[]}`},
		{"malformed json", `{"display_name":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := doRequest(t, handler, http.MethodPut, base, tc.body)
			switch tc.name {
			case "empty thinking clears":
				// An empty list is a clear request, which leaves no row: the
				// call is accepted and behaves like a delete.
				if recorder.Code != http.StatusOK {
					t.Fatalf("PUT = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
				}
				return
			case "malformed json":
				// decodeJSON rejects it before validation runs.
				if recorder.Code != http.StatusBadRequest {
					t.Fatalf("PUT = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
				}
				return
			}
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("PUT = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
			}
			var payload errorResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
				t.Fatalf("error body: %v", err)
			}
			if payload.Error.Type != "invalid_request" {
				t.Fatalf("error type = %q, want invalid_request", payload.Error.Type)
			}
		})
	}
	if overrides, err := st.GetModelOverrides("officeace"); err != nil || len(overrides) != 0 {
		t.Fatalf("rejected requests stored rows: %v, %v", overrides, err)
	}

	// Thinking beyond the cap is rejected.
	levels := make([]string, 0, maxModelOverrideLevels+1)
	for i := 0; i <= maxModelOverrideLevels; i++ {
		levels = append(levels, "level"+string(rune('a'+i)))
	}
	body, _ := json.Marshal(map[string]any{"thinking": levels})
	if recorder := doRequest(t, handler, http.MethodPut, base, string(body)); recorder.Code != http.StatusBadRequest {
		t.Fatalf("too many tiers = %d, want 400", recorder.Code)
	}
}

func TestModelOverrideMethodAndEscapedModel(t *testing.T) {
	handler, _ := newOverrideTestServer(t)
	if recorder := doRequest(t, handler, http.MethodPost,
		"/api/pools/officeace/models/overrides/glm-5", "{}"); recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d, want 405", recorder.Code)
	}
	if recorder := doRequest(t, handler, http.MethodPost,
		"/api/pools/officeace/models/overrides", "{}"); recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST list = %d, want 405", recorder.Code)
	}
	if recorder := doRequest(t, handler, http.MethodGet,
		"/api/pools/%20/models/overrides", ""); recorder.Code != http.StatusBadRequest {
		t.Fatalf("empty plugin = %d, want 400", recorder.Code)
	}

	// Model ids are URL-escaped by the SPA; PathValue must hand back the raw id.
	model := "glm 5.3"
	target := "/api/pools/officeace/models/overrides/" + url.PathEscape(model)
	if recorder := doRequest(t, handler, http.MethodPut, target, `{"display_name":"空格模型"}`); recorder.Code != http.StatusOK {
		t.Fatalf("PUT escaped model = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	entry := decodeOverrideView(t, doRequest(t, handler, http.MethodGet,
		"/api/pools/officeace/models/overrides", "").Body.Bytes(), model)
	if entry["display_name"] != "空格模型" {
		t.Fatalf("escaped model entry = %+v", entry)
	}
}

func TestModelOverridesUnavailableWithoutStore(t *testing.T) {
	handler := New(Options{})
	recorder := doRequest(t, handler, http.MethodGet, "/api/pools/officeace/models/overrides", "")
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("GET without store = %d, want 501 (body %s)", recorder.Code, recorder.Body.String())
	}
	recorder = doRequest(t, handler, http.MethodPut, "/api/pools/officeace/models/overrides/glm-5", `{"vendor":"v"}`)
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("PUT without store = %d, want 501", recorder.Code)
	}
}

// overrideLongString builds a string of n runes for the length limits.
func overrideLongString(n int) string {
	out := make([]rune, n)
	for i := range out {
		out[i] = '模'
	}
	return string(out)
}

// errorResponse mirrors the body shape writeError emits.
type errorResponse struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}
