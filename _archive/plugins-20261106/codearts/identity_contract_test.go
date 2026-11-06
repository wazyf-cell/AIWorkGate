package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

// hostIdentity mirrors the field names the host decodes for an account's
// identity object (internal/model/model.go: Identity). The host builds
// POST /api/accounts/identity/details by reading GET /accounts and masking the
// values it finds here (internal/app/webui_handlers.go accountIdentityDetails),
// so a plugin that names the fields differently silently shows an empty panel.
//
// Decoding into this struct — rather than into the plugin's own Identity type —
// is the point of the test: a renamed json tag in production still round-trips
// through the plugin's type, but stops appearing here.
type hostIdentity struct {
	MachineID string `json:"machine_id"`
	DeviceID  string `json:"device_id"`
	SessionID string `json:"session_id"`
	Source    string `json:"source"`
	UpdatedAt int64  `json:"updated_at"`
}

type hostAccount struct {
	ID       string        `json:"id"`
	Label    string        `json:"label"`
	Enabled  bool          `json:"enabled"`
	Status   string        `json:"status"`
	Identity *hostIdentity `json:"identity"`
}

type hostAccountsEnvelope struct {
	Accounts []hostAccount `json:"accounts"`
}

func getHostAccounts(t *testing.T, h http.Handler) hostAccountsEnvelope {
	t.Helper()
	rec := doReq(t, h, http.MethodGet, "/accounts", reqOpts{})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /accounts status = %d, body %s", rec.Code, rec.Body.String())
	}
	var env hostAccountsEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode GET /accounts: %v (body %s)", err, rec.Body.String())
	}
	return env
}

// TestIdentityContractVisibleToHost proves the two facts the host depends on:
//
//   - GET /accounts carries an identity object under the host's field names, so
//     POST /api/accounts/identity/details has something real to mask and show;
//   - POST /accounts/{id}/identity/reset rotates that value and the rotation is
//     persisted, so the next GET /accounts (which is what details reads) returns
//     the new identity rather than the old one.
//
// The account is a synthetic fixture created in a t.TempDir() data dir through
// upsertAccount; no runtime account or network call is involved.
func TestIdentityContractVisibleToHost(t *testing.T) {
	p, h := newTestPlugin(t, nil)
	acct := seedAccount(t, p, "identity-contract")

	env := getHostAccounts(t, h)
	if len(env.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(env.Accounts))
	}
	before := env.Accounts[0]
	if before.ID != acct.ID {
		t.Fatalf("account id = %q, want %q", before.ID, acct.ID)
	}
	if before.Identity == nil {
		t.Fatal("GET /accounts has no identity object; host identity/details would be empty")
	}
	if len(before.Identity.SessionID) != 32 {
		t.Fatalf("identity.session_id = %q, want 32 hex chars", before.Identity.SessionID)
	}
	if before.Identity.Source != "derived" || before.Identity.UpdatedAt <= 0 {
		t.Fatalf("identity = %+v, want source=derived and a positive updated_at", *before.Identity)
	}
	// CodeArts has no machine/device header, so the plugin must not invent ids
	// the upstream would ignore. The host renders these two fields when present.
	if before.Identity.MachineID != "" || before.Identity.DeviceID != "" {
		t.Fatalf("identity must not invent machine/device ids: %+v", *before.Identity)
	}

	rec := doReq(t, h, http.MethodPost, "/accounts/"+acct.ID+"/identity/reset", reqOpts{body: `{"mode":"request"}`})
	if rec.Code != http.StatusOK {
		t.Fatalf("identity reset status = %d, body %s", rec.Code, rec.Body.String())
	}
	var reset struct {
		OK       bool          `json:"ok"`
		Mode     string        `json:"mode"`
		Identity *hostIdentity `json:"identity"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reset); err != nil {
		t.Fatalf("decode reset: %v", err)
	}
	if !reset.OK || reset.Mode != "request" || reset.Identity == nil {
		t.Fatalf("reset response = %+v", reset)
	}
	if reset.Identity.SessionID == before.Identity.SessionID {
		t.Fatalf("identity reset did not rotate session_id (%q)", reset.Identity.SessionID)
	}

	// The reset must survive a save/load cycle through the same handler: details
	// reads GET /accounts, not the reset response.
	after := getHostAccounts(t, h).Accounts[0]
	if after.Identity == nil {
		t.Fatal("GET /accounts lost the identity after reset")
	}
	if after.Identity.SessionID != reset.Identity.SessionID {
		t.Fatalf("GET /accounts session_id = %q, reset returned %q (rotation not persisted)",
			after.Identity.SessionID, reset.Identity.SessionID)
	}
	if after.Identity.SessionID == before.Identity.SessionID {
		t.Fatal("GET /accounts still reports the pre-reset session_id")
	}
}
