package main

// M2 guard: the manifest's capability list must equal what /meta advertises and
// what the routes implement. Raccoon exposes no balance endpoint, so `quota`
// must not be declared: the host would poll /accounts and treat the unreported
// Remaining=0 as depleted.

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
)

func TestRaccoonManifestCapabilitiesMatchMeta(t *testing.T) {
	raw, err := os.ReadFile("manifest.json")
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	var mf struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &mf); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	got := append([]string(nil), capabilities...)
	want := append([]string(nil), mf.Capabilities...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("capabilities = %v, manifest = %v", got, want)
	}
	for _, c := range append(got, want...) {
		if c == "quota" {
			t.Fatalf("capabilities = %v: quota is declared but /accounts has no quota source", got)
		}
	}
	// The quota block only declares a poll interval; it is not a capability.
	var blocks struct {
		Quota *struct {
			PollMinutes int `json:"poll_minutes"`
		} `json:"quota"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		t.Fatalf("parse manifest quota block: %v", err)
	}
	if blocks.Quota == nil {
		t.Fatal("manifest quota block disappeared; only the capability should be removed")
	}

	st, err := newStore(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	srv := &server{cfg: pluginConfig{ID: "raccoon", APILevel: 2, Token: "tok"}, store: st, client: newUpstreamClient()}

	rec := doReq(t, srv.routes(), "/meta")
	if rec.Code != http.StatusOK {
		t.Fatalf("/meta = %d, body %s", rec.Code, rec.Body.String())
	}
	var meta struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatalf("decode /meta: %v (%s)", err, rec.Body.String())
	}
	if strings.Join(meta.Capabilities, ",") != strings.Join(mf.Capabilities, ",") {
		t.Fatalf("/meta capabilities = %v, manifest = %v", meta.Capabilities, mf.Capabilities)
	}

	if err := st.set(credentials{AccessToken: "a"}); err != nil {
		t.Fatalf("set credential: %v", err)
	}
	rec = doReq(t, srv.routes(), "/accounts")
	if rec.Code != http.StatusOK {
		t.Fatalf("/accounts = %d, body %s", rec.Code, rec.Body.String())
	}
	var accounts struct {
		Accounts []struct {
			Quota map[string]any `json:"quota"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &accounts); err != nil {
		t.Fatalf("decode /accounts: %v (%s)", err, rec.Body.String())
	}
	if len(accounts.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(accounts.Accounts))
	}
	if q := accounts.Accounts[0].Quota; len(q) != 0 {
		t.Fatalf("quota = %v, want empty (unknown): there is no balance endpoint", q)
	}
}
