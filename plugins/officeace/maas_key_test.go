package main

import (
	"net/http"
	"testing"
)

// TestMaasAPIKeyOnlyCredentialIsAccepted: OfficeAce may store only
// maas_api_key (no model app key/secret). That is a valid MaaS bearer
// credential, so the account must not be reported invalid and chat must work.
func TestMaasAPIKeyOnlyCredentialIsAccepted(t *testing.T) {
	maas := newFakeMaaS(t, func(w http.ResponseWriter, r *http.Request, body string) {
		if got := r.Header.Get("Authorization"); got != "Bearer maas-key-only" {
			t.Errorf("authorization = %q, want the maas_api_key bearer", got)
		}
		successSSE(w, r, body)
	})
	s := defaultSession(maas.URL())
	s.AppKey = ""
	s.AppSecret = ""
	s.MaasAPIKey = "maas-key-only"
	root := makeInstall(t, s)
	local := newFakeLocal(t)
	p, h, _ := newTestPlugin(t, func(o *Options) {
		o.ForcedOfficeAceHome = installHome(t, root)
		o.LocalAPIURL = local.URL()
	})

	accts := listAccounts(t, h).Accounts
	if len(accts) != 1 {
		t.Fatalf("accounts = %d", len(accts))
	}
	if accts[0].Status != statusOK {
		t.Fatalf("status = %q, last_error = %q", accts[0].Status, accts[0].LastError)
	}
	if accts[0].Credential == nil || !accts[0].Credential.Present {
		t.Fatalf("credential = %+v", accts[0].Credential)
	}

	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{body: sampleChatBody("glm-5.3", "hi", nil)})
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d; body %s", rec.Code, rec.Body.String())
	}
	if resp := decodeAs[chatResp](t, rec); resp.Choices[0].Message.Content != "hello world" {
		t.Fatalf("reply = %+v", resp.Choices[0])
	}
	reqs := maas.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("upstream requests = %d, want 1", len(reqs))
	}
	if reqs[0].Authorization != "Bearer maas-key-only" {
		t.Fatalf("authorization = %q", reqs[0].Authorization)
	}
	_ = p
}

// TestMaasAPIKeyWithoutBaseURLStaysInvalid: the bearer key cannot route
// without the model API base, so the account is still reported as invalid.
func TestMaasAPIKeyWithoutBaseURLStaysInvalid(t *testing.T) {
	s := defaultSession("")
	s.AppKey = ""
	s.AppSecret = ""
	s.MaasAPIKey = "maas-key-only"
	root := makeInstall(t, s)
	_, h, _ := newTestPlugin(t, func(o *Options) { o.ForcedOfficeAceHome = installHome(t, root) })

	accts := listAccounts(t, h).Accounts
	if len(accts) != 1 || accts[0].Status != statusInvalid {
		t.Fatalf("accounts = %+v", accts)
	}
	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{body: sampleChatBody("glm-5.3", "hi", nil)})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("chat status = %d, want 503; body %s", rec.Code, rec.Body.String())
	}
}
