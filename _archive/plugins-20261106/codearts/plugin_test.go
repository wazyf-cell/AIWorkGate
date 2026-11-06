package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "test-token-123"

type reqOpts struct {
	body    string
	token   string
	noAuth  bool
	headers map[string]string
}

func testEndpoints() Endpoints {
	return Endpoints{
		STS:           "http://127.0.0.1:1/sts",
		Chat:          "http://127.0.0.1:1/chat",
		Queue:         "http://127.0.0.1:1/queue",
		Statistics:    "http://127.0.0.1:1/statistics",
		Delivery:      "http://127.0.0.1:1/delivery",
		Claim:         "http://127.0.0.1:1/claim",
		Confirm:       "http://127.0.0.1:1/confirm",
		Ticket:        "http://127.0.0.1:1/ticket",
		GatewayConfig: "http://127.0.0.1:1/gateway",
		BuiltinModels: "http://127.0.0.1:1/builtin",
		Authorize:     "https://codearts.huaweicloud.com/portal/authorize",
		PortalLogin:   "https://codearts.huaweicloud.com/portal/login",
	}
}

func newTestPlugin(t *testing.T, mutate func(*Options)) (*Plugin, http.Handler) {
	t.Helper()
	o := Options{
		DataDir:           t.TempDir(),
		Token:             testToken,
		LogWriter:         io.Discard,
		DisableBackground: true,
		OpenBrowser:       func(string) error { return nil },
		Endpoints:         testEndpoints(),
		QueueRetryDelay:   5 * time.Millisecond,
		QueueMaxAttempts:  4,
		FirstTokenTimeout: 2 * time.Second,
		IdleTimeout:       2 * time.Second,
	}
	if mutate != nil {
		mutate(&o)
	}
	p, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(p.Close)
	return p, p.Handler()
}

func doReq(t *testing.T, h http.Handler, method, path string, o reqOpts) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if o.body != "" {
		body = strings.NewReader(o.body)
	}
	req := httptest.NewRequest(method, path, body)
	if o.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if !o.noAuth {
		tok := o.token
		if tok == "" {
			tok = testToken
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for k, v := range o.headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeAs[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 20, 9, 0, 0, 0, time.Local)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func seedAccount(t *testing.T, p *Plugin, label string) Account {
	t.Helper()
	key, err := generateDPoPKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	now := p.now()
	cred := Credential{
		AccessKeyID:     "AK-" + label,
		SecretAccessKey: "SK-" + label,
		SecurityToken:   "ST-" + label,
		ExpiresAt:       now.Add(2 * time.Hour).Format(time.RFC3339),
		RefreshToken:    "RT-" + label,
		CodeVerifier:    "verifier-" + label,
		DPoPPrivateJWK:  key.marshalJWK(),
		DomainID:        "domain-" + label,
		UserID:          "uid-" + label,
		UserName:        label,
	}
	acct, err := p.upsertAccount(cred, now)
	if err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	return *acct
}

type accountsEnvelope struct {
	Accounts []accountView `json:"accounts"`
}

type errorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

type healthResp struct {
	OK      bool   `json:"ok"`
	Version string `json:"version"`
	Uptime  int64  `json:"uptime_s"`
}

type metaResp struct {
	ID           string   `json:"id"`
	Version      string   `json:"version"`
	APILevel     int      `json:"api_level"`
	Capabilities []string `json:"capabilities"`
	AccountCount int      `json:"account_count"`
	Uptime       int64    `json:"uptime_s"`
}

type reloadResp = reloadResult

type modelsResp struct {
	Data []ModelInfo `json:"data"`
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func listAccounts(t *testing.T, h http.Handler) accountsEnvelope {
	t.Helper()
	rec := doReq(t, h, http.MethodGet, "/accounts", reqOpts{})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /accounts status = %d, body %s", rec.Code, rec.Body.String())
	}
	return decodeAs[accountsEnvelope](t, rec)
}

func TestHealthNeedsNoTokenButMetaDoes(t *testing.T) {
	_, h := newTestPlugin(t, nil)

	rec := doReq(t, h, http.MethodGet, "/health", reqOpts{noAuth: true})
	if rec.Code != http.StatusOK {
		t.Fatalf("health status = %d", rec.Code)
	}
	if got := decodeAs[healthResp](t, rec); !got.OK || got.Version != Version {
		t.Fatalf("unexpected health payload: %+v", got)
	}

	for _, o := range []reqOpts{{noAuth: true}, {token: "wrong-token"}} {
		rec = doReq(t, h, http.MethodGet, "/meta", o)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("meta with %+v status = %d, want 401", o, rec.Code)
		}
		if env := decodeAs[errorEnvelope](t, rec); env.Error.Type != "unauthorized" {
			t.Fatalf("error type = %q, want unauthorized", env.Error.Type)
		}
	}

	rec = doReq(t, h, http.MethodGet, "/meta", reqOpts{})
	if rec.Code != http.StatusOK {
		t.Fatalf("meta status = %d", rec.Code)
	}
	meta := decodeAs[metaResp](t, rec)
	if meta.ID != "codearts" || meta.Version != Version || meta.APILevel != APIVersion {
		t.Fatalf("unexpected meta: %+v", meta)
	}
	if meta.AccountCount != 0 {
		t.Fatalf("account_count = %d, want 0", meta.AccountCount)
	}
	if strings.Join(meta.Capabilities, ",") != strings.Join(capabilities, ",") {
		t.Fatalf("capabilities = %v", meta.Capabilities)
	}
}

func TestManifestMatchesContract(t *testing.T) {
	b, err := os.ReadFile("manifest.json")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m struct {
		ID           string   `json:"id"`
		Name         string   `json:"name"`
		Version      string   `json:"version"`
		APILevel     int      `json:"api_level"`
		OS           []string `json:"os"`
		Capabilities []string `json:"capabilities"`
		Permissions  struct {
			NetworkDomains []string `json:"network_domains"`
			UseProxy       bool     `json:"use_proxy"`
			OSIdentity     bool     `json:"os_identity"`
		} `json:"permissions"`
		Egress struct {
			Strict bool `json:"strict"`
		} `json:"egress"`
		Models []any `json:"models"`
		Login  struct {
			Methods         []string `json:"methods"`
			ReadsLocalState bool     `json:"reads_local_state"`
			RequiresApp     bool     `json:"requires_app"`
			AutoRefresh     bool     `json:"auto_refresh"`
			Notes           string   `json:"notes"`
		} `json:"login"`
		Checkin struct {
			DefaultTime string `json:"default_time"`
		} `json:"checkin"`
		Quota struct {
			PollMinutes int `json:"poll_minutes"`
		} `json:"quota"`
		Concurrency struct {
			Suggested int `json:"suggested"`
		} `json:"concurrency"`
		Rules struct {
			Packs []string `json:"packs"`
		} `json:"rules"`
		Startup struct {
			Mode string `json:"mode"`
		} `json:"startup"`
		Entry struct {
			Exe string `json:"exe"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if m.ID != "codearts" || m.Name != "CodeArts" || m.Version != Version || m.APILevel != 2 {
		t.Fatalf("identity mismatch: %+v", m)
	}
	wantCaps := []string{"upstream", "accounts", "checkin", "quota", "login_oauth", "refresh", "egress", "identity_request", "login_paste_code", "login_ide_plugin"}
	if strings.Join(m.Capabilities, ",") != strings.Join(wantCaps, ",") {
		t.Fatalf("capabilities = %v", m.Capabilities)
	}
	// /meta reports the same list, and the host's login UI derives its buttons
	// from the manifest. A drift between the two means the plugin tells the host
	// one thing and a human debugging it another (the paste_code button existed
	// in the manifest while /meta denied it).
	if strings.Join(capabilities, ",") != strings.Join(m.Capabilities, ",") {
		t.Fatalf("runtime /meta capabilities = %v, manifest = %v", capabilities, m.Capabilities)
	}
	if !m.Permissions.UseProxy || m.Permissions.OSIdentity || m.Egress.Strict {
		t.Fatalf("permissions/egress mismatch: %+v", m.Permissions)
	}
	for _, host := range []string{"sts.cn-north-4.myhuaweicloud.com", "snap-access.cn-north-4.myhuaweicloud.com", "opengw.developer.huaweicloud.com"} {
		found := false
		for _, d := range m.Permissions.NetworkDomains {
			if d == host {
				found = true
			}
		}
		if !found {
			t.Fatalf("network_domains missing %s: %v", host, m.Permissions.NetworkDomains)
		}
	}
	if m.Models == nil || len(m.Models) != 0 {
		t.Fatalf("models should be an empty dynamic list: %+v", m.Models)
	}
	if strings.Join(m.Login.Methods, ",") != "oauth,paste_code,ide_plugin" || m.Login.ReadsLocalState || m.Login.RequiresApp || !m.Login.AutoRefresh || m.Login.Notes == "" {
		t.Fatalf("login block = %+v", m.Login)
	}
	if m.Checkin.DefaultTime != "09:00" || m.Quota.PollMinutes != 30 || m.Concurrency.Suggested != 2 {
		t.Fatalf("schedule mismatch: %+v", m)
	}
	if m.Rules.Packs == nil || len(m.Rules.Packs) != 0 {
		t.Fatalf("rules packs = %v", m.Rules.Packs)
	}
	if m.Startup.Mode != "always" || m.Entry.Exe != "codearts-plugin.exe" {
		t.Fatalf("startup/entry mismatch: %+v", m)
	}
}

func TestAccountsCRUDAndIdentity(t *testing.T) {
	p, h := newTestPlugin(t, nil)
	acct := seedAccount(t, p, "alice")

	views := listAccounts(t, h).Accounts
	if len(views) != 1 {
		t.Fatalf("accounts = %d, want 1", len(views))
	}
	v := views[0]
	if v.ID != acct.ID || v.Label != "alice" {
		t.Fatalf("account view = %+v", v)
	}
	if !strings.HasPrefix(v.ID, "acc_") || len(v.ID) != 14 {
		t.Fatalf("account id = %q", v.ID)
	}
	if !v.Enabled || v.Status != "ok" {
		t.Fatalf("account state = %+v", v)
	}
	if v.Identity == nil || v.Identity.Source != "derived" || len(v.Identity.SessionID) != 32 {
		t.Fatalf("identity = %+v", v.Identity)
	}

	rec := doReq(t, h, http.MethodPatch, "/accounts/"+acct.ID, reqOpts{body: `{"enabled":false}`})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d, body %s", rec.Code, rec.Body.String())
	}
	views = listAccounts(t, h).Accounts
	if views[0].Enabled {
		t.Fatal("account should be disabled")
	}

	rec = doReq(t, h, http.MethodPost, "/accounts/"+acct.ID+"/identity/reset", reqOpts{body: `{"mode":"request"}`})
	if rec.Code != http.StatusOK {
		t.Fatalf("identity reset status = %d, body %s", rec.Code, rec.Body.String())
	}
	res := decodeAs[struct {
		OK       bool     `json:"ok"`
		Mode     string   `json:"mode"`
		Identity Identity `json:"identity"`
	}](t, rec)
	if !res.OK || res.Identity.SessionID == v.Identity.SessionID || res.Identity.Source != "derived" {
		t.Fatalf("identity reset = %+v (old %s)", res, v.Identity.SessionID)
	}

	rec = doReq(t, h, http.MethodPost, "/accounts/"+acct.ID+"/identity/reset", reqOpts{body: `{"mode":"os"}`})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("os identity status = %d, want 400", rec.Code)
	}
	if env := decodeAs[errorEnvelope](t, rec); env.Error.Code != "unsupported_identity_mode" {
		t.Fatalf("os identity error = %+v", env.Error)
	}

	if rec = doReq(t, h, http.MethodDelete, "/accounts/"+acct.ID, reqOpts{}); rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", rec.Code)
	}
	if n := len(listAccounts(t, h).Accounts); n != 0 {
		t.Fatalf("accounts after delete = %d, want 0", n)
	}
}

func TestDPAPIRoundTripAndNoPlaintextOnDisk(t *testing.T) {
	secret := "SUPER-SECRET-ACCESS-KEY-42"
	enc, err := dpapiProtect([]byte(secret))
	if err != nil {
		t.Fatalf("protect: %v", err)
	}
	if strings.Contains(string(enc), secret) {
		t.Fatal("DPAPI output contains plaintext")
	}
	plain, err := dpapiUnprotect(enc)
	if err != nil {
		t.Fatalf("unprotect: %v", err)
	}
	if string(plain) != secret {
		t.Fatalf("round trip = %q", plain)
	}

	p, _ := newTestPlugin(t, func(o *Options) { o.DataDir = t.TempDir() })
	seedAccount(t, p, "secret-user")
	raw, err := os.ReadFile(filepath.Join(p.DataDir, accountsFileName))
	if err != nil {
		t.Fatalf("read accounts.dat: %v", err)
	}
	for _, needle := range []string{"AK-secret-user", "SK-secret-user", "ST-secret-user", "RT-secret-user", "verifier-secret-user"} {
		if strings.Contains(string(raw), needle) {
			t.Fatalf("accounts.dat contains plaintext %q", needle)
		}
	}
	loaded, err := loadAccounts(p.DataDir)
	if err != nil {
		t.Fatalf("load accounts: %v", err)
	}
	if len(loaded) != 1 || loaded[0].Credential.SecretAccessKey != "SK-secret-user" {
		t.Fatalf("loaded accounts = %+v", loaded)
	}
}

func TestStatePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	o := Options{DataDir: dir, Token: testToken, LogWriter: io.Discard, DisableBackground: true, Endpoints: testEndpoints()}
	p1, err := New(o)
	if err != nil {
		t.Fatalf("New p1: %v", err)
	}
	acct := seedAccount(t, p1, "bob")
	p1.Close()

	p2, err := New(o)
	if err != nil {
		t.Fatalf("New p2: %v", err)
	}
	defer p2.Close()
	views := listAccounts(t, p2.Handler()).Accounts
	if len(views) != 1 || views[0].ID != acct.ID || views[0].Label != "bob" {
		t.Fatalf("accounts after restart = %+v", views)
	}
}

func TestModelsFallbackNeverNeedsNetwork(t *testing.T) {
	_, h := newTestPlugin(t, nil)
	rec := doReq(t, h, http.MethodGet, "/v1/models", reqOpts{})
	if rec.Code != http.StatusOK {
		t.Fatalf("models status = %d", rec.Code)
	}
	resp := decodeAs[modelsResp](t, rec)
	if len(resp.Data) != len(fallbackModelIDs) {
		t.Fatalf("fallback models = %d, want %d", len(resp.Data), len(fallbackModelIDs))
	}
	if resp.Data[0].ID != "GLM-5.2" || resp.Data[0].ContextLength != 202752 || !resp.Data[0].Tools {
		t.Fatalf("first fallback model = %+v", resp.Data[0])
	}
}

func TestExpiredAccountReportsExpired(t *testing.T) {
	p, h := newTestPlugin(t, nil)
	now := p.now()
	cred := Credential{
		AccessKeyID:     "AK-expired",
		SecretAccessKey: "SK-expired",
		SecurityToken:   "ST-expired",
		ExpiresAt:       now.Add(-time.Hour).Format(time.RFC3339),
		DomainID:        "domain-expired",
		UserID:          "uid-expired",
		UserName:        "expired-user",
	}
	acct, err := p.upsertAccount(cred, now)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	views := listAccounts(t, h).Accounts
	if len(views) != 1 || views[0].ID != acct.ID {
		t.Fatalf("accounts = %+v", views)
	}
	if views[0].Status != "expired" {
		t.Fatalf("status = %q, want expired", views[0].Status)
	}
}
