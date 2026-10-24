package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
	Status       string   `json:"status"`
	Message      string   `json:"message"`
	OfficeAce    struct {
		Found  bool   `json:"found"`
		Home   string `json:"home"`
		Source string `json:"source"`
	} `json:"officeace"`
}

type loginStartResp struct {
	SessionID    string       `json:"session_id"`
	Status       string       `json:"status"`
	Method       string       `json:"method"`
	Instructions string       `json:"instructions"`
	AccountID    string       `json:"account_id"`
	Error        string       `json:"error"`
	Fields       []loginField `json:"fields"`
}

type modelsResp struct {
	Data []struct {
		ID            string   `json:"id"`
		Name          string   `json:"name"`
		ContextLength int      `json:"context_length"`
		Tools         *bool    `json:"tools"`
		Vision        *bool    `json:"vision"`
		Multiplier    float64  `json:"multiplier"`
		Vendor        string   `json:"vendor"`
		Free          bool     `json:"free"`
		Thinking      []string `json:"thinking"`
	} `json:"data"`
	Source string `json:"source"`
}

func listAccounts(t *testing.T, h http.Handler) accountsEnvelope {
	t.Helper()
	rec := doReq(t, h, http.MethodGet, "/accounts", reqOpts{})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /accounts status = %d, body %s", rec.Code, rec.Body.String())
	}
	return decodeAs[accountsEnvelope](t, rec)
}

func TestHealthNoTokenAndMetaRequiresBearer(t *testing.T) {
	_, h, _ := newTestPlugin(t, nil)

	rec := doReq(t, h, http.MethodGet, "/health", reqOpts{noAuth: true})
	if rec.Code != http.StatusOK {
		t.Fatalf("health status = %d", rec.Code)
	}
	if got := decodeAs[healthResp](t, rec); !got.OK || got.Version != Version {
		t.Fatalf("health = %+v", got)
	}

	for _, o := range []reqOpts{{noAuth: true}, {token: "wrong"}} {
		rec = doReq(t, h, http.MethodGet, "/meta", o)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("meta status = %d, want 401", rec.Code)
		}
		if env := decodeAs[errorEnvelope](t, rec); env.Error.Type != "unauthorized" {
			t.Fatalf("error = %+v", env.Error)
		}
	}

	rec = doReq(t, h, http.MethodGet, "/meta", reqOpts{})
	meta := decodeAs[metaResp](t, rec)
	if meta.ID != "officeace" || meta.Version != Version || meta.APILevel != APIVersion {
		t.Fatalf("meta = %+v", meta)
	}
	if len(meta.Capabilities) != len(capabilities) || meta.Status != pluginStatusNotFound {
		t.Fatalf("meta = %+v", meta)
	}
	if meta.AccountCount != 0 || meta.OfficeAce.Found {
		t.Fatalf("unexpected discovery state: %+v", meta)
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
			ReadPaths []string `json:"read_paths"`
			UseProxy  bool     `json:"use_proxy"`
			OSID      bool     `json:"os_identity"`
		} `json:"permissions"`
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
	if m.ID != "officeace" || m.Name != "OfficeAce" || m.Version != Version || m.APILevel != 2 {
		t.Fatalf("identity = %+v", m)
	}
	wantCaps := "upstream,accounts,checkin,quota,login_local_state,refresh,egress"
	if strings.Join(m.Capabilities, ",") != wantCaps {
		t.Fatalf("capabilities = %v", m.Capabilities)
	}
	if !m.Permissions.UseProxy || m.Permissions.OSID {
		t.Fatalf("permissions = %+v", m.Permissions)
	}
	joined := strings.Join(m.Permissions.ReadPaths, "|")
	for _, want := range []string{`%PROGRAMFILES%\OfficeAce`, `%LOCALAPPDATA%\Programs\OfficeAce`, `%USERPROFILE%\.office-claw`, `E:\Program\OfficeAce\program\OfficeAce`} {
		if !strings.Contains(joined, want) {
			t.Fatalf("read_paths missing %q: %v", want, m.Permissions.ReadPaths)
		}
	}
	if m.Checkin.DefaultTime != "09:10" || m.Quota.PollMinutes != 30 || m.Concurrency.Suggested != 2 {
		t.Fatalf("schedule = %+v", m)
	}
	if len(m.Models) != 0 || len(m.Rules.Packs) != 0 || m.Startup.Mode != "always" || m.Entry.Exe != "officeace-plugin.exe" {
		t.Fatalf("manifest tail = %+v", m)
	}
	if strings.Join(m.Login.Methods, ",") != "local_state" || !m.Login.ReadsLocalState || m.Login.RequiresApp || m.Login.AutoRefresh || m.Login.Notes == "" {
		t.Fatalf("login block = %+v", m.Login)
	}
}

func TestLocalStateLoginSuccessAndAccounts(t *testing.T) {
	s := defaultSession("maas.example.test")
	root := makeInstall(t, s)
	local := newFakeLocal(t)
	p, h, _ := newTestPlugin(t, func(o *Options) {
		o.ForcedOfficeAceHome = installHome(t, root)
		o.LocalAPIURL = local.URL()
	})

	rec := doReq(t, h, http.MethodPost, "/accounts/login", reqOpts{body: `{"method":"local_state"}`})
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d body %s", rec.Code, rec.Body.String())
	}
	login := decodeAs[loginStartResp](t, rec)
	wantID := accountIDFor(s.userKey())
	if login.Status != "success" || login.AccountID != wantID {
		t.Fatalf("login = %+v", login)
	}
	if login.Instructions != "" || len(login.Fields) != 0 {
		t.Fatalf("login extras = %+v", login)
	}

	accts := listAccounts(t, h).Accounts
	if len(accts) != 1 {
		t.Fatalf("accounts = %d", len(accts))
	}
	a := accts[0]
	if a.ID != wantID || a.Label != "hid_testuser" || !a.Enabled || a.Status != statusOK {
		t.Fatalf("account = %+v", a)
	}
	if a.Quota == nil || a.Quota.Remaining != 3800 || a.Quota.Total != 5000 || a.Quota.TodayCheckedIn {
		t.Fatalf("quota = %+v", a.Quota)
	}
	if a.Quota.ExpiresAt != fixedNow.Add(30*24*time.Hour).Unix() {
		t.Fatalf("expires_at = %d", a.Quota.ExpiresAt)
	}
	auths := local.authSnapshot()
	if len(auths) == 0 || auths[0] != "Bearer "+s.SessionID {
		t.Fatalf("local auth = %v", auths)
	}
	if p.accountCount() != 1 {
		t.Fatalf("state accounts = %d", p.accountCount())
	}
}

func TestLoginPendingThenSubmitHome(t *testing.T) {
	s := defaultSession("maas.example.test")
	root := makeInstall(t, s)
	empty := ""
	p, h, _ := newTestPlugin(t, func(o *Options) {
		o.ForcedOfficeAceHome = &empty
	})

	rec := doReq(t, h, http.MethodPost, "/accounts/login", reqOpts{body: `{"method":"local_state"}`})
	login := decodeAs[loginStartResp](t, rec)
	if login.Status != "pending" || login.Instructions == "" {
		t.Fatalf("login = %+v", login)
	}
	if len(login.Fields) != 1 || login.Fields[0].Name != "officeace_home" {
		t.Fatalf("fields = %+v", login.Fields)
	}

	// Invalid home keeps the session pending with a clear error.
	rec = doReq(t, h, http.MethodPost, "/accounts/login/"+login.SessionID+"/submit",
		reqOpts{body: `{"values":{"officeace_home":"` + strings.ReplaceAll(t.TempDir(), `\`, `\\`) + `"}}`})
	bad := decodeAs[loginStartResp](t, rec)
	if bad.Status != "pending" || !strings.Contains(bad.Error, "storage.sqlite") {
		t.Fatalf("invalid home = %+v", bad)
	}

	// A valid home is persisted to the plugin data dir and completes login.
	rec = doReq(t, h, http.MethodPost, "/accounts/login/"+login.SessionID+"/submit",
		reqOpts{body: `{"values":{"officeace_home":"` + strings.ReplaceAll(root, `\`, `\\`) + `"}}`})
	ok := decodeAs[loginStartResp](t, rec)
	if ok.Status != "success" || ok.AccountID == "" {
		t.Fatalf("submit = %+v", ok)
	}
	b, err := os.ReadFile(filepath.Join(p.DataDir, configFileName))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil || cfg.OfficeAceHome != root {
		t.Fatalf("config = %q (%v)", string(b), err)
	}

	rec = doReq(t, h, http.MethodGet, "/accounts/login/"+login.SessionID, reqOpts{})
	poll := decodeAs[loginStartResp](t, rec)
	if poll.Status != "success" || poll.AccountID != ok.AccountID {
		t.Fatalf("poll = %+v", poll)
	}
	if rec = doReq(t, h, http.MethodDelete, "/accounts/login/"+login.SessionID, reqOpts{}); rec.Code != http.StatusNoContent {
		t.Fatalf("cancel status = %d", rec.Code)
	}
}

func TestLoginUnsupportedMethodAndCancel(t *testing.T) {
	_, h, _ := newTestPlugin(t, nil)
	rec := doReq(t, h, http.MethodPost, "/accounts/login", reqOpts{body: `{"method":"api_key"}`})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
	rec = doReq(t, h, http.MethodGet, "/accounts/login/nope", reqOpts{})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestAccountsWhenOfficeAceNotRunning(t *testing.T) {
	s := defaultSession("maas.example.test")
	root := makeInstall(t, s)
	_, h, _ := newTestPlugin(t, func(o *Options) {
		o.ForcedOfficeAceHome = installHome(t, root)
	})
	accts := listAccounts(t, h).Accounts
	if len(accts) != 1 {
		t.Fatalf("accounts = %d", len(accts))
	}
	a := accts[0]
	if a.Status != statusOK {
		t.Fatalf("status = %q", a.Status)
	}
	if a.Quota != nil {
		t.Fatalf("quota should be omitted when the service is down: %+v", a.Quota)
	}
	if !strings.Contains(a.LastError, "OfficeAce 未运行") {
		t.Fatalf("last_error = %q", a.LastError)
	}
}

func TestAccounts401MarksSessionExpired(t *testing.T) {
	s := defaultSession("maas.example.test")
	root := makeInstall(t, s)
	local := newFakeLocal(t)
	local.setSubStatus(http.StatusUnauthorized)
	_, h, _ := newTestPlugin(t, func(o *Options) {
		o.ForcedOfficeAceHome = installHome(t, root)
		o.LocalAPIURL = local.URL()
	})
	a := listAccounts(t, h).Accounts[0]
	if a.Status != statusExpired || !strings.Contains(a.LastError, "重新登录") {
		t.Fatalf("account = %+v", a)
	}
	if a.Quota != nil {
		t.Fatalf("quota = %+v", a.Quota)
	}
}

func TestAccountsServiceErrorOmitsQuota(t *testing.T) {
	s := defaultSession("maas.example.test")
	root := makeInstall(t, s)
	local := newFakeLocal(t)
	local.setSubStatus(http.StatusInternalServerError)
	_, h, _ := newTestPlugin(t, func(o *Options) {
		o.ForcedOfficeAceHome = installHome(t, root)
		o.LocalAPIURL = local.URL()
	})
	a := listAccounts(t, h).Accounts[0]
	if a.Quota != nil {
		t.Fatalf("quota should be omitted on 5xx: %+v", a.Quota)
	}
	if !strings.Contains(a.LastError, "本机 API") {
		t.Fatalf("last_error = %q", a.LastError)
	}
	if a.Status != statusOK {
		t.Fatalf("status = %q, want session-derived ok", a.Status)
	}
}

func TestLoginRefreshExpiredSession(t *testing.T) {
	s := defaultSession("maas.example.test")
	s.STSExpiresAt = fixedNow.Add(-time.Hour).Format(time.RFC3339)
	s.SessionExpiresAt = s.STSExpiresAt
	root := makeInstall(t, s)
	iam := newFakeIAM(t, "sts-login-renewed")
	p, h, _ := newTestPlugin(t, func(o *Options) {
		o.ForcedOfficeAceHome = installHome(t, root)
		o.IAMURL = iam.URL()
	})
	p.cfgMu.Lock()
	p.cfg.AutoRefresh = true
	p.cfgMu.Unlock()
	rec := doReq(t, h, http.MethodPost, "/accounts/login", reqOpts{body: `{"method":"local_state"}`})
	login := decodeAs[loginStartResp](t, rec)
	if login.Status != "success" || login.AccountID != accountIDFor(s.userKey()) {
		t.Fatalf("login = %+v", login)
	}
	if iam.callCount() != 1 {
		t.Fatalf("IAM calls = %d", iam.callCount())
	}
	if a, _ := p.findAccount(login.AccountID); a.Status != statusOK {
		t.Fatalf("account = %+v", a)
	}
}

func TestCheckinOkAlreadyFailAndDown(t *testing.T) {
	s := defaultSession("maas.example.test")
	root := makeInstall(t, s)
	local := newFakeLocal(t)
	local.onSub = func(n int) (int, string) {
		if n == 1 {
			return 200, subscriptionJSON(5000, 1200, fixedNow.Add(30*24*time.Hour))
		}
		return 200, subscriptionJSON(5000, 1100, fixedNow.Add(30*24*time.Hour))
	}
	p, h, _ := newTestPlugin(t, func(o *Options) {
		o.ForcedOfficeAceHome = installHome(t, root)
		o.LocalAPIURL = local.URL()
	})
	id := accountIDFor(s.userKey())

	rec := doReq(t, h, http.MethodPost, "/accounts/"+id+"/checkin", reqOpts{})
	if rec.Code != http.StatusOK {
		t.Fatalf("checkin status = %d body %s", rec.Code, rec.Body.String())
	}
	ok := decodeAs[checkinResult](t, rec)
	if ok.Result != "ok" || ok.CreditsBefore == nil || ok.CreditsAfter == nil {
		t.Fatalf("checkin = %+v", ok)
	}
	if *ok.CreditsBefore != 3800 || *ok.CreditsAfter != 3900 || ok.Message != "+100" {
		t.Fatalf("checkin credits = %+v", ok)
	}
	a := listAccounts(t, h).Accounts[0]
	if a.Quota == nil || !a.Quota.TodayCheckedIn || a.Quota.CheckedInAt == 0 || a.Quota.Remaining != 3900 {
		t.Fatalf("quota after checkin = %+v", a.Quota)
	}

	// Already claimed: no balance change, empty sku list.
	local.onSub = nil
	local.setSubscription(5000, 1100, fixedNow.Add(30*24*time.Hour))
	local.setClaim(200, `{"bonusSkus":[]}`)
	rec = doReq(t, h, http.MethodPost, "/accounts/"+id+"/checkin", reqOpts{})
	already := decodeAs[checkinResult](t, rec)
	if already.Result != "already" {
		t.Fatalf("second checkin = %+v", already)
	}
	if *already.CreditsBefore != 3900 || *already.CreditsAfter != 3900 {
		t.Fatalf("already credits = %+v", already)
	}

	// Server error -> fail and do not fabricate a check-in.
	local.setClaim(500, `{"error":"boom"}`)
	rec = doReq(t, h, http.MethodPost, "/accounts/"+id+"/checkin", reqOpts{})
	failed := decodeAs[checkinResult](t, rec)
	if failed.Result != "fail" || !strings.Contains(failed.Message, "领取失败") {
		t.Fatalf("failed checkin = %+v", failed)
	}
	if failed.CreditsAfter != nil {
		t.Fatalf("failed checkin must omit after credits: %+v", failed)
	}

	// 4xx is also a failure (not "already").
	local.setClaim(400, `{"error":"bad request"}`)
	rec = doReq(t, h, http.MethodPost, "/accounts/"+id+"/checkin", reqOpts{})
	if bad := decodeAs[checkinResult](t, rec); bad.Result != "fail" {
		t.Fatalf("4xx checkin = %+v", bad)
	}

	// Service down.
	_, h2, _ := newTestPlugin(t, func(o *Options) { o.ForcedOfficeAceHome = installHome(t, root) })
	rec = doReq(t, h2, http.MethodPost, "/accounts/"+id+"/checkin", reqOpts{})
	down := decodeAs[checkinResult](t, rec)
	if down.Result != "fail" || !strings.Contains(down.Message, "OfficeAce 未运行") {
		t.Fatalf("down checkin = %+v", down)
	}
	_ = p
}

func TestPatchDisableAndHistory(t *testing.T) {
	s := defaultSession("maas.example.test")
	root := makeInstall(t, s)
	local := newFakeLocal(t)
	_, h, _ := newTestPlugin(t, func(o *Options) {
		o.ForcedOfficeAceHome = installHome(t, root)
		o.LocalAPIURL = local.URL()
	})
	id := accountIDFor(s.userKey())

	rec := doReq(t, h, http.MethodPatch, "/accounts/"+id, reqOpts{body: `{"enabled":false}`})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d body %s", rec.Code, rec.Body.String())
	}
	if a := listAccounts(t, h).Accounts[0]; a.Enabled {
		t.Fatal("account should be disabled")
	}
	rec = doReq(t, h, http.MethodPost, "/accounts/"+id+"/checkin", reqOpts{})
	res := decodeAs[checkinResult](t, rec)
	if res.Result != "fail" || res.Message != "账号已停用" {
		t.Fatalf("disabled checkin = %+v", res)
	}

	// Re-enable and check that history reports only real bookkeeping.
	doReq(t, h, http.MethodPatch, "/accounts/"+id, reqOpts{body: `{"enabled":true}`})
	doReq(t, h, http.MethodPost, "/accounts/"+id+"/checkin", reqOpts{})
	rec = doReq(t, h, http.MethodGet, "/accounts/"+id+"/history?days=7", reqOpts{})
	if rec.Code != http.StatusOK {
		t.Fatalf("history status = %d", rec.Code)
	}
	var hist struct {
		Records []historyRecord `json:"records"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &hist); err != nil {
		t.Fatalf("history decode: %v", err)
	}
	if len(hist.Records) != 1 || hist.Records[0].Result == "" {
		t.Fatalf("history = %+v", hist.Records)
	}
}
