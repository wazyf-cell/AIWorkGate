package webui

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"aiworkgate/internal/model"
)

// guardOptions wires the four endpoints the pass-through test exercises.
func guardOptions() Options {
	return Options{
		Status: func() model.HostStatus { return model.HostStatus{Version: "test"} },
		Keys:   func() []model.ClientKey { return []model.ClientKey{{ID: "ck_1", Name: "laptop"}} },
		CreateKey: func(name string) (string, model.ClientKey, error) {
			return "agk_secret", model.ClientKey{ID: "ck_2", Name: name, Prefix: "agk_sec"}, nil
		},
		PoolDetail: func(pluginID string) (any, error) {
			return map[string]any{"plugin_id": pluginID}, nil
		},
		LogTimeline: func(query map[string][]string) (any, error) {
			return map[string]any{"items": []any{}, "total": 0}, nil
		},
	}
}

type guardRequest struct {
	method  string
	target  string
	body    string
	host    string
	origin  string
	referer string
	fetch   string
	cookie  *http.Cookie
}

// serveGuard runs one request through the guarded handler.
func serveGuard(t *testing.T, handler http.Handler, tc guardRequest) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if tc.body != "" {
		body = strings.NewReader(tc.body)
	}
	request := httptest.NewRequest(tc.method, tc.target, body)
	if tc.body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	// A browser typing http://127.0.0.1:7865 sends both the Host and the Origin
	// in that loopback form, so the default example.com is replaced.
	if tc.host != "" {
		request.Host = tc.host
	} else if tc.origin != "" || tc.referer != "" || tc.fetch != "" {
		request.Host = "127.0.0.1:7865"
	}
	if tc.origin != "" {
		request.Header.Set("Origin", tc.origin)
	}
	if tc.referer != "" {
		request.Header.Set("Referer", tc.referer)
	}
	if tc.fetch != "" {
		request.Header.Set("Sec-Fetch-Site", tc.fetch)
	}
	if tc.cookie != nil {
		request.AddCookie(tc.cookie)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func guardErrorType(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	decodeJSONBody(t, recorder, &payload)
	if payload.Error.Message == "" {
		t.Fatalf("rejection body %q has no message", recorder.Body.String())
	}
	return payload.Error.Type
}

func TestGuardRejectsForeignOriginAndReferer(t *testing.T) {
	handler := New(Options{})
	cookie := hostSessionCookieFor(t, handler)

	cases := []struct {
		name    string
		foreign guardRequest
	}{
		{"foreign origin", guardRequest{
			method: http.MethodPost, target: "/api/keys", body: `{"name":"pwned"}`,
			origin: "http://evil.example", cookie: cookie,
		}},
		{"foreign referer", guardRequest{
			method: http.MethodPost, target: "/api/keys", body: `{"name":"pwned"}`,
			referer: "http://evil.example/form.html", cookie: cookie,
		}},
		{"loopback but wrong port", guardRequest{
			method: http.MethodPost, target: "/api/settings", body: `{"port":1}`,
			origin: "http://127.0.0.1:9999", cookie: cookie,
		}},
		{"localhost versus loopback", guardRequest{
			method: http.MethodPut, target: "/api/settings", body: `{"port":1}`,
			origin: "http://localhost:7865", cookie: cookie,
		}},
		{"scheme downgrade", guardRequest{
			method: http.MethodPost, target: "/api/plugins/install", body: `{"zip_path":"C:\\x.zip"}`,
			origin: "https://127.0.0.1:7865", cookie: cookie,
		}},
		{"malformed origin", guardRequest{
			method: http.MethodPost, target: "/api/keys", body: `{"name":"pwned"}`,
			origin: "not a url", cookie: cookie,
		}},
	}

	for _, tc := range cases {
		recorder := serveGuard(t, handler, tc.foreign)
		if recorder.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403 (body %s)", tc.name, recorder.Code, recorder.Body.String())
			continue
		}
		if kind := guardErrorType(t, recorder); kind != "cross_origin" {
			t.Errorf("%s: error type = %q, want cross_origin", tc.name, kind)
		}
	}
}

func TestGuardRejectsCrossSiteSecFetchSite(t *testing.T) {
	handler := New(guardOptions())
	cookie := hostSessionCookieFor(t, handler)
	allowed := guardRequest{
		method: http.MethodGet, target: "/api/status",
		origin: "http://127.0.0.1:7865", fetch: "same-origin", cookie: cookie,
	}

	for _, site := range []string{"cross-site", "same-site", "Cross-Site"} {
		tc := allowed
		tc.fetch = site
		recorder := serveGuard(t, handler, tc)
		if recorder.Code != http.StatusForbidden {
			t.Errorf("Sec-Fetch-Site %q: status = %d, want 403", site, recorder.Code)
			continue
		}
		if kind := guardErrorType(t, recorder); kind != "cross_origin" {
			t.Errorf("Sec-Fetch-Site %q: error type = %q, want cross_origin", site, kind)
		}
	}

	for _, site := range []string{"same-origin", "none", ""} {
		tc := allowed
		tc.fetch = site
		if recorder := serveGuard(t, handler, tc); recorder.Code != http.StatusOK {
			t.Errorf("Sec-Fetch-Site %q: status = %d, want 200 (body %s)", site, recorder.Code, recorder.Body.String())
		}
	}
}

func TestGuardRequiresSessionCookie(t *testing.T) {
	handler := New(guardOptions())
	cookie := hostSessionCookieFor(t, handler)

	t.Run("reads no longer bootstrap the session", func(t *testing.T) {
		// Reversed on purpose: a bare GET used to earn the cookie, which made it
		// a CSRF token any local process could fetch instead of a credential.
		// Every method, reads included, now answers 403 without a cookie.
		for _, tc := range []guardRequest{
			{method: http.MethodGet, target: "/api/status"},
			{method: http.MethodGet, target: "/api/logs/timeline"},
			{method: http.MethodPost, target: "/api/keys", body: `{"name":"pwned"}`,
				origin: "http://127.0.0.1:7865", referer: "http://127.0.0.1:7865/settings"},
			{method: http.MethodPost, target: "/api/plugins/install", body: `{}`},
		} {
			recorder := serveGuard(t, handler, tc)
			if recorder.Code != http.StatusForbidden {
				t.Errorf("%s %s: status = %d, want 403 (body %s)",
					tc.method, tc.target, recorder.Code, recorder.Body.String())
				continue
			}
			if kind := guardErrorType(t, recorder); kind != "admin_session_required" {
				t.Errorf("%s %s: error type = %q, want admin_session_required", tc.method, tc.target, kind)
			}
			if servesSessionCookie(recorder) {
				t.Errorf("%s %s: a rejected request still handed out the session cookie", tc.method, tc.target)
			}
		}
	})

	t.Run("stale or wrong cookie value is still a valid session source for writes", func(t *testing.T) {
		stale := *cookie
		stale.Value = strings.Repeat("0", len(cookie.Value))
		for _, value := range []string{stale.Value, "0", cookie.Value[:63], ""} {
			tc := guardRequest{method: http.MethodPost, target: "/api/keys", body: `{"name":"pwned"}`,
				cookie: &http.Cookie{Name: hostSessionCookie, Value: value, Path: "/"}}
			recorder := serveGuard(t, handler, tc)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("cookie value %q: status = %d, want 403", value, recorder.Code)
			}
			if kind := guardErrorType(t, recorder); kind != "admin_session_required" {
				t.Fatalf("cookie value %q: error type = %q, want admin_session_required", value, kind)
			}
		}
	})

	t.Run("token in the query string is not accepted", func(t *testing.T) {
		recorder := serveGuard(t, handler, guardRequest{
			method: http.MethodPost, target: "/api/keys?" + hostSessionCookie + "=" + cookie.Value,
			body: `{"name":"pwned"}`,
		})
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("query-string token: status = %d, want 403", recorder.Code)
		}
		if kind := guardErrorType(t, recorder); kind != "admin_session_required" {
			t.Fatalf("query-string token: error type = %q, want admin_session_required", kind)
		}
		if strings.Contains(recorder.Body.String(), cookie.Value) {
			t.Errorf("rejection body %q echoes the token", recorder.Body.String())
		}
	})

	t.Run("guard is scoped to /api", func(t *testing.T) {
		// internal/app routes /v1 to the gateway before this handler, so the
		// guard must never be the reason a gateway request fails; the SPA shell
		// and its assets stay readable without a session.
		for _, target := range []string{"/", "/settings", "/v1/chat/completions"} {
			recorder := serveGuard(t, handler, guardRequest{method: http.MethodGet, target: target})
			if recorder.Code != http.StatusOK {
				t.Errorf("GET %s: status = %d, want 200", target, recorder.Code)
			}
		}
		recorder := serveGuard(t, handler, guardRequest{method: http.MethodDelete, target: "/api/keys/ck_nope"})
		if recorder.Code != http.StatusForbidden {
			t.Errorf("DELETE /api/keys without a cookie: status = %d, want 403", recorder.Code)
		}
	})
}

// servesSessionCookie reports whether the response hands out the admin cookie.
func servesSessionCookie(recorder *httptest.ResponseRecorder) bool {
	for _, c := range recorder.Result().Cookies() {
		if c.Name == hostSessionCookie {
			return true
		}
	}
	return false
}

func TestSessionCookieAttributes(t *testing.T) {
	handler := New(Options{})
	// The shell no longer issues the cookie; the launch-token exchange does.
	document := serveGuard(t, handler, guardRequest{method: http.MethodGet, target: "/"})
	if document.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", document.Code)
	}
	if set := document.Header().Values("Set-Cookie"); len(set) != 0 {
		t.Fatalf("the SPA document still sets cookies %v; it must set none", set)
	}

	exchange := serveGuard(t, handler, guardRequest{
		method: http.MethodPost, target: sessionEndpoint + "?" + launchTokenParam + "=" + launchTokenOf(t, handler),
	})
	if exchange.Code != http.StatusOK {
		t.Fatalf("POST %s = %d, want 200 (body %s)", sessionEndpoint, exchange.Code, exchange.Body.String())
	}
	cookies := exchange.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != hostSessionCookie {
		t.Fatalf("session exchange cookies = %+v, want exactly one %s", cookies, hostSessionCookie)
	}
	cookie := cookies[0]
	if !cookie.HttpOnly {
		t.Error("session cookie is readable from page scripts (HttpOnly missing)")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("SameSite = %v, want Strict", cookie.SameSite)
	}
	if cookie.Secure {
		t.Error("Secure set: the admin UI is served over plain http on loopback")
	}
	if cookie.Path != "/" {
		t.Errorf("Path = %q, want /", cookie.Path)
	}
	if cookie.MaxAge != 0 || !cookie.Expires.IsZero() {
		t.Errorf("MaxAge = %d Expires = %v, want a session cookie", cookie.MaxAge, cookie.Expires)
	}
	if raw, err := hex.DecodeString(cookie.Value); err != nil || len(raw) != sessionTokenBytes {
		t.Errorf("cookie value %q is not %d random hex bytes", cookie.Value, sessionTokenBytes)
	}

	// The token lives in memory only: a second host start must not reuse it.
	other := New(Options{})
	if launchTokenOf(t, other) == launchTokenOf(t, handler) {
		t.Error("two host starts share one session token")
	}
	// A token from one process must not open another's admin API.
	recorder := serveGuard(t, other, guardRequest{
		method: http.MethodPost, target: sessionEndpoint + "?" + launchTokenParam + "=" + launchTokenOf(t, handler),
	})
	if recorder.Code != http.StatusForbidden {
		t.Errorf("a foreign launch token = %d, want 403", recorder.Code)
	}
}

func TestGuardKeepsSameOriginStatusesUnchanged(t *testing.T) {
	handler := New(guardOptions())
	cookie := hostSessionCookieFor(t, handler)
	base := guardRequest{origin: "http://127.0.0.1:7865", referer: "http://127.0.0.1:7865/settings",
		fetch: "same-origin", cookie: cookie}

	cases := []struct {
		method, target, body string
		want                 int
	}{
		{http.MethodGet, "/api/status", "", http.StatusOK},
		{http.MethodPost, "/api/keys", `{"name":"laptop"}`, http.StatusCreated},
		{http.MethodGet, "/api/pools/mock", "", http.StatusOK},
		{http.MethodGet, "/api/logs/timeline?type=api", "", http.StatusOK},
		{http.MethodGet, "/api/keys", "", http.StatusOK},
		{http.MethodGet, "/api/does-not-exist", "", http.StatusNotFound},
		{http.MethodGet, "/api/settings", "", http.StatusNotImplemented},
		{http.MethodPatch, "/api/status", "", http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		request := base
		request.method, request.target, request.body = tc.method, tc.target, tc.body
		recorder := serveGuard(t, handler, request)
		if recorder.Code != tc.want {
			t.Errorf("%s %s: status = %d, want %d (body %s)", tc.method, tc.target, recorder.Code, tc.want, recorder.Body.String())
			continue
		}
		// Statuses that reach a handler must still carry its payload shape.
		if tc.want < 300 && strings.HasPrefix(recorder.Header().Get("Content-Type"), "application/json") {
			var probe any
			if err := json.Unmarshal(recorder.Body.Bytes(), &probe); err != nil {
				t.Errorf("%s %s: body %q is not the expected JSON: %v", tc.method, tc.target, recorder.Body.String(), err)
			}
		}
	}

	// Every loopback spelling a browser can put in both the Host header and the
	// Origin is accepted, including the default-port forms.
	allowed := []struct{ host, origin string }{
		{"127.0.0.1", "http://127.0.0.1"},
		{"127.0.0.1:7865", "http://127.0.0.1:7865"},
		{"localhost:7865", "http://localhost:7865"},
		{"LOCALHOST:7865", "http://localhost:7865"},
		{"[::1]:7865", "http://[::1]:7865"},
		{"192.168.1.20:7865", "http://192.168.1.20:7865"},
	}
	for _, entry := range allowed {
		recorder := serveGuard(t, handler, guardRequest{
			method: http.MethodPost, target: "/api/keys", body: `{"name":"phone"}`,
			host: entry.host, origin: entry.origin, referer: entry.origin + "/",
			cookie: cookie,
		})
		if recorder.Code != http.StatusCreated {
			t.Errorf("host %q origin %q: status = %d, want 201 (body %s)",
				entry.host, entry.origin, recorder.Code, recorder.Body.String())
		}
	}
}

// TestGuardRealClientCookieFlow drives the guard over a real TCP server with a
// cookie jar, which is the closest reach a browser has: the page trades the
// launch token for the cookie, the API calls (including the SSE tail) then work,
// and an attacker page that cannot read the HttpOnly cookie is stopped.
func TestGuardRealClientCookieFlow(t *testing.T) {
	handler := New(guardOptions())
	host := httptest.NewServer(handler)
	defer host.Close()

	browser := &http.Client{}
	browser.Jar, _ = cookiejar.New(nil)

	document, err := browser.Get(host.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	_, _ = io.Copy(io.Discard, document.Body)
	_ = document.Body.Close()
	if document.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", document.StatusCode)
	}

	// A plain document load no longer authorises anything: this is the fix.
	bare, err := browser.Get(host.URL + "/api/status")
	if err != nil {
		t.Fatalf("GET /api/status without a session: %v", err)
	}
	bareBody, _ := io.ReadAll(bare.Body)
	_ = bare.Body.Close()
	if bare.StatusCode != http.StatusForbidden || !strings.Contains(string(bareBody), "admin_session_required") {
		t.Fatalf("cookie-less GET /api/status = %d (%s), want 403 admin_session_required", bare.StatusCode, bareBody)
	}

	// The launch URL is what earns the cookie. It goes through the same client
	// (and jar) the API calls use, exactly like a page load would.
	exchangeRequest, err := http.NewRequest(http.MethodPost,
		host.URL+sessionEndpoint+"?"+launchTokenParam+"="+launchTokenOf(t, handler), nil)
	if err != nil {
		t.Fatalf("build session exchange: %v", err)
	}
	exchange, err := browser.Do(exchangeRequest)
	if err != nil {
		t.Fatalf("POST %s: %v", sessionEndpoint, err)
	}
	_, _ = io.Copy(io.Discard, exchange.Body)
	_ = exchange.Body.Close()
	if exchange.StatusCode != http.StatusOK {
		t.Fatalf("POST %s = %d, want 200", sessionEndpoint, exchange.StatusCode)
	}

	for _, tc := range []struct {
		method, path string
		body         io.Reader
		want         int
	}{
		{http.MethodGet, "/api/status", nil, http.StatusOK},
		{http.MethodGet, "/api/logs/stream", nil, http.StatusNotImplemented},
		{http.MethodPost, "/api/keys", strings.NewReader(`{"name":"laptop"}`), http.StatusCreated},
	} {
		request, err := http.NewRequest(tc.method, host.URL+tc.path, tc.body)
		if err != nil {
			t.Fatalf("build %s %s: %v", tc.method, tc.path, err)
		}
		if tc.body != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		response, err := browser.Do(request)
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != tc.want {
			t.Errorf("browser %s %s = %d, want %d", tc.method, tc.path, response.StatusCode, tc.want)
		}
	}

	// A cross-site page sees the same endpoints as unauthenticated: it cannot
	// attach the cookie, and a forged Origin still stops at the session check.
	attacker := &http.Client{}
	for _, origin := range []string{"http://evil.example", host.URL} {
		request, err := http.NewRequest(http.MethodPost, host.URL+"/api/keys",
			strings.NewReader(`{"name":"pwned"}`))
		if err != nil {
			t.Fatalf("build CSRF request: %v", err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", origin)
		response, err := attacker.Do(request)
		if err != nil {
			t.Fatalf("attacker POST /api/keys (origin %s): %v", origin, err)
		}
		payload, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("attacker origin %s = %d, want 403 (body %s)", origin, response.StatusCode, payload)
		}
		want := "cross_origin"
		if origin != "http://evil.example" {
			want = "admin_session_required"
		}
		if !strings.Contains(string(payload), want) {
			t.Errorf("attacker origin %s body %q, want code %s", origin, payload, want)
		}
	}
}

// TestSessionTokenNeverReachesOutput asserts the admin session token cannot be
// recovered from anything this package writes: response bodies and headers, the
// request URL a host adapter may log, the query maps handed to the Options
// callbacks, or the process loggers.
func TestSessionTokenNeverReachesOutput(t *testing.T) {
	var captured strings.Builder
	logBuffer := &bytes.Buffer{}
	slogBuffer := &bytes.Buffer{}
	previousWriter, previousStd := log.Writer(), slog.Default()
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		slog.SetDefault(previousStd)
	})
	log.SetOutput(logBuffer)
	slog.SetDefault(slog.New(slog.NewTextHandler(slogBuffer, nil)))

	handler := New(Options{
		Status: func() model.HostStatus {
			fmt.Fprint(&captured, "status")
			return model.HostStatus{Version: "test"}
		},
		RequestLogs: func(query map[string]string) any {
			fmt.Fprintf(&captured, "request logs %v\n", query)
			return []any{}
		},
		LogTimeline: func(query map[string][]string) (any, error) {
			fmt.Fprintf(&captured, "timeline %v\n", query)
			return map[string]any{"items": []any{}}, nil
		},
		UsageSummary: func(query map[string][]string) (any, error) {
			fmt.Fprintf(&captured, "usage %v\n", query)
			return map[string]any{}, nil
		},
		RuntimeLogs: func(source string, n int) []string {
			fmt.Fprintf(&captured, "runtime %s %d\n", source, n)
			return []string{"one line"}
		},
	})
	cookie := hostSessionCookieFor(t, handler)
	token := cookie.Value

	// seenURLs records the URL exactly as the handlers receive it: the token has
	// to stay in the cookie header, never in a path or query a host can log.
	var seenURLs strings.Builder
	recordURLs := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(&seenURLs, "%s %s\n", r.Method, r.URL.String())
		handler.ServeHTTP(w, r)
	})

	requests := []guardRequest{
		{method: http.MethodGet, target: "/api/status", cookie: cookie},
		{method: http.MethodGet, target: "/api/logs/requests?limit=10", cookie: cookie},
		{method: http.MethodGet, target: "/api/logs/timeline?type=api&category=audit", cookie: cookie},
		{method: http.MethodGet, target: "/api/usage/summary?from=2026-10-01", cookie: cookie},
		{method: http.MethodGet, target: "/api/logs/runtime?source=host&n=5", cookie: cookie},
		{method: http.MethodGet, target: "/api/keys", cookie: cookie},
		{method: http.MethodPost, target: "/api/keys", body: `{`, cookie: cookie},
		{method: http.MethodGet, target: "/api/does-not-exist-" + hostSessionCookie, cookie: cookie},
		{method: http.MethodPost, target: "/api/keys", body: `{"name":"x"}`, origin: "http://evil.example"},
	}
	for _, tc := range requests {
		recorder := serveGuard(t, recordURLs, tc)
		if strings.Contains(recorder.Body.String(), token) {
			t.Errorf("body of %s %s leaks the token: %s", tc.method, tc.target, recorder.Body.String())
		}
		if values := recorder.Header().Values("Set-Cookie"); len(values) != 0 {
			t.Errorf("%s %s set cookies %v", tc.method, tc.target, values)
		}
	}
	// The session exchange is the only place the token may travel through, and
	// it must appear in the cookie it sets - not in the body.
	exchange := serveGuard(t, handler, guardRequest{
		method: http.MethodPost, target: sessionEndpoint + "?" + launchTokenParam + "=" + token,
	})
	set := exchange.Header().Values("Set-Cookie")
	if len(set) != 1 || !strings.Contains(set[0], token) {
		t.Errorf("session Set-Cookie = %v, want the session cookie", set)
	}
	if strings.Contains(exchange.Body.String(), token) {
		t.Errorf("session body %q echoes the token", exchange.Body.String())
	}
	// The document must hand out nothing at all any more.
	document := serveGuard(t, handler, guardRequest{method: http.MethodGet, target: "/"})
	if set := document.Header().Values("Set-Cookie"); len(set) != 0 {
		t.Errorf("document Set-Cookie = %v, want none", set)
	}

	for name, output := range map[string]string{
		"host log":  logBuffer.String(),
		"slog":      slogBuffer.String(),
		"callbacks": captured.String(),
		"urls":      seenURLs.String(),
	} {
		if strings.Contains(output, token) {
			t.Errorf("%s output contains the session token: %q", name, output)
		}
	}
}
