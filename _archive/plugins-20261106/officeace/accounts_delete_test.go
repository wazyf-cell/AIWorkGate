package main

import (
	"net/http"
	"strings"
	"testing"
)

// TestAccountDeleteReportsNotSupported: the OfficeAce account is read from
// read-only external state, so a 204 would be a lie: the account reappears on
// the next /accounts read. The contract error must say so instead.
func TestAccountDeleteReportsNotSupported(t *testing.T) {
	s := defaultSession("maas.example.test")
	root := makeInstall(t, s)
	local := newFakeLocal(t)
	_, h, _ := newTestPlugin(t, func(o *Options) {
		o.ForcedOfficeAceHome = installHome(t, root)
		o.LocalAPIURL = local.URL()
	})
	id := accountIDFor(s.userKey())

	rec := doReq(t, h, http.MethodDelete, "/accounts/"+id, reqOpts{})
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("delete status = %d, want 501; body %s", rec.Code, rec.Body.String())
	}
	env := decodeAs[errorEnvelope](t, rec)
	if env.Error.Type != "not_supported" || env.Error.Code != "not_supported" {
		t.Fatalf("error = %+v", env.Error)
	}
	if !strings.Contains(env.Error.Message, "只读") || !strings.Contains(env.Error.Message, "停用") {
		t.Fatalf("message must explain the read-only account and the disable alternative: %q", env.Error.Message)
	}

	// Nothing was removed: the same account is still listed.
	accts := listAccounts(t, h).Accounts
	if len(accts) != 1 || accts[0].ID != id {
		t.Fatalf("account must stay visible: %+v", accts)
	}
}

func TestAccountDeleteUnknownIsStillNotFound(t *testing.T) {
	_, h, _ := newTestPlugin(t, nil)
	rec := doReq(t, h, http.MethodDelete, "/accounts/acc_missing", reqOpts{})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body %s", rec.Code, rec.Body.String())
	}
}
