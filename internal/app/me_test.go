package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aiworkgate/internal/config"
	"aiworkgate/internal/model"
	"aiworkgate/internal/secrets"
	"aiworkgate/internal/store"
)

// P14: /me is key-scoped, re-checks the key every request, and never exposes
// bodies or other keys' data.
func newMeTestApp(t *testing.T) (*App, *meHandler, string, string) {
	t.Helper()
	app, st := openAppWithStore(t)
	sec, err := secrets.Open(t.TempDir())
	if err != nil {
		t.Fatalf("secrets.Open: %v", err)
	}
	app.secrets = sec
	app.dataDir = t.TempDir()
	app.cfg = config.Default()
	app.me = newMeHandler(app)

	plain, id, prefix, hash := secrets.NewClientKey()
	if err := sec.AddClientKey(id, hash); err != nil {
		t.Fatalf("AddClientKey: %v", err)
	}
	scope := store.ClientKeyScope{
		ModelScope: []string{"p1/*"}, DailyQuota: 5, MaxRequestsTotal: 100,
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}
	if err := st.UpsertClientKeyScoped(id, "friend", prefix, hash, true, 100, scope); err != nil {
		t.Fatalf("UpsertClientKeyScoped: %v", err)
	}
	if err := st.IncrementKeyUsage(id, store.ClientKeyUsageDay(time.Now()), 3, 10, 5, 0, nil); err != nil {
		t.Fatalf("IncrementKeyUsage: %v", err)
	}
	if err := st.InsertAPIRequestLog(model.APIRequestLog{
		TS: time.Now().Format(time.RFC3339), ReqID: "r1", Source: "plugin:p1", Model: "m1",
		KeyID: id, Status: 200, PromptTokens: 10, CompletionTokens: 5, LatencyMS: 42,
		BodyReq: "SECRET-BODY", BodyResp: "SECRET-RESP",
	}); err != nil {
		t.Fatalf("InsertAPIRequestLog: %v", err)
	}
	return app, app.me, plain, id
}

func meRequest(t *testing.T, h http.Handler, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func meLogin(t *testing.T, h http.Handler, plain string) *http.Cookie {
	t.Helper()
	rec := meRequest(t, h, http.MethodPost, "/me/api/session", `{"key":"`+plain+`"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d (%s)", rec.Code, rec.Body.String())
	}
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == meCookieName {
			return cookie
		}
	}
	t.Fatalf("login did not set the session cookie")
	return nil
}

func TestMeOverviewScopedAndBodyFree(t *testing.T) {
	_, h, plain, _ := newMeTestApp(t)
	cookie := meLogin(t, h, plain)

	rec := meRequest(t, h, http.MethodGet, "/me/api/me", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview = %d (%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "SECRET-BODY") || strings.Contains(body, "SECRET-RESP") || strings.Contains(body, "body_req") {
		t.Fatalf("overview leaked request/response content: %s", body)
	}
	var payload struct {
		Key struct {
			Name       string   `json:"name"`
			ModelScope []string `json:"model_scope"`
			UsedToday  int      `json:"used_today"`
		} `json:"key"`
		Today struct {
			Requests int `json:"requests"`
		} `json:"today"`
		Models []struct {
			Model string `json:"Model"`
		} `json:"models"`
		Logs []map[string]any `json:"logs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Key.Name != "friend" || len(payload.Key.ModelScope) != 1 || payload.Key.ModelScope[0] != "p1/*" {
		t.Fatalf("key view = %+v", payload.Key)
	}
	if payload.Today.Requests != 3 || len(payload.Models) != 1 || payload.Models[0].Model != "m1" {
		t.Fatalf("usage = today %d models %+v", payload.Today.Requests, payload.Models)
	}
	if len(payload.Logs) != 1 {
		t.Fatalf("logs = %+v", payload.Logs)
	}
}

func TestMeInvalidKeyAndExpiry(t *testing.T) {
	app, h, plain, id := newMeTestApp(t)
	if rec := meRequest(t, h, http.MethodPost, "/me/api/session", `{"key":"wrong"}`, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key = %d, want 401", rec.Code)
	}
	cookie := meLogin(t, h, plain)

	scope := store.ClientKeyScope{ModelScope: []string{"p1/*"}, ExpiresAt: time.Now().Add(-time.Minute).Unix()}
	if err := app.store.SetClientKeyScope(id, scope); err != nil {
		t.Fatalf("SetClientKeyScope: %v", err)
	}
	rec := meRequest(t, h, http.MethodGet, "/me/api/me", "", cookie)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "过期") {
		t.Fatalf("expired session = %d (%s), want 401 with 过期", rec.Code, rec.Body.String())
	}
}

func TestMeDisabledAndRevokedInvalidateSession(t *testing.T) {
	app, h, plain, id := newMeTestApp(t)
	cookie := meLogin(t, h, plain)

	if err := app.store.UpdateClientKey(id, "friend", false, store.ClientKeyScope{ModelScope: []string{"p1/*"}}); err != nil {
		t.Fatalf("UpdateClientKey: %v", err)
	}
	if rec := meRequest(t, h, http.MethodGet, "/me/api/me", "", cookie); rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled key = %d, want 401", rec.Code)
	}

	if err := app.store.UpdateClientKey(id, "friend", true, store.ClientKeyScope{}); err != nil {
		t.Fatalf("UpdateClientKey: %v", err)
	}
	if err := app.store.DeleteClientKey(id); err != nil {
		t.Fatalf("DeleteClientKey: %v", err)
	}
	if rec := meRequest(t, h, http.MethodGet, "/me/api/me", "", cookie); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key = %d, want 401", rec.Code)
	}
}

func TestRouteHTTPMeBypassesAdminGate(t *testing.T) {
	app, _, _, _ := newMeTestApp(t)
	req := httptest.NewRequest(http.MethodGet, "/me/", nil)
	req.Host = "localhost"
	rec := httptest.NewRecorder()
	app.routeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "AIWorkGate") {
		t.Fatalf("/me/ = %d (%s)", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/me/api/me", nil)
	req.Host = "localhost"
	rec = httptest.NewRecorder()
	app.routeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /me/api/me = %d, want 401 (not the admin gate)", rec.Code)
	}
}
