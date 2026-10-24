package main

// M2 guard: the manifest's capability list must equal what the plugin really
// implements, and /meta must report that same list. A capability that only
// exists on one side is a dead UI control (or a hidden feature); this test is
// where "declared quota with no quota source" fails instead of shipping.

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
)

func TestManifestCapabilitiesMatchRuntime(t *testing.T) {
	raw, err := os.ReadFile("manifest.json")
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	var mf struct {
		Capabilities []string       `json:"capabilities"`
		Quota        map[string]any `json:"quota"`
	}
	if err := json.Unmarshal(raw, &mf); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}

	got := append([]string(nil), capabilities...)
	want := append([]string(nil), mf.Capabilities...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("runtime capabilities = %v, manifest = %v", got, want)
	}

	// quota is declared nowhere because there is no quota source: /accounts
	// returns an empty quota and nothing ever fills it.
	if containsString(capabilities, "quota") || containsString(mf.Capabilities, "quota") {
		t.Fatalf("quota declared without a source: runtime %v, manifest %v", capabilities, mf.Capabilities)
	}
	if _, ok := mf.Quota["poll_minutes"]; ok {
		t.Fatalf("manifest still configures quota polling: %v", mf.Quota)
	}

	// The identity endpoints exist, so the capability must be declared or the
	// UI hides a working feature.
	if !containsString(capabilities, "identity_request") {
		t.Fatalf("identity_request missing from %v", capabilities)
	}

	// /meta reports the hardcoded list; it must not drift from the manifest.
	_, handler := newAccountTestServer(t)
	rec := accountRequest(t, handler, http.MethodGet, "/meta", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /meta = %d %s", rec.Code, rec.Body.String())
	}
	var meta struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatalf("decode meta: %v %s", err, rec.Body.String())
	}
	metaCaps := append([]string(nil), meta.Capabilities...)
	sort.Strings(metaCaps)
	if strings.Join(metaCaps, ",") != strings.Join(want, ",") {
		t.Fatalf("/meta capabilities = %v, manifest = %v", meta.Capabilities, mf.Capabilities)
	}
}
