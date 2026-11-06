package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestEgressStrictDirectRejectsChatAndCheckin(t *testing.T) {
	s := defaultSession("maas.invalid")
	root := makeInstall(t, s)
	local := newFakeLocal(t)
	egressPath := filepath.Join(t.TempDir(), "egress.json")
	writeTestFile(t, egressPath, egressJSON(true, nil))
	_, h, _ := newTestPlugin(t, func(o *Options) {
		o.ForcedOfficeAceHome = installHome(t, root)
		o.LocalAPIURL = local.URL()
		o.EgressFile = egressPath
	})
	id := accountIDFor(s.userKey())

	rec := doReq(t, h, http.MethodPost, "/accounts/"+id+"/checkin", reqOpts{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("checkin status = %d body %s", rec.Code, rec.Body.String())
	}
	if env := decodeAs[errorEnvelope](t, rec); env.Error.Type != "egress_missing" {
		t.Fatalf("error = %+v", env.Error)
	}
	if local.claimCall != 0 {
		t.Fatal("claim must not run when egress is missing")
	}

	rec = doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{body: sampleChatBody("glm-5.3", "hi", nil)})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("chat status = %d body %s", rec.Code, rec.Body.String())
	}
	if env := decodeAs[errorEnvelope](t, rec); env.Error.Type != "egress_missing" {
		t.Fatalf("error = %+v", env.Error)
	}
}

func TestEgressBlockedAccountRejected(t *testing.T) {
	s := defaultSession("maas.invalid")
	root := makeInstall(t, s)
	id := accountIDFor(s.userKey())
	egressPath := filepath.Join(t.TempDir(), "egress.json")
	writeTestFile(t, egressPath, egressJSON(false, map[string]map[string]string{
		id: {"mode": "blocked"},
	}))
	_, h, _ := newTestPlugin(t, func(o *Options) {
		o.ForcedOfficeAceHome = installHome(t, root)
		o.EgressFile = egressPath
	})
	rec := doReq(t, h, http.MethodPost, "/accounts/"+id+"/checkin", reqOpts{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
}

func TestEgressProxyUsedForMaaSButNotForLocalAPI(t *testing.T) {
	s := defaultSession("http://maas.invalid")
	root := makeInstall(t, s)
	local := newFakeLocal(t)
	proxy := newFakeProxy(t)
	id := accountIDFor(s.userKey())
	egressPath := filepath.Join(t.TempDir(), "egress.json")
	writeTestFile(t, egressPath, egressJSON(false, map[string]map[string]string{
		id: {"mode": "proxy", "url": proxy.URL(), "proxy_id": "px_1"},
	}))
	_, h, _ := newTestPlugin(t, func(o *Options) {
		o.ForcedOfficeAceHome = installHome(t, root)
		o.LocalAPIURL = local.URL()
		o.EgressFile = egressPath
	})

	// Local API (loopback) must bypass the proxy even for a proxy-bound account.
	accts := listAccounts(t, h).Accounts
	if len(accts) != 1 || accts[0].Quota == nil {
		t.Fatalf("accounts = %+v", accts)
	}
	if proxy.count() != 0 {
		t.Fatalf("proxy used for local API: %d", proxy.count())
	}
	if auths := local.authSnapshot(); len(auths) == 0 || auths[0] != "Bearer "+s.SessionID {
		t.Fatalf("local auth = %v", auths)
	}

	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{body: sampleChatBody("glm-5.3", "hi", nil)})
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d body %s", rec.Code, rec.Body.String())
	}
	resp := decodeAs[chatResp](t, rec)
	if resp.Choices[0].Message.Content != "via proxy" {
		t.Fatalf("reply = %+v", resp.Choices[0])
	}
	if proxy.count() != 1 {
		t.Fatalf("proxy hits = %d, want 1", proxy.count())
	}
	proxy.mu.Lock()
	url := proxy.urls[0]
	proxy.mu.Unlock()
	if !strings.Contains(url, "maas.invalid/v2/chat/completions") {
		t.Fatalf("proxy target = %q", url)
	}
	if proxy.count() != 1 {
		t.Fatalf("local API should not add proxy hits: %d", proxy.count())
	}
}

func TestReloadAppliesEgressChanges(t *testing.T) {
	s := defaultSession("http://maas.invalid")
	root := makeInstall(t, s)
	proxy := newFakeProxy(t)
	id := accountIDFor(s.userKey())
	egressPath := filepath.Join(t.TempDir(), "egress.json")
	writeTestFile(t, egressPath, egressJSON(true, nil))
	_, h, _ := newTestPlugin(t, func(o *Options) {
		o.ForcedOfficeAceHome = installHome(t, root)
		o.EgressFile = egressPath
	})

	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{body: sampleChatBody("glm-5.3", "hi", nil)})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("strict chat status = %d body %s", rec.Code, rec.Body.String())
	}

	writeTestFile(t, egressPath, egressJSON(false, map[string]map[string]string{
		id: {"mode": "proxy", "url": proxy.URL(), "proxy_id": "px_1"},
	}))
	rec = doReq(t, h, http.MethodPost, "/reload", reqOpts{})
	if rec.Code != http.StatusOK {
		t.Fatalf("reload status = %d", rec.Code)
	}
	var res struct {
		OK             bool `json:"ok"`
		EgressAccounts int  `json:"egress_accounts"`
		Strict         bool `json:"strict"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !res.OK || res.EgressAccounts != 1 || res.Strict {
		t.Fatalf("reload = %+v", res)
	}

	rec = doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{body: sampleChatBody("glm-5.3", "hi", nil)})
	if rec.Code != http.StatusOK {
		t.Fatalf("chat after reload status = %d body %s", rec.Code, rec.Body.String())
	}
	if proxy.count() != 1 {
		t.Fatalf("proxy hits = %d", proxy.count())
	}
}

// TestTransportForDirectHasNilProxy: egress mode "direct" must not follow the
// process HTTP(S)_PROXY environment. Only an explicit egress binding may.
func TestTransportForDirectHasNilProxy(t *testing.T) {
	p, _, _ := newTestPlugin(t, nil)
	tr := p.transportFor("acc_direct", "")
	if tr.Proxy != nil {
		t.Fatal("direct transport has a Proxy function; it must be nil so HTTP_PROXY/HTTPS_PROXY are ignored")
	}
}

// TestTransportForProxyModeUsesEgressURL pins proxy mode to the egress URL.
func TestTransportForProxyModeUsesEgressURL(t *testing.T) {
	p, _, _ := newTestPlugin(t, nil)
	const proxyURL = "http://127.0.0.1:3128"
	tr := p.transportFor("acc_proxy", proxyURL)
	if tr.Proxy == nil {
		t.Fatal("proxy transport has no Proxy function")
	}
	req, err := http.NewRequest(http.MethodGet, "https://maas.example.test/v2/chat/completions", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	got, err := tr.Proxy(req)
	if err != nil || got == nil || got.String() != proxyURL {
		t.Fatalf("proxy = %v (%v), want %s", got, err, proxyURL)
	}
}
