package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestTicketPollUsesAuthorizePluginIdentity covers the legacy secret fallback of
// the portal login: when the portal answers the callback with `secret=...`
// instead of `code=...`, the plugin polls the Snap ticket endpoint. That ticket
// was minted by a request that named itself snap_AIIDE/5.2.0 (the same identity
// as the authorize URL), so the poll must present the same plugin name/version.
//
// The reference implementation makes this explicit: its portal callback polls
// with LOGIN_PLUGIN_NAME/LOGIN_PLUGIN_VERSION, while only the separate legacy
// doer flow uses snap_jetbrains/26.3.3. Polling a portal ticket as the JetBrains
// plugin is a porting slip the endpoint cannot detect for us without a real
// Huawei account, so it is pinned here against a fake ticket server.
func TestTicketPollUsesAuthorizePluginIdentity(t *testing.T) {
	var mu sync.Mutex
	var gotName, gotVersion string
	ticket := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotName = r.Header.Get("plugin-name")
		gotVersion = r.Header.Get("plugin-version")
		mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"credential": map[string]any{
				"access":        "AK-ticket",
				"secret":        "SK-ticket",
				"securitytoken": "ST-ticket",
				"expires_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			},
			"domain_id": "domain-ticket",
			"user_id":   "user-ticket",
			"user_name": "Ticket User",
		})
	}))
	defer ticket.Close()

	ep := testEndpoints()
	ep.Ticket = ticket.URL
	_, h := newTestPlugin(t, func(o *Options) { o.Endpoints = ep })

	start := decodeAs[loginStartResp](t, doReq(t, h, http.MethodPost, "/accounts/login", reqOpts{body: `{"method":"paste_code"}`}))
	u, err := url.Parse(start.URL)
	if err != nil {
		t.Fatalf("authorize url: %v", err)
	}
	callback := "http://127.0.0.1:" + u.Query().Get("port") + oauthRedirectPath + "?secret=test-secret"
	rec := doReq(t, h, http.MethodPost, "/accounts/login/"+start.SessionID+"/submit", reqOpts{
		body: submitBodyWithCode(t, callback),
	})
	var sub loginStatusResp
	if err := json.Unmarshal(rec.Body.Bytes(), &sub); err != nil {
		t.Fatalf("decode submit: %v (body %s)", err, rec.Body.String())
	}
	if sub.Status != "success" || !strings.HasPrefix(sub.AccountID, "acc_") {
		t.Fatalf("secret fallback = %+v (body %s)", sub, rec.Body.String())
	}

	mu.Lock()
	name, version := gotName, gotVersion
	mu.Unlock()
	if name != loginPluginName || version != loginPluginVersion {
		t.Fatalf("ticket poll headers plugin-name=%q plugin-version=%q, want %q/%q (the identity named in the authorize URL)",
			name, version, loginPluginName, loginPluginVersion)
	}

	accs := listAccounts(t, h).Accounts
	if len(accs) != 1 || accs[0].ID != sub.AccountID || accs[0].Label != "Ticket User" {
		t.Fatalf("accounts after secret fallback = %+v", accs)
	}
}
