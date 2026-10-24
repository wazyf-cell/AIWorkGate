package webui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"aiworkgate/internal/model"
	"aiworkgate/internal/rules"
)

// launchTokenOf returns the token handler's /api/session accepted at build time.
// New mints one when Options.SessionToken is empty, so tests cannot hardcode it.
func launchTokenOf(t *testing.T, handler http.Handler) string {
	t.Helper()
	carrier, ok := handler.(*launchHandler)
	if !ok {
		t.Fatalf("handler is %T, want *launchHandler", handler)
	}
	return carrier.LaunchToken()
}

// hostSessionCookieFor exchanges the launch token on /api/session for the cookie
// the guard requires, so tests describe intent instead of plumbing.
//
// This used to replay the SPA document request and read the cookie it set. That
// flow is the vulnerability being fixed - any local GET used to earn a
// credential - so the helper now proves possession of the token instead.
func hostSessionCookieFor(t *testing.T, handler http.Handler) *http.Cookie {
	t.Helper()
	target := sessionEndpoint + "?" + launchTokenParam + "=" + launchTokenOf(t, handler)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, target, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST %s = %d, want 200 (body %s)", sessionEndpoint, recorder.Code, recorder.Body.String())
	}
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == hostSessionCookie {
			return cookie
		}
	}
	t.Fatalf("session exchange did not set the %s cookie", hostSessionCookie)
	return nil
}

func doRequest(t *testing.T, handler http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var request *http.Request
	if body == "" {
		request = httptest.NewRequest(method, target, nil)
	} else {
		request = httptest.NewRequest(method, target, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
	}
	if strings.HasPrefix(target, apiPrefix) {
		request.AddCookie(hostSessionCookieFor(t, handler))
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func decodeJSONBody(t *testing.T, recorder *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("content-type = %q, want application/json", contentType)
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
}

func TestStatus(t *testing.T) {
	handler := New(Options{
		Status: func() model.HostStatus {
			return model.HostStatus{
				Version:        "0.1.0-test",
				Port:           7865,
				DataDir:        `C:\data`,
				AConfigured:    true,
				ARunning:       false,
				APort:          7864,
				PluginsRunning: 2,
				PluginsTotal:   3,
			}
		},
	})

	recorder := doRequest(t, handler, http.MethodGet, "/api/status", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var status model.HostStatus
	decodeJSONBody(t, recorder, &status)
	if status.Version != "0.1.0-test" || status.Port != 7865 || !status.AConfigured {
		t.Fatalf("unexpected status payload: %+v", status)
	}
	if status.PluginsRunning != 2 || status.PluginsTotal != 3 {
		t.Fatalf("unexpected plugin counters: %+v", status)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	stored := json.RawMessage(`{"port":7865,"debug_body":false}`)
	handler := New(Options{
		GetSettings: func() (json.RawMessage, error) {
			return append(json.RawMessage(nil), stored...), nil
		},
		SaveSettings: func(raw json.RawMessage) error {
			stored = append(json.RawMessage(nil), raw...)
			return nil
		},
	})

	recorder := doRequest(t, handler, http.MethodGet, "/api/settings", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET settings = %d, want 200", recorder.Code)
	}
	var initial map[string]any
	decodeJSONBody(t, recorder, &initial)
	if initial["port"] != float64(7865) {
		t.Fatalf("initial port = %v, want 7865", initial["port"])
	}

	recorder = doRequest(t, handler, http.MethodPut, "/api/settings", `{"port":7999,"debug_body":true,"lan_enabled":false}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT settings = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	recorder = doRequest(t, handler, http.MethodGet, "/api/settings", "")
	var updated map[string]any
	decodeJSONBody(t, recorder, &updated)
	if updated["port"] != float64(7999) || updated["debug_body"] != true {
		t.Fatalf("round-trip settings = %+v", updated)
	}
}

func TestNilFunctionsReturn501(t *testing.T) {
	handler := New(Options{})
	cases := []struct {
		method string
		target string
		body   string
	}{
		{http.MethodGet, "/api/status", ""},
		{http.MethodGet, "/api/settings", ""},
		{http.MethodPut, "/api/settings", `{"port":7865}`},
		{http.MethodGet, "/api/secrets/a-key", ""},
		{http.MethodPut, "/api/secrets/a-key", `{"key":"secret"}`},
		{http.MethodPut, "/api/secrets/lan-password", `{"password":"pw"}`},
		{http.MethodGet, "/api/plugins", ""},
		{http.MethodPost, "/api/plugins/install", `{"zip_path":"C:\\plugin.zip"}`},
		{http.MethodPost, "/api/plugins/mock/enable", ""},
		{http.MethodGet, "/api/accounts?plugin=mock", ""},
		{http.MethodPut, "/api/accounts/egress", `{"plugin_id":"mock","account_id":"acc_1","mode":"direct"}`},
		{http.MethodPost, "/api/accounts/identity/reset", `{"plugin_id":"mock","account_id":"acc_1","mode":"request"}`},
		{http.MethodPost, "/api/accounts/login", `{"plugin_id":"mock","method":"oauth"}`},
		{http.MethodGet, "/api/accounts/login/ls_1?plugin=mock", ""},
		{http.MethodPost, "/api/accounts/login/ls_1/submit", `{"plugin_id":"mock","values":{"api_key":"x"}}`},
		{http.MethodDelete, "/api/accounts/login/ls_1?plugin=mock", ""},
		{http.MethodPost, "/api/accounts/patch", `{"plugin_id":"mock","account_id":"acc_1","patch":{"enabled":false}}`},
		{http.MethodPost, "/api/accounts/delete", `{"plugin_id":"mock","account_id":"acc_1"}`},
		{http.MethodPost, "/api/accounts/refresh", `{"plugin_id":"mock","account_id":"acc_1"}`},
		{http.MethodPost, "/api/accounts/checkin", `{"plugin_id":"mock","account_id":"acc_1"}`},
		{http.MethodPost, "/api/accounts/rename", `{"plugin_id":"mock","account_id":"acc_1","alias":"x"}`},
		{http.MethodPost, "/api/accounts/bulk", `{"action":"checkin","items":[{"plugin_id":"mock","account_id":"acc_1"}]}`},
		{http.MethodPost, "/api/accounts/save", `{"accounts":[]}`},
		{http.MethodPost, "/api/accounts/identity/details", `{"plugin_id":"mock","account_id":"acc_1"}`},
		{http.MethodGet, "/api/accounts/login-state", ""},
		{http.MethodGet, "/api/groups", ""},
		{http.MethodPost, "/api/groups", `{"name":"g"}`},
		{http.MethodGet, "/api/checkin?plugin=mock&from=2026-10-01&to=2026-10-31", ""},
		{http.MethodPost, "/api/checkin/all", ""},
		{http.MethodGet, "/api/rules", ""},
		{http.MethodPut, "/api/rules/pack-a", `{"rules":[{"from":"a","to":"b"}]}`},
		{http.MethodDelete, "/api/rules/pack-a", ""},
		{http.MethodPost, "/api/rules/wb-moderation/reset", ""},
		{http.MethodPost, "/api/rules/reload", ""},
		{http.MethodGet, "/api/proxies", ""},
		{http.MethodPost, "/api/proxies", `{"name":"p","host":"127.0.0.1","port":1080}`},
		{http.MethodPut, "/api/proxies/px_1", `{"name":"p","host":"127.0.0.1","port":1080}`},
		{http.MethodDelete, "/api/proxies/px_1", ""},
		{http.MethodPost, "/api/proxies/px_1/check", ""},
		{http.MethodGet, "/api/logs/requests", ""},
		{http.MethodGet, "/api/logs/runtime", ""},
		{http.MethodGet, "/api/keys", ""},
		{http.MethodPost, "/api/keys", `{"name":"test"}`},
		{http.MethodDelete, "/api/keys/key_1", ""},
	}

	for _, tc := range cases {
		recorder := doRequest(t, handler, tc.method, tc.target, tc.body)
		if recorder.Code != http.StatusNotImplemented {
			t.Errorf("%s %s = %d, want 501 (body %s)", tc.method, tc.target, recorder.Code, recorder.Body.String())
			continue
		}
		var payload struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		decodeJSONBody(t, recorder, &payload)
		if payload.Error.Type != "not_implemented" {
			t.Errorf("%s %s error type = %q, want not_implemented", tc.method, tc.target, payload.Error.Type)
		}
		if payload.Error.Message == "" {
			t.Errorf("%s %s returned an empty error message", tc.method, tc.target)
		}
	}
}

// TestPluginInstallRejectsServerSidePath pins the T3 fix: the install endpoint
// no longer accepts a server-side path. A bare JSON body (the old zip_path /
// path form) must answer 400 instead of handing a path to the installer, which
// would let any caller install anything the gateway user can read - and a UNC
// path would leak NTLM credentials to a remote host.
func TestPluginInstallRejectsServerSidePath(t *testing.T) {
	var installed []string
	handler := New(Options{
		InstallPlugin: func(zipPath string, _ bool) error {
			installed = append(installed, zipPath)
			return nil
		},
	})

	for _, body := range []string{
		`{"zip_path":"C:\\plugin.zip"}`,
		`{"path":"\\\\attacker\\share\\plugin.zip"}`,
		`{}`,
	} {
		recorder := doRequest(t, handler, http.MethodPost, "/api/plugins/install", body)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("POST /api/plugins/install %s = %d, want 400 (body %s)",
				body, recorder.Code, recorder.Body.String())
			continue
		}
	}
	if len(installed) != 0 {
		t.Fatalf("a path-mode request reached the installer: %v", installed)
	}
}

// TestPluginInstallUploadsFile proves the multipart path works and that the
// archive handed to the installer is a real, readable zip that is cleaned up
// afterwards.
func TestPluginInstallUploadsFile(t *testing.T) {
	const payload = "PK\x03\x04 not really a zip, but the handler never parses it"
	var gotPath string
	var gotEnabled bool
	handler := New(Options{
		InstallPlugin: func(zipPath string, enabled bool) error {
			gotPath, gotEnabled = zipPath, enabled
			data, err := os.ReadFile(zipPath)
			if err != nil {
				t.Errorf("installer cannot read the staged archive: %v", err)
				return nil
			}
			if string(data) != payload {
				t.Errorf("staged archive body = %q, want the uploaded bytes", data)
			}
			return nil
		},
	})

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile(installFormField, "plugin.zip")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write([]byte(payload)); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/plugins/install", &buf)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.AddCookie(hostSessionCookieFor(t, handler))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("upload = %d, want 202 (body %s)", recorder.Code, recorder.Body.String())
	}
	if gotPath == "" {
		t.Fatal("installer was not called")
	}
	if gotEnabled {
		t.Error("enabled defaulted to true; a fresh upload must stay stopped")
	}
	if _, err := os.Stat(gotPath); !os.IsNotExist(err) {
		t.Errorf("staged archive %q was not cleaned up: %v", gotPath, err)
	}
}

func TestSPAFallback(t *testing.T) {
	handler := New(Options{})

	for _, target := range []string{"/", "/plugins", "/accounts/detail?x=1"} {
		recorder := doRequest(t, handler, http.MethodGet, target, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", target, recorder.Code)
		}
		if contentType := recorder.Header().Get("Content-Type"); !strings.Contains(contentType, "text/html") {
			t.Fatalf("GET %s content-type = %q, want text/html", target, contentType)
		}
		if !strings.Contains(recorder.Body.String(), `id="app"`) {
			t.Fatalf("GET %s did not serve the SPA shell", target)
		}
	}
}

func TestEmbeddedAssetsServed(t *testing.T) {
	assets, err := fs.Glob(distFS, "dist/assets/*.js")
	if err != nil || len(assets) == 0 {
		t.Fatalf("embedded dist has no JS assets: %v", err)
	}
	handler := New(Options{})
	target := "/" + strings.TrimPrefix(assets[0], "dist/")
	recorder := doRequest(t, handler, http.MethodGet, target, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", target, recorder.Code)
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.Contains(contentType, "javascript") {
		t.Fatalf("GET %s content-type = %q, want JavaScript", target, contentType)
	}
	if cache := recorder.Header().Get("Cache-Control"); !strings.Contains(cache, "immutable") {
		t.Fatalf("GET %s cache-control = %q, want immutable", target, cache)
	}
}

func TestAPIUnknownReturnsJSON404(t *testing.T) {
	handler := New(Options{})
	recorder := doRequest(t, handler, http.MethodGet, "/api/does-not-exist", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
	var payload struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	decodeJSONBody(t, recorder, &payload)
	if payload.Error.Type != "not_found" {
		t.Fatalf("error type = %q, want not_found", payload.Error.Type)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	handler := New(Options{Status: func() model.HostStatus { return model.HostStatus{} }})
	recorder := doRequest(t, handler, http.MethodPatch, "/api/status", "")
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", recorder.Code)
	}
	if allow := recorder.Header().Get("Allow"); !strings.Contains(allow, http.MethodGet) {
		t.Fatalf("Allow = %q, want GET", allow)
	}

	recorder = doRequest(t, handler, http.MethodPost, "/some/spa/route", "")
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST spa = %d, want 405", recorder.Code)
	}
}

func TestProxiesCRUDAndValidation(t *testing.T) {
	proxies := []model.Proxy{}
	handler := New(Options{
		Proxies: func() []model.Proxy { return proxies },
		SaveProxy: func(proxy model.Proxy) error {
			if proxy.ID == "" {
				proxy.ID = "px_test"
			}
			for index := range proxies {
				if proxies[index].ID == proxy.ID {
					proxies[index] = proxy
					return nil
				}
			}
			proxies = append(proxies, proxy)
			return nil
		},
		DeleteProxy: func(id string) error {
			kept := proxies[:0]
			for _, proxy := range proxies {
				if proxy.ID != id {
					kept = append(kept, proxy)
				}
			}
			proxies = kept
			return nil
		},
		CheckProxy: func(id string) any {
			return map[string]any{"ok": true, "exit_ip": "203.0.113.7", "latency_ms": 42}
		},
	})

	recorder := doRequest(t, handler, http.MethodPost, "/api/proxies", `{"name":"hk-1","type":"socks5","host":"127.0.0.1","port":1080,"enabled":true}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create proxy = %d, want 201 (body %s)", recorder.Code, recorder.Body.String())
	}
	if len(proxies) != 1 || proxies[0].Type != "socks5" || proxies[0].Name != "hk-1" {
		t.Fatalf("stored proxies = %+v", proxies)
	}
	// The caller binds a new proxy by the id in this response, and the stored
	// password must never be echoed back.
	var created struct {
		Proxy model.Proxy `json:"proxy"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
		t.Fatalf("create body is not JSON: %v", err)
	}
	if created.Proxy.ID != "px_test" {
		t.Fatalf("created proxy id = %q, want px_test (body %s)", created.Proxy.ID, recorder.Body.String())
	}
	if created.Proxy.Password != "" {
		t.Fatalf("created proxy echoed a password: %q", created.Proxy.Password)
	}

	recorder = doRequest(t, handler, http.MethodPost, "/api/proxies", `{"name":"bad","type":"ftp","host":"127.0.0.1","port":21}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid type = %d, want 400", recorder.Code)
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/proxies", `{"name":"bad","type":"http","host":"127.0.0.1","port":70000}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid port = %d, want 400", recorder.Code)
	}

	recorder = doRequest(t, handler, http.MethodPut, "/api/proxies/px_test", `{"name":"hk-2","type":"http","host":"10.0.0.1","port":3128}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("update proxy = %d, want 200", recorder.Code)
	}
	if proxies[0].ID != "px_test" || proxies[0].Name != "hk-2" || proxies[0].Type != "http" {
		t.Fatalf("updated proxy = %+v", proxies[0])
	}

	recorder = doRequest(t, handler, http.MethodGet, "/api/proxies", "")
	var listed []model.Proxy
	decodeJSONBody(t, recorder, &listed)
	if len(listed) != 1 {
		t.Fatalf("listed proxies = %+v", listed)
	}

	recorder = doRequest(t, handler, http.MethodPost, "/api/proxies/px_test/check", "")
	var check map[string]any
	decodeJSONBody(t, recorder, &check)
	if check["exit_ip"] != "203.0.113.7" {
		t.Fatalf("check payload = %+v", check)
	}

	recorder = doRequest(t, handler, http.MethodDelete, "/api/proxies/px_test", "")
	if recorder.Code != http.StatusOK || len(proxies) != 0 {
		t.Fatalf("delete proxy code = %d, remaining = %+v", recorder.Code, proxies)
	}
}

// The "system" row is mirrored from Windows Internet Settings, so the API must
// refuse to create, overwrite or delete it instead of letting the next poll
// silently undo the edit.
func TestSystemProxyRowIsReadOnly(t *testing.T) {
	saved, deleted := 0, 0
	handler := New(Options{
		Proxies: func() []model.Proxy {
			return []model.Proxy{{ID: model.SystemProxyID, Name: model.SystemProxyName, Type: "http", Host: "127.0.0.1", Port: 7897, Enabled: true}}
		},
		SaveProxy:   func(model.Proxy) error { saved++; return nil },
		DeleteProxy: func(string) error { deleted++; return nil },
	})

	recorder := doRequest(t, handler, http.MethodPost, "/api/proxies",
		`{"id":"system","name":"冒充","type":"http","host":"127.0.0.1","port":1,"enabled":true}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("create with reserved id = %d, want 409 (body %s)", recorder.Code, recorder.Body.String())
	}
	recorder = doRequest(t, handler, http.MethodPut, "/api/proxies/system",
		`{"name":"冒充","type":"http","host":"127.0.0.1","port":1,"enabled":true}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("update reserved row = %d, want 409 (body %s)", recorder.Code, recorder.Body.String())
	}
	recorder = doRequest(t, handler, http.MethodDelete, "/api/proxies/system", "")
	if recorder.Code != http.StatusConflict {
		t.Fatalf("delete reserved row = %d, want 409 (body %s)", recorder.Code, recorder.Body.String())
	}
	if saved != 0 || deleted != 0 {
		t.Fatalf("reserved row reached the app layer: saved=%d deleted=%d", saved, deleted)
	}
}

func TestSecretsAndKeys(t *testing.T) {
	aKey := ""
	lanPassword := "unset"
	keys := []model.ClientKey{}
	handler := New(Options{
		AKeyConfigured: func() bool { return aKey != "" },
		SetAKey:        func(key string) error { aKey = key; return nil },
		SetLANPassword: func(password string) error { lanPassword = password; return nil },
		Keys:           func() []model.ClientKey { return keys },
		CreateKey: func(name string) (string, model.ClientKey, error) {
			key := model.ClientKey{ID: "agk_1", Name: name, Prefix: "agk_abcd", Enabled: true}
			keys = append(keys, key)
			return "agk_secret_value", key, nil
		},
		DeleteKey: func(id string) error {
			keys = keys[:0]
			return nil
		},
	})

	recorder := doRequest(t, handler, http.MethodGet, "/api/secrets/a-key", "")
	var keyStatus map[string]bool
	decodeJSONBody(t, recorder, &keyStatus)
	if keyStatus["configured"] {
		t.Fatalf("A key should start unconfigured")
	}

	recorder = doRequest(t, handler, http.MethodPut, "/api/secrets/a-key", `{"key":"  token-123  "}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT a-key = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if aKey != "token-123" {
		t.Fatalf("stored A key = %q, want trimmed token", aKey)
	}

	recorder = doRequest(t, handler, http.MethodPut, "/api/secrets/a-key", `{"key":"   "}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("empty A key = %d, want 400", recorder.Code)
	}

	recorder = doRequest(t, handler, http.MethodPut, "/api/secrets/lan-password", `{"password":"lan-pass"}`)
	if recorder.Code != http.StatusOK || lanPassword != "lan-pass" {
		t.Fatalf("LAN password code = %d, stored = %q", recorder.Code, lanPassword)
	}

	recorder = doRequest(t, handler, http.MethodPost, "/api/keys", `{"name":"laptop"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create key = %d, want 201", recorder.Code)
	}
	var created struct {
		Secret string          `json:"secret"`
		Key    model.ClientKey `json:"key"`
	}
	decodeJSONBody(t, recorder, &created)
	if created.Secret != "agk_secret_value" || created.Key.Name != "laptop" {
		t.Fatalf("created key = %+v", created)
	}

	recorder = doRequest(t, handler, http.MethodDelete, "/api/keys/agk_1", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("delete key = %d, want 200", recorder.Code)
	}
}

func TestRuntimeLogsDefaultsAndCap(t *testing.T) {
	var gotSource string
	var gotLimit int
	handler := New(Options{
		RuntimeLogs: func(source string, n int) []string {
			gotSource = source
			gotLimit = n
			return nil
		},
	})

	recorder := doRequest(t, handler, http.MethodGet, "/api/logs/runtime", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("runtime logs = %d, want 200", recorder.Code)
	}
	if gotSource != "host" || gotLimit != 200 {
		t.Fatalf("defaults = (%q, %d), want (host, 200)", gotSource, gotLimit)
	}
	var payload struct {
		Lines []string `json:"lines"`
		Count int      `json:"count"`
	}
	decodeJSONBody(t, recorder, &payload)
	if payload.Lines == nil || payload.Count != 0 {
		t.Fatalf("empty runtime payload = %+v", payload)
	}

	doRequest(t, handler, http.MethodGet, "/api/logs/runtime?source=plugin:mock&n=99999", "")
	if gotSource != "plugin:mock" || gotLimit != 5000 {
		t.Fatalf("capped request = (%q, %d), want (plugin:mock, 5000)", gotSource, gotLimit)
	}

	doRequest(t, handler, http.MethodGet, "/api/logs/runtime?n=abc", "")
	if gotLimit != 200 {
		t.Fatalf("invalid n limit = %d, want 200", gotLimit)
	}
}

func TestRequestLogsQueryForwarding(t *testing.T) {
	var got map[string]string
	handler := New(Options{
		RequestLogs: func(query map[string]string) any {
			got = query
			return []model.APIRequestLog{}
		},
	})

	recorder := doRequest(t, handler, http.MethodGet, "/api/logs/requests?source=plugin:mock&status=200&limit=50", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("request logs = %d, want 200", recorder.Code)
	}
	if got["source"] != "plugin:mock" || got["status"] != "200" || got["limit"] != "50" {
		t.Fatalf("forwarded query = %+v", got)
	}
}

func TestBodyLimit(t *testing.T) {
	handler := New(Options{
		SaveSettings: func(json.RawMessage) error { return nil },
	})
	huge := `{"padding":"` + strings.Repeat("a", maxBodyBytes+64) + `"}`
	recorder := doRequest(t, handler, http.MethodPut, "/api/settings", huge)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body = %d, want 413 (body %s)", recorder.Code, recorder.Body.String())
	}
}

// ---------------------------------------------------------------- M6 rules

// errorType decodes the error.type field of an admin API failure.
func errorType(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	decodeJSONBody(t, recorder, &payload)
	if payload.Error.Message == "" {
		t.Error("error message is empty")
	}
	return payload.Error.Type
}

func TestRulesEndpoints(t *testing.T) {
	packs := []model.RulePack{{Name: rules.BuiltinWBPack, Description: "builtin"}}
	var saved []model.RulePack
	var deleted []string
	var resets []string
	reloads := 0
	handler := New(Options{
		RulePacks: func() ([]model.RulePack, []string, error) {
			return packs, []string{rules.BuiltinWBPack}, nil
		},
		SaveRulePack: func(pack model.RulePack) error {
			switch pack.Name {
			case "reject-me":
				return fmt.Errorf("%w: rule 0 has an empty from", rules.ErrInvalidPack)
			case "break-me":
				return errors.New("disk is full")
			}
			saved = append(saved, pack)
			for i := range packs {
				if packs[i].Name == pack.Name {
					packs[i] = pack
					return nil
				}
			}
			packs = append(packs, pack)
			return nil
		},
		DeleteRulePack: func(name string) error {
			deleted = append(deleted, name)
			return nil
		},
		ResetRulePack: func(name string) error {
			if name != rules.BuiltinWBPack {
				return fmt.Errorf("%w: %s", rules.ErrPackNotFound, name)
			}
			resets = append(resets, name)
			return nil
		},
		ReloadRules: func() ([]string, map[string]string, error) {
			reloads++
			return []string{"mock"}, map[string]string{"other": "reload failed"}, nil
		},
	})

	recorder := doRequest(t, handler, http.MethodGet, "/api/rules", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET rules = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var listed struct {
		Packs   []model.RulePack `json:"packs"`
		Builtin []string         `json:"builtin"`
	}
	decodeJSONBody(t, recorder, &listed)
	if len(listed.Packs) != 1 || listed.Packs[0].Name != rules.BuiltinWBPack {
		t.Fatalf("listed packs = %+v", listed.Packs)
	}
	if len(listed.Builtin) != 1 || listed.Builtin[0] != rules.BuiltinWBPack {
		t.Fatalf("listed builtins = %+v", listed.Builtin)
	}

	// A body without a name takes it from the path.
	recorder = doRequest(t, handler, http.MethodPut, "/api/rules/pack-a", `{"rules":[{"from":"a","to":"b"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT rules = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if len(saved) != 1 || saved[0].Name != "pack-a" || len(saved[0].Rules) != 1 {
		t.Fatalf("saved packs = %+v", saved)
	}

	// A body name that disagrees with the path is rejected.
	recorder = doRequest(t, handler, http.MethodPut, "/api/rules/pack-a", `{"name":"pack-b"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("mismatched name = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
	}
	if got := errorType(t, recorder); got != "invalid_rules" {
		t.Fatalf("mismatched name error type = %q, want invalid_rules", got)
	}

	// Manager validation errors become invalid_rules; other failures stay 500.
	recorder = doRequest(t, handler, http.MethodPut, "/api/rules/reject-me", `{"rules":[{"from":"","to":"b"}]}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid pack = %d, want 400", recorder.Code)
	}
	if got := errorType(t, recorder); got != "invalid_rules" {
		t.Fatalf("invalid pack error type = %q, want invalid_rules", got)
	}
	recorder = doRequest(t, handler, http.MethodPut, "/api/rules/break-me", `{"rules":[{"from":"a","to":"b"}]}`)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("failing save = %d, want 500", recorder.Code)
	}
	if got := errorType(t, recorder); got != "internal_error" {
		t.Fatalf("failing save error type = %q, want internal_error", got)
	}

	// DELETE succeeds for unknown names too (the manager treats it as a no-op).
	recorder = doRequest(t, handler, http.MethodDelete, "/api/rules/pack-a", "")
	if recorder.Code != http.StatusOK || len(deleted) != 1 || deleted[0] != "pack-a" {
		t.Fatalf("DELETE rules = %d, %+v", recorder.Code, deleted)
	}

	// Reset only knows the builtin packs.
	recorder = doRequest(t, handler, http.MethodPost, "/api/rules/custom-pack/reset", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("reset unknown = %d, want 404 (body %s)", recorder.Code, recorder.Body.String())
	}
	if got := errorType(t, recorder); got != "not_found" {
		t.Fatalf("reset unknown error type = %q, want not_found", got)
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/rules/wb-moderation/reset", "")
	if recorder.Code != http.StatusOK || len(resets) != 1 || resets[0] != rules.BuiltinWBPack {
		t.Fatalf("reset builtin = %d, %+v", recorder.Code, resets)
	}

	// Reload reports both reloaded ids and per-plugin failures, still with 200.
	recorder = doRequest(t, handler, http.MethodPost, "/api/rules/reload", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("reload = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var reloaded struct {
		OK       bool              `json:"ok"`
		Reloaded []string          `json:"reloaded"`
		Failed   map[string]string `json:"failed"`
	}
	decodeJSONBody(t, recorder, &reloaded)
	if !reloaded.OK || reloads != 1 {
		t.Fatalf("reload payload = %+v (calls %d)", reloaded, reloads)
	}
	if len(reloaded.Reloaded) != 1 || reloaded.Reloaded[0] != "mock" {
		t.Fatalf("reloaded ids = %+v", reloaded.Reloaded)
	}
	if reloaded.Failed["other"] != "reload failed" {
		t.Fatalf("failed map = %+v", reloaded.Failed)
	}
}

// ---------------------------------------------------------------- M7 egress

// pluginStub builds the PluginStatus the account-level routes inspect.
func pluginStub(id string, running bool, capabilities []string, osIdentity bool) model.PluginStatus {
	return model.PluginStatus{
		Manifest: model.Manifest{
			ID:           id,
			Capabilities: capabilities,
			Permissions:  model.Permissions{OSIdentity: osIdentity},
		},
		Enabled: true,
		Running: running,
	}
}

func TestAccountEgressAssignment(t *testing.T) {
	proxies := []model.Proxy{
		{ID: "px_on", Name: "on", Enabled: true},
		{ID: "px_off", Name: "off", Enabled: false},
	}
	type call struct {
		pluginID  string
		accountID string
		mode      string
		proxyID   string
	}
	var calls []call
	handler := New(Options{
		Plugins: func() []model.PluginStatus {
			return []model.PluginStatus{pluginStub("mock", true, nil, false)}
		},
		Proxies: func() []model.Proxy { return proxies },
		SetAccountEgress: func(pluginID, accountID, mode, proxyID string) (bool, string, error) {
			calls = append(calls, call{pluginID, accountID, mode, proxyID})
			if accountID == "acc_warn" {
				return false, "plugin reload failed: boom", nil
			}
			return true, "", nil
		},
	})

	// Proxy mode with an enabled proxy.
	recorder := doRequest(t, handler, http.MethodPut, "/api/accounts/egress",
		`{"plugin_id":"mock","account_id":"acc_1","mode":"proxy","proxy_id":"px_on"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("proxy mode = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var assigned struct {
		OK       bool `json:"ok"`
		Reloaded bool `json:"reloaded"`
	}
	decodeJSONBody(t, recorder, &assigned)
	if !assigned.OK || !assigned.Reloaded {
		t.Fatalf("proxy mode payload = %+v", assigned)
	}
	if len(calls) != 1 || calls[0] != (call{"mock", "acc_1", "proxy", "px_on"}) {
		t.Fatalf("calls = %+v", calls)
	}

	// Direct mode drops the proxy id.
	recorder = doRequest(t, handler, http.MethodPut, "/api/accounts/egress",
		`{"plugin_id":"mock","account_id":"acc_1","mode":"direct","proxy_id":"px_on"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("direct mode = %d, want 200", recorder.Code)
	}
	if calls[len(calls)-1] != (call{"mock", "acc_1", "direct", ""}) {
		t.Fatalf("direct call = %+v", calls[len(calls)-1])
	}

	// Empty mode clears the assignment.
	recorder = doRequest(t, handler, http.MethodPut, "/api/accounts/egress",
		`{"plugin_id":"mock","account_id":"acc_1","mode":""}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("default mode = %d, want 200", recorder.Code)
	}
	if calls[len(calls)-1] != (call{"mock", "acc_1", "default", ""}) {
		t.Fatalf("default call = %+v", calls[len(calls)-1])
	}

	// A failed reload is a warning, not an error.
	recorder = doRequest(t, handler, http.MethodPut, "/api/accounts/egress",
		`{"plugin_id":"mock","account_id":"acc_warn","mode":"direct"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("warning case = %d, want 200", recorder.Code)
	}
	var warned struct {
		Reloaded bool   `json:"reloaded"`
		Warning  string `json:"warning"`
	}
	decodeJSONBody(t, recorder, &warned)
	if warned.Reloaded || warned.Warning != "plugin reload failed: boom" {
		t.Fatalf("warning payload = %+v", warned)
	}

	before := len(calls)
	for _, body := range []string{
		`{"plugin_id":"mock","account_id":"acc_1","mode":"proxy","proxy_id":"px_missing"}`,
		`{"plugin_id":"mock","account_id":"acc_1","mode":"proxy","proxy_id":"px_off"}`,
		`{"plugin_id":"mock","account_id":"acc_1","mode":"proxy"}`,
		`{"plugin_id":"mock","account_id":"acc_1","mode":"blocked"}`,
		`{"plugin_id":"mock","account_id":"","mode":"direct"}`,
	} {
		recorder = doRequest(t, handler, http.MethodPut, "/api/accounts/egress", body)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400 (body %s)", body, recorder.Code, recorder.Body.String())
		}
		if got := errorType(t, recorder); got != "invalid_egress" {
			t.Fatalf("%s error type = %q, want invalid_egress", body, got)
		}
	}
	if len(calls) != before {
		t.Fatalf("invalid requests reached the host: %+v", calls[before:])
	}

	// Unknown plugins are a 404.
	recorder = doRequest(t, handler, http.MethodPut, "/api/accounts/egress",
		`{"plugin_id":"ghost","account_id":"acc_1","mode":"direct"}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown plugin = %d, want 404", recorder.Code)
	}
	if got := errorType(t, recorder); got != "not_found" {
		t.Fatalf("unknown plugin error type = %q, want not_found", got)
	}
}

// ---------------------------------------------------------------- M8 identity

func TestIdentityResetValidation(t *testing.T) {
	var got []string
	handler := New(Options{
		Plugins: func() []model.PluginStatus {
			return []model.PluginStatus{
				pluginStub("mock", true, []string{model.CapIdentityOS}, true),
				pluginStub("limited", true, []string{model.CapIdentityRequest}, false),
				pluginStub("stopped", false, nil, false),
			}
		},
		ResetIdentity: func(pluginID, accountID, mode string) error {
			got = append(got, pluginID+"/"+accountID+"/"+mode)
			if accountID == "acc_fail" {
				return errors.New("os identity is locked by policy")
			}
			if accountID == "acc_unsupported" {
				return notApplicableError("identity is not applicable to this channel")
			}
			return nil
		},
	})

	// Request mode needs no confirmation.
	recorder := doRequest(t, handler, http.MethodPost, "/api/accounts/identity/reset",
		`{"plugin_id":"mock","account_id":"acc_1","mode":"request"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("request mode = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if len(got) != 1 || got[0] != "mock/acc_1/request" {
		t.Fatalf("reset calls = %v", got)
	}

	// OS mode requires confirm=true.
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/identity/reset",
		`{"plugin_id":"mock","account_id":"acc_1","mode":"os"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("os without confirm = %d, want 400", recorder.Code)
	}
	if kind := errorType(t, recorder); kind != "confirmation_required" {
		t.Fatalf("os without confirm error type = %q, want confirmation_required", kind)
	}
	if len(got) != 1 {
		t.Fatalf("unconfirmed reset reached the plugin: %v", got)
	}

	// OS mode needs the capability and the permission.
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/identity/reset",
		`{"plugin_id":"limited","account_id":"acc_1","mode":"os","confirm":true}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("os unsupported = %d, want 400", recorder.Code)
	}
	if kind := errorType(t, recorder); kind != "os_identity_unsupported" {
		t.Fatalf("os unsupported error type = %q, want os_identity_unsupported", kind)
	}

	// A capable plugin is called with mode os.
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/identity/reset",
		`{"plugin_id":"mock","account_id":"acc_2","mode":"os","confirm":true}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("os reset = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if len(got) != 2 || got[1] != "mock/acc_2/os" {
		t.Fatalf("reset calls = %v", got)
	}

	// Stopped plugins cannot answer.
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/identity/reset",
		`{"plugin_id":"stopped","account_id":"acc_1","mode":"request"}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("stopped plugin = %d, want 409", recorder.Code)
	}
	if kind := errorType(t, recorder); kind != "plugin_not_running" {
		t.Fatalf("stopped plugin error type = %q, want plugin_not_running", kind)
	}

	// Unknown plugins and unknown modes are rejected.
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/identity/reset",
		`{"plugin_id":"ghost","account_id":"acc_1","mode":"request"}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown plugin = %d, want 404", recorder.Code)
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/identity/reset",
		`{"plugin_id":"mock","account_id":"acc_1","mode":"derived"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown mode = %d, want 400", recorder.Code)
	}

	// Plugin failures surface as 502 with the plugin message.
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/identity/reset",
		`{"plugin_id":"mock","account_id":"acc_fail","mode":"request"}`)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("plugin failure = %d, want 502", recorder.Code)
	}
	if kind := errorType(t, recorder); kind != "plugin_error" {
		t.Fatalf("plugin failure error type = %q, want plugin_error", kind)
	}
	if !strings.Contains(recorder.Body.String(), "os identity is locked by policy") {
		t.Fatalf("plugin message missing: %s", recorder.Body.String())
	}

	// A plugin's own 501 (identity_not_applicable) must survive as 501, not 502.
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/identity/reset",
		`{"plugin_id":"mock","account_id":"acc_unsupported","mode":"request"}`)
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("unsupported reset = %d, want 501 (body %s)", recorder.Code, recorder.Body.String())
	}
	if kind := errorType(t, recorder); kind != "plugin_error" {
		t.Fatalf("unsupported reset error type = %q, want plugin_error", kind)
	}
}

// notApplicableError mimics plugins.HTTPError: a plugin-declared status the
// host must preserve.
type notApplicableError string

func (e notApplicableError) Error() string   { return string(e) }
func (e notApplicableError) HTTPStatus() int { return http.StatusNotImplemented }

// ---------------------------------------------------------------- M10 login

func TestAccountLoginStart(t *testing.T) {
	var methods []string
	handler := New(Options{
		Plugins: func() []model.PluginStatus {
			return []model.PluginStatus{
				pluginStub("codearts", true, []string{model.CapLoginOAuth}, false),
				pluginStub("stopped", false, nil, false),
			}
		},
		AccountLoginStart: func(pluginID, method string) (model.LoginStart, error) {
			methods = append(methods, method)
			switch method {
			case "oauth":
				return model.LoginStart{
					SessionID:    "ls_1",
					Status:       model.LoginPending,
					URL:          "https://example.test/login",
					Code:         "ABCD-1234",
					Instructions: "在浏览器完成登录",
				}, nil
			case "cookie":
				return model.LoginStart{}, errors.New("cookie login is not supported")
			default:
				return model.LoginStart{SessionID: "ls_2", Status: model.LoginSuccess, AccountID: "acc_1"}, nil
			}
		},
	})

	recorder := doRequest(t, handler, http.MethodPost, "/api/accounts/login",
		`{"plugin_id":"codearts","method":"oauth"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("login start = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var start model.LoginStart
	decodeJSONBody(t, recorder, &start)
	if start.SessionID != "ls_1" || start.Status != model.LoginPending ||
		start.URL != "https://example.test/login" || start.Code != "ABCD-1234" ||
		start.Instructions != "在浏览器完成登录" {
		t.Fatalf("login start payload = %+v", start)
	}

	// An empty method is forwarded so the plugin picks its default flow.
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/login", `{"plugin_id":"codearts"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("default method = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if len(methods) != 2 || methods[1] != "" {
		t.Fatalf("forwarded methods = %+v, want a second empty method", methods)
	}

	for _, body := range []string{
		`{"plugin_id":"","method":"oauth"}`,
		`{"plugin_id":"codearts","method":"telepathy"}`,
		`{"plugin_id":123}`,
		`not-json`,
	} {
		recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/login", body)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400 (body %s)", body, recorder.Code, recorder.Body.String())
		}
	}

	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/login",
		`{"plugin_id":"ghost","method":"oauth"}`)
	if recorder.Code != http.StatusNotFound || errorType(t, recorder) != "not_found" {
		t.Fatalf("unknown plugin = %d", recorder.Code)
	}

	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/login",
		`{"plugin_id":"stopped","method":"oauth"}`)
	if recorder.Code != http.StatusConflict || errorType(t, recorder) != "plugin_not_running" {
		t.Fatalf("stopped plugin = %d", recorder.Code)
	}

	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/login",
		`{"plugin_id":"codearts","method":"cookie"}`)
	if recorder.Code != http.StatusBadGateway || errorType(t, recorder) != "plugin_error" {
		t.Fatalf("plugin rejection = %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "cookie login is not supported") {
		t.Fatalf("plugin message missing: %s", recorder.Body.String())
	}

	recorder = doRequest(t, handler, http.MethodGet, "/api/accounts/login", "")
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET login start = %d, want 405", recorder.Code)
	}
}

func TestAccountLoginStartAcceptsPasteCode(t *testing.T) {
	var got string
	handler := New(Options{
		Plugins: func() []model.PluginStatus {
			return []model.PluginStatus{
				pluginStub("codearts", true, []string{model.CapLoginOAuth, model.CapLoginPasteCode}, false),
			}
		},
		AccountLoginStart: func(pluginID, method string) (model.LoginStart, error) {
			got = method
			return model.LoginStart{SessionID: "ls_paste", Status: model.LoginPending}, nil
		},
	})
	recorder := doRequest(t, handler, http.MethodPost, "/api/accounts/login",
		`{"plugin_id":"codearts","method":"paste_code"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("paste_code login start = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	if got != "paste_code" {
		t.Fatalf("forwarded method = %q", got)
	}
}

func TestAccountLoginSession(t *testing.T) {
	polls := 0
	var submitted map[string]string
	cancels := 0
	handler := New(Options{
		Plugins: func() []model.PluginStatus {
			return []model.PluginStatus{
				pluginStub("codearts", true, []string{model.CapLoginAPIKey}, false),
				pluginStub("stopped", false, nil, false),
			}
		},
		AccountLoginPoll: func(pluginID, sessionID string) (model.LoginStatus, error) {
			polls++
			if sessionID == "ls_boom" {
				return model.LoginStatus{}, errors.New("login session expired")
			}
			return model.LoginStatus{Status: model.LoginSuccess, AccountID: "acc_1"}, nil
		},
		AccountLoginSubmit: func(pluginID, sessionID string, values map[string]string) (model.LoginStatus, error) {
			submitted = values
			if sessionID == "ls_boom" {
				return model.LoginStatus{}, errors.New("api key rejected")
			}
			return model.LoginStatus{Status: model.LoginSuccess, AccountID: "acc_1"}, nil
		},
		AccountLoginCancel: func(pluginID, sessionID string) error {
			cancels++
			if sessionID == "ls_boom" {
				return errors.New("plugin refused to cancel")
			}
			return nil
		},
	})

	recorder := doRequest(t, handler, http.MethodGet, "/api/accounts/login/ls_1?plugin=codearts", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("poll = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var status model.LoginStatus
	decodeJSONBody(t, recorder, &status)
	if status.Status != model.LoginSuccess || status.AccountID != "acc_1" || polls != 1 {
		t.Fatalf("poll payload = %+v (calls %d)", status, polls)
	}

	recorder = doRequest(t, handler, http.MethodGet, "/api/accounts/login/ls_1", "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("poll without plugin = %d, want 400", recorder.Code)
	}
	recorder = doRequest(t, handler, http.MethodGet, "/api/accounts/login/ls_1?plugin=ghost", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("poll unknown plugin = %d, want 404", recorder.Code)
	}
	recorder = doRequest(t, handler, http.MethodGet, "/api/accounts/login/ls_1?plugin=stopped", "")
	if recorder.Code != http.StatusConflict || errorType(t, recorder) != "plugin_not_running" {
		t.Fatalf("poll stopped plugin = %d", recorder.Code)
	}
	recorder = doRequest(t, handler, http.MethodGet, "/api/accounts/login/ls_boom?plugin=codearts", "")
	if recorder.Code != http.StatusBadGateway || errorType(t, recorder) != "plugin_error" {
		t.Fatalf("poll failure = %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "login session expired") {
		t.Fatalf("poll error message missing: %s", recorder.Body.String())
	}

	// Submit forwards values untouched and reports the resulting status.
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/login/ls_1/submit",
		`{"plugin_id":"codearts","values":{"api_key":"sk-test"}}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("submit = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if submitted["api_key"] != "sk-test" {
		t.Fatalf("submitted values = %+v", submitted)
	}
	decodeJSONBody(t, recorder, &status)
	if status.Status != model.LoginSuccess || status.AccountID != "acc_1" {
		t.Fatalf("submit payload = %+v", status)
	}

	for _, body := range []string{
		`{"plugin_id":"codearts"}`,
		`{"plugin_id":"codearts","values":{}}`,
		`{"values":{"api_key":"sk-test"}}`,
		`{"plugin_id":"codearts","values":"nope"}`,
	} {
		recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/login/ls_1/submit", body)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("submit %s = %d, want 400 (body %s)", body, recorder.Code, recorder.Body.String())
		}
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/login/ls_1/submit",
		`{"plugin_id":"ghost","values":{"api_key":"sk-test"}}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("submit unknown plugin = %d, want 404", recorder.Code)
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/login/ls_1/submit",
		`{"plugin_id":"stopped","values":{"api_key":"sk-test"}}`)
	if recorder.Code != http.StatusConflict || errorType(t, recorder) != "plugin_not_running" {
		t.Fatalf("submit stopped plugin = %d", recorder.Code)
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/login/ls_boom/submit",
		`{"plugin_id":"codearts","values":{"api_key":"sk-test"}}`)
	if recorder.Code != http.StatusBadGateway || errorType(t, recorder) != "plugin_error" {
		t.Fatalf("submit failure = %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "api key rejected") {
		t.Fatalf("submit error message missing: %s", recorder.Body.String())
	}

	// Cancel returns ok and never echoes the session id back.
	recorder = doRequest(t, handler, http.MethodDelete, "/api/accounts/login/ls_1?plugin=codearts", "")
	if recorder.Code != http.StatusOK || cancels != 1 {
		t.Fatalf("cancel = %d (calls %d)", recorder.Code, cancels)
	}
	var cancelled map[string]bool
	decodeJSONBody(t, recorder, &cancelled)
	if !cancelled["ok"] {
		t.Fatalf("cancel payload = %+v", cancelled)
	}
	recorder = doRequest(t, handler, http.MethodDelete, "/api/accounts/login/ls_1", "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("cancel without plugin = %d, want 400", recorder.Code)
	}
	recorder = doRequest(t, handler, http.MethodDelete, "/api/accounts/login/ls_1?plugin=ghost", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("cancel unknown plugin = %d, want 404", recorder.Code)
	}
	recorder = doRequest(t, handler, http.MethodDelete, "/api/accounts/login/ls_boom?plugin=codearts", "")
	if recorder.Code != http.StatusBadGateway || errorType(t, recorder) != "plugin_error" {
		t.Fatalf("cancel failure = %d", recorder.Code)
	}
	recorder = doRequest(t, handler, http.MethodDelete, "/api/accounts/login/ls_1?plugin=stopped", "")
	if recorder.Code != http.StatusConflict {
		t.Fatalf("cancel stopped plugin = %d, want 409", recorder.Code)
	}

	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/login/ls_1", "{}")
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST login session = %d, want 405", recorder.Code)
	}
}

func TestAccountMutations(t *testing.T) {
	var patched map[string]any
	deleted := ""
	refreshed := ""
	handler := New(Options{
		Plugins: func() []model.PluginStatus {
			return []model.PluginStatus{
				pluginStub("codearts", true, []string{model.CapRefresh}, false),
				pluginStub("stopped", false, nil, false),
			}
		},
		AccountPatch: func(pluginID, accountID string, patch map[string]any) error {
			patched = patch
			switch accountID {
			case "acc_boom":
				return errors.New("account is locked")
			case "acc_not_running":
				return fmt.Errorf("%w: %s", ErrPluginNotRunning, pluginID)
			}
			return nil
		},
		AccountDelete: func(pluginID, accountID string) error {
			deleted = accountID
			switch accountID {
			case "acc_boom":
				return errors.New("cannot delete account")
			case "acc_missing":
				return fmt.Errorf("%w: %s", ErrPluginNotFound, pluginID)
			}
			return nil
		},
		AccountRefresh: func(pluginID, accountID string) (model.RefreshResult, error) {
			refreshed = accountID
			if accountID == "acc_boom" {
				return model.RefreshResult{}, errors.New("refresh rejected")
			}
			return model.RefreshResult{Result: "ok", Message: "续期成功"}, nil
		},
	})

	// Patch: enable/disable toggles are relayed verbatim.
	recorder := doRequest(t, handler, http.MethodPost, "/api/accounts/patch",
		`{"plugin_id":"codearts","account_id":"acc_1","patch":{"enabled":false}}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("patch = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if value, ok := patched["enabled"]; !ok || value != false {
		t.Fatalf("patch payload = %+v", patched)
	}
	for _, body := range []string{
		`{"plugin_id":"codearts","patch":{"enabled":false}}`,
		`{"plugin_id":"codearts","account_id":"acc_1","patch":{}}`,
		`{"plugin_id":"codearts","account_id":"acc_1"}`,
	} {
		recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/patch", body)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("patch %s = %d, want 400 (body %s)", body, recorder.Code, recorder.Body.String())
		}
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/patch",
		`{"plugin_id":"ghost","account_id":"acc_1","patch":{"enabled":false}}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("patch unknown plugin = %d, want 404", recorder.Code)
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/patch",
		`{"plugin_id":"stopped","account_id":"acc_1","patch":{"enabled":false}}`)
	if recorder.Code != http.StatusConflict || errorType(t, recorder) != "plugin_not_running" {
		t.Fatalf("patch stopped plugin = %d", recorder.Code)
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/patch",
		`{"plugin_id":"codearts","account_id":"acc_boom","patch":{"enabled":true}}`)
	if recorder.Code != http.StatusBadGateway || errorType(t, recorder) != "plugin_error" {
		t.Fatalf("patch failure = %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "account is locked") {
		t.Fatalf("patch error message missing: %s", recorder.Body.String())
	}
	// Adapter-wrapped runtime errors keep their status.
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/patch",
		`{"plugin_id":"codearts","account_id":"acc_not_running","patch":{"enabled":true}}`)
	if recorder.Code != http.StatusConflict || errorType(t, recorder) != "plugin_not_running" {
		t.Fatalf("patch wrapped not-running = %d", recorder.Code)
	}

	// Delete.
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/delete",
		`{"plugin_id":"codearts","account_id":"acc_1"}`)
	if recorder.Code != http.StatusOK || deleted != "acc_1" {
		t.Fatalf("delete = %d (deleted %q)", recorder.Code, deleted)
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/delete",
		`{"plugin_id":"codearts"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("delete without account = %d, want 400", recorder.Code)
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/delete",
		`{"plugin_id":"ghost","account_id":"acc_1"}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("delete unknown plugin = %d, want 404", recorder.Code)
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/delete",
		`{"plugin_id":"stopped","account_id":"acc_1"}`)
	if recorder.Code != http.StatusConflict || errorType(t, recorder) != "plugin_not_running" {
		t.Fatalf("delete stopped plugin = %d", recorder.Code)
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/delete",
		`{"plugin_id":"codearts","account_id":"acc_boom"}`)
	if recorder.Code != http.StatusBadGateway || errorType(t, recorder) != "plugin_error" {
		t.Fatalf("delete failure = %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "cannot delete account") {
		t.Fatalf("delete error message missing: %s", recorder.Body.String())
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/delete",
		`{"plugin_id":"codearts","account_id":"acc_missing"}`)
	if recorder.Code != http.StatusNotFound || errorType(t, recorder) != "not_found" {
		t.Fatalf("delete wrapped not-found = %d", recorder.Code)
	}

	// Refresh.
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/refresh",
		`{"plugin_id":"codearts","account_id":"acc_1"}`)
	if recorder.Code != http.StatusOK || refreshed != "acc_1" {
		t.Fatalf("refresh = %d (refreshed %q)", recorder.Code, refreshed)
	}
	var payload struct {
		OK     bool                `json:"ok"`
		Result model.RefreshResult `json:"result"`
	}
	decodeJSONBody(t, recorder, &payload)
	if !payload.OK || payload.Result.Result != "ok" || payload.Result.Message != "续期成功" {
		t.Fatalf("refresh payload = %+v", payload)
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/refresh",
		`{"plugin_id":"codearts"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("refresh without account = %d, want 400", recorder.Code)
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/refresh",
		`{"plugin_id":"ghost","account_id":"acc_1"}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("refresh unknown plugin = %d, want 404", recorder.Code)
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/refresh",
		`{"plugin_id":"stopped","account_id":"acc_1"}`)
	if recorder.Code != http.StatusConflict || errorType(t, recorder) != "plugin_not_running" {
		t.Fatalf("refresh stopped plugin = %d", recorder.Code)
	}
	recorder = doRequest(t, handler, http.MethodPost, "/api/accounts/refresh",
		`{"plugin_id":"codearts","account_id":"acc_boom"}`)
	if recorder.Code != http.StatusBadGateway || errorType(t, recorder) != "plugin_error" {
		t.Fatalf("refresh failure = %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "refresh rejected") {
		t.Fatalf("refresh error message missing: %s", recorder.Body.String())
	}

	// The new subpaths must not shadow the account list route.
	handlerList := New(Options{Accounts: func(pluginID string) any {
		return map[string]any{"accounts": []model.Account{}}
	}})
	recorder = doRequest(t, handlerList, http.MethodGet, "/api/accounts?plugin=codearts", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/accounts = %d, want 200", recorder.Code)
	}
}

func TestPutProxyDoesNotEchoThePassword(t *testing.T) {
	proxies := []model.Proxy{{ID: "px_test", Name: "hk", Type: "socks5", Host: "127.0.0.1", Port: 1080, Enabled: true}}
	handler := New(Options{
		Proxies: func() []model.Proxy { return proxies },
		SaveProxy: func(proxy model.Proxy) error {
			for index := range proxies {
				if proxies[index].ID == proxy.ID {
					proxies[index] = proxy
					return nil
				}
			}
			return nil
		},
	})
	recorder := doRequest(t, handler, http.MethodPut, "/api/proxies/px_test",
		`{"name":"hk","type":"socks5","host":"127.0.0.1","port":1080,"enabled":true,"password":"sekrit"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("put proxy = %d body %s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Proxy model.Proxy `json:"proxy"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("put body: %v", err)
	}
	if body.Proxy.Password != "" {
		t.Fatalf("PUT echoed the password: %q", body.Proxy.Password)
	}
}
