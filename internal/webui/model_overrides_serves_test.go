package webui

import (
	"net/http"
	"testing"

	"aiworkgate/internal/store"
)

// D-35 serves(): only a newly set non-empty name is checked, so clearing a name
// or editing unrelated fields on a model the plugin no longer lists still works.
func TestModelOverrideRenameChecksServes(t *testing.T) {
	puts, deletes := 0, 0
	handler := New(Options{
		ModelOverrides: func(string) (map[string]store.ModelOverride, error) {
			return map[string]store.ModelOverride{}, nil
		},
		PutModelOverride: func(string, string, store.ModelOverride) error {
			puts++
			return nil
		},
		DeleteModelOverride: func(string, string) error {
			deletes++
			return nil
		},
		PluginServesModel: func(pluginID, model string) (bool, bool) {
			if pluginID == "p" && model == "ghost" {
				return false, true
			}
			return true, true
		},
	})

	rec := doRequest(t, handler, http.MethodPut, "/api/pools/p/models/overrides/ghost", `{"display_name":"幽灵"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("renaming an unserved model = %d (%s), want 400", rec.Code, rec.Body.String())
	}
	if puts != 0 {
		t.Fatalf("rejected rename reached the store (%d puts)", puts)
	}

	rec = doRequest(t, handler, http.MethodPut, "/api/pools/p/models/overrides/m1", `{"display_name":"我的模型"}`)
	if rec.Code != http.StatusOK || puts != 1 {
		t.Fatalf("served rename = %d, puts=%d", rec.Code, puts)
	}

	rec = doRequest(t, handler, http.MethodPut, "/api/pools/p/models/overrides/ghost", `{"context_length":100000}`)
	if rec.Code != http.StatusOK || puts != 2 {
		t.Fatalf("unrelated edit on unserved model = %d, puts=%d", rec.Code, puts)
	}

	rec = doRequest(t, handler, http.MethodPut, "/api/pools/p/models/overrides/ghost", `{"display_name":null}`)
	if rec.Code != http.StatusOK || deletes != 1 {
		t.Fatalf("clearing a name = %d, deletes=%d", rec.Code, deletes)
	}
}
