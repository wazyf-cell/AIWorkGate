package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

// TestManifestAndMetaTellTheSameTruth guards the channel's self-description.
//
// This plugin is a forwarder with one synthetic account and no login of its own
// (the assistant holds the credential). Its manifest once advertised checkin and
// quota: checkin/refresh had no implementation at all, and quota has no real
// number to report. The host reads the manifest to decide what to poll and what
// to call, so a capability here is a promise, not a label:
//
//   - checkin/refresh would be called and answer 501;
//   - quota makes the host poll /accounts and treat the absent Remaining=0 as
//     low, raising a false daily "额度不足：aichat" notification.
//
// The /meta list must not drift from the manifest either: the two are the same
// claim read by different consumers.
func TestManifestAndMetaTellTheSameTruth(t *testing.T) {
	raw, err := os.ReadFile("manifest.json")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("parse manifest map: %v", err)
	}

	want := []string{"upstream", "accounts"}
	if strings.Join(manifest.Capabilities, ",") != strings.Join(want, ",") {
		t.Fatalf("manifest capabilities = %v, want %v", manifest.Capabilities, want)
	}
	// No login block: the assistant owns the credential, so the UI must not
	// offer a login button that this plugin cannot honour.
	if _, ok := top["login"]; ok {
		t.Fatal("manifest declares a login block, but this channel has no login endpoint")
	}
	for _, cap := range manifest.Capabilities {
		if strings.HasPrefix(cap, "login_") {
			t.Fatalf("manifest declares %q, but POST /accounts/login is not implemented", cap)
		}
	}

	s := newTestServer(t, "http://127.0.0.1:17864", "sk-test")
	rec := doReq(t, s.handler(), http.MethodGet, "/meta", "", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /meta status = %d, body %s", rec.Code, rec.Body.String())
	}
	var meta struct {
		Capabilities []string `json:"capabilities"`
		AccountCount int      `json:"account_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatalf("decode /meta: %v", err)
	}
	if strings.Join(meta.Capabilities, ",") != strings.Join(manifest.Capabilities, ",") {
		t.Fatalf("/meta capabilities = %v, manifest = %v", meta.Capabilities, manifest.Capabilities)
	}
	if meta.AccountCount != 1 {
		t.Fatalf("/meta account_count = %d, want the one synthetic account", meta.AccountCount)
	}

	// The single account must stay quota-free: an invented number here would
	// either park the account (Remaining<=0 with Total>0) or raise a false alert.
	rec = doReq(t, s.handler(), http.MethodGet, "/accounts", "", testToken)
	var payload struct {
		Accounts []struct {
			ID    string         `json:"id"`
			Quota map[string]any `json:"quota"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode /accounts: %v", err)
	}
	if len(payload.Accounts) != 1 || payload.Accounts[0].ID != upstreamAccountID {
		t.Fatalf("accounts = %+v, want the single %q account", payload.Accounts, upstreamAccountID)
	}
	if len(payload.Accounts[0].Quota) != 0 {
		t.Fatalf("quota must carry no numbers, got %v", payload.Accounts[0].Quota)
	}
}
