package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestAccountsCredentialPresentTracksTheAPIKey pins what credential.present
// means: the host's scheduler reads it as CredentialPresent and filters an
// account with no credential. A stored base_url without an api_key is not a
// usable credential, so it must be reported as present:false instead of letting
// the scheduler route traffic the plugin cannot attribute to a key.
func TestAccountsCredentialPresentTracksTheAPIKey(t *testing.T) {
	cases := []struct {
		name     string
		apiKey   string
		wantPres bool
	}{
		{name: "missing api key", apiKey: "", wantPres: false},
		{name: "blank api key", apiKey: "   ", wantPres: false},
		{name: "usable api key", apiKey: "sk-assistant", wantPres: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t, "http://127.0.0.1:17864", tc.apiKey)
			rec := doReq(t, s.handler(), http.MethodGet, "/accounts", "", testToken)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
			}
			var payload struct {
				Accounts []struct {
					ID         string `json:"id"`
					Credential *struct {
						Present     bool `json:"present"`
						Refreshable bool `json:"refreshable"`
					} `json:"credential"`
				} `json:"accounts"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatalf("body is not JSON: %v", err)
			}
			if len(payload.Accounts) != 1 || payload.Accounts[0].Credential == nil {
				t.Fatalf("accounts = %+v, want the single account with a credential block", payload.Accounts)
			}
			if got := payload.Accounts[0].Credential.Present; got != tc.wantPres {
				t.Fatalf("credential.present = %v, want %v for api_key %q", got, tc.wantPres, tc.apiKey)
			}
			if payload.Accounts[0].Credential.Refreshable {
				t.Fatal("aichat has no refresh endpoint; refreshable must stay false")
			}
		})
	}
}
