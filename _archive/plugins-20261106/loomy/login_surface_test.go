package main

// Login-surface tests. What matters to the user is that the management page
// shows a login entry and, when the flow cannot work, says exactly what to
// configure — instead of the old 501 that the host reads as "this plugin has no
// login".
//
// Status vocabulary is the host's (model.Login*): pending | need_input |
// success | failed. "ok"/"expired" are NOT understood by the host and would
// leave the dialog spinning, so they are asserted against here.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// loadManifestLogin reads manifest.json's login section.
func loadManifestLogin(t *testing.T) (methods []string, notes string) {
	t.Helper()
	raw, err := os.ReadFile("manifest.json")
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	var mf struct {
		Login *struct {
			Methods []string `json:"methods"`
			Notes   string   `json:"notes"`
		} `json:"login"`
	}
	if err := json.Unmarshal(raw, &mf); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if mf.Login == nil {
		t.Fatal("manifest has no login section: the host reports methods=[] source=derived")
	}
	return mf.Login.Methods, mf.Login.Notes
}

// The host derives the login UI from this section: no methods means the account
// page says "该插件没有声明登录方式".
func TestManifestDeclaresLoginMethod(t *testing.T) {
	methods, notes := loadManifestLogin(t)
	if len(methods) == 0 {
		t.Fatal("manifest.login.methods is empty; the host shows no login entry")
	}
	if methods[0] != "oauth" {
		t.Fatalf("methods = %v, want oauth first (WeChat scan)", methods)
	}
	for _, want := range []string{envAccessKeyID, envAccessKeySecret} {
		if !strings.Contains(notes, want) {
			t.Fatalf("login.notes must name %s so the operator knows what to set; got %q", want, notes)
		}
	}
}

// The manifest's capability list must equal what /meta advertises, or the host's
// account page and the plugin disagree about whether login exists.
func TestManifestCapabilitiesMatchMeta(t *testing.T) {
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
	if strings.Join(mf.Capabilities, ",") != strings.Join(capabilities, ",") {
		t.Fatalf("manifest capabilities = %v, /meta = %v", mf.Capabilities, capabilities)
	}
	has := false
	for _, c := range mf.Capabilities {
		if c == "login_oauth" {
			has = true
		}
	}
	if !has {
		t.Fatal("manifest must declare login_oauth: POST /accounts/login really opens a scan session")
	}
}

func newTestServer(t *testing.T, signing xfyunSecret) *server {
	t.Helper()
	st, err := newStore(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	return &server{
		cfg:     pluginConfig{ID: "loomy", APILevel: 2, Token: "tk"},
		store:   st,
		client:  newUpstreamClient(),
		signing: signing,
		logins:  newLoginManager(),
	}
}

// Without the signing pair the start must be actionable: 400 plus the variable
// names. A 501 would make the host hide the entry entirely.
func TestLoginStartWithoutSigningKeyIsActionable(t *testing.T) {
	s := newTestServer(t, xfyunSecret{})
	rec := httptest.NewRecorder()
	s.handleLoginStart(rec, httptest.NewRequest(http.MethodPost, "/accounts/login", nil))

	if rec.Code == http.StatusNotImplemented {
		t.Fatal("501 tells the host this plugin has no login; it must be a 4xx with a hint")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{envAccessKeyID, envAccessKeySecret} {
		if !strings.Contains(body, want) {
			t.Fatalf("body must name %s; got %q", want, body)
		}
	}
	// The operator must learn where the pair comes from, not just its name.
	if !strings.Contains(body, "讯飞") {
		t.Fatalf("body should say where the pair comes from; got %q", body)
	}
}

// Only oauth exists here; another method is a clear 400, not a silent fallback.
func TestLoginStartRejectsUnknownMethod(t *testing.T) {
	s := newTestServer(t, xfyunSecret{AccessKeyID: "id", AccessKeySecret: "secret"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/accounts/login", strings.NewReader(`{"method":"paste_code"}`))
	s.handleLoginStart(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "/admin/credentials") {
		t.Fatalf("body should point at the manual path; got %q", rec.Body.String())
	}
}

// With the pair configured the start hands out a scannable URL. The auth page is
// stubbed (wechatAuthPageURL), so this exercises the real parsing and response
// shape without touching the network.
func TestLoginStartWithKeyReturnsScannableURL(t *testing.T) {
	old := wechatAuthPageURL
	wechatAuthPageURL = func(string) string { return "https://stub.invalid/qrconnect" }
	defer func() { wechatAuthPageURL = old }()

	const uuid = "001ZDsw64Vu7ll2E"
	var gotUA string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(`<img class="js_qrcode_img" src="/connect/qrcode/` + uuid + `"/>`))
	}))
	defer stub.Close()
	wechatAuthPageURL = func(string) string { return stub.URL + "/connect/qrconnect" }

	s := newTestServer(t, xfyunSecret{AccessKeyID: "id", AccessKeySecret: "secret"})
	rec := httptest.NewRecorder()
	s.handleLoginStart(rec, httptest.NewRequest(http.MethodPost, "/accounts/login", strings.NewReader(`{"method":"oauth"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var out struct {
		SessionID    string `json:"session_id"`
		Status       string `json:"status"`
		Method       string `json:"method"`
		URL          string `json:"url"`
		Code         string `json:"code"`
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if out.SessionID == "" || out.Status != "pending" || out.Method != "oauth" {
		t.Fatalf("start = %+v, want a pending oauth session", out)
	}
	if out.Code != uuid {
		t.Fatalf("code = %q, want the extracted uuid %q", out.Code, uuid)
	}
	want := "https://open.weixin.qq.com/connect/qrcode/" + uuid
	if out.URL != want {
		t.Fatalf("url = %q, want the QR page %q", out.URL, want)
	}
	if out.Instructions == "" {
		t.Fatal("instructions must tell the user to scan")
	}
	if gotUA == "" {
		t.Fatal("the auth page must be fetched with a browser UA")
	}
	// The session must be retrievable for polling.
	if _, ok := s.logins.get(out.SessionID); !ok {
		t.Fatal("the started session is not in the manager")
	}
}

// The auth page URL is the reference's, verbatim: the appid and the official
// redirect matter because WeChat validates the redirect domain against a
// whitelist (a local address yields "redirect_uri 参数错误").
func TestWechatAuthURLShape(t *testing.T) {
	got := buildLoomyWechatAuthURL("st8")
	for _, want := range []string{
		"https://open.weixin.qq.com/connect/qrconnect?",
		"appid=" + wechatAppID,
		"redirect_uri=" + url.QueryEscape(wechatRedirectURI),
		"response_type=code",
		"scope=snsapi_login",
		"state=st8",
		"#wechat_redirect",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("auth URL %q is missing %q", got, want)
		}
	}
	if want := "https://open.weixin.qq.com/connect/qrcode/abc_DEF-1"; qrImageURL("abc_DEF-1") != want {
		t.Errorf("qrImageURL = %q, want %q", qrImageURL("abc_DEF-1"), want)
	}
}

// extractWechatUUID reads both embedded paths and rejects junk, so a page
// structure change surfaces as "no uuid" instead of a bogus scan.
func TestExtractWechatUUID(t *testing.T) {
	cases := []struct{ name, html, want string }{
		{"img", `<img class="js_qrcode_img" src="/connect/qrcode/001ZDsw64Vu7ll2E"/>`, "001ZDsw64Vu7ll2E"},
		{"poll", `var fordevtool = "https://long.open.weixin.qq.com/connect/l/qrconnect?uuid=4Y0N_jyVQg=="`, "4Y0N_jyVQg=="},
		{"empty", ``, ""},
		{"junk", `<img src="/connect/qrcode/ab"/>`, ""},
	}
	for _, c := range cases {
		if got := extractWechatUUID(c.html); got != c.want {
			t.Errorf("%s: extractWechatUUID = %q, want %q", c.name, got, c.want)
		}
	}
}

