package webui

import (
	"net/http"
	"strings"
	"testing"
)

func TestSessionEndpointIsTheOnlyCookieLessRoute(t *testing.T) {
	const token = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	handler := New(Options{SessionToken: token})

	// The exchange works with the token in the query string.
	recorder := serveGuard(t, handler, guardRequest{
		method: http.MethodPost, target: sessionEndpoint + "?" + launchTokenParam + "=" + token,
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST %s = %d, want 200 (body %s)", sessionEndpoint, recorder.Code, recorder.Body.String())
	}
	cookie := recorder.Result().Cookies()
	if len(cookie) != 1 || cookie[0].Name != hostSessionCookie || cookie[0].Value != token {
		t.Fatalf("session cookies = %+v, want one %s with the launch token", cookie, hostSessionCookie)
	}

	// It also works with a JSON body.
	bodyRecorder := serveGuard(t, handler, guardRequest{
		method: http.MethodPost, target: sessionEndpoint, body: `{"token":"` + token + `"}`,
	})
	if bodyRecorder.Code != http.StatusOK {
		t.Fatalf("POST %s (body token) = %d, want 200 (body %s)",
			sessionEndpoint, bodyRecorder.Code, bodyRecorder.Body.String())
	}

	// Wrong, empty and foreign tokens all answer the same 403 and never echo the
	// value they were given.
	for _, tc := range []guardRequest{
		{method: http.MethodPost, target: sessionEndpoint},
		{method: http.MethodPost, target: sessionEndpoint + "?" + launchTokenParam + "=wrong"},
		{method: http.MethodPost, target: sessionEndpoint, body: `{"token":"wrong"}`},
		{method: http.MethodPost, target: sessionEndpoint, body: `{"token":""}`},
	} {
		rejected := serveGuard(t, handler, tc)
		if rejected.Code != http.StatusForbidden {
			t.Errorf("%s %s: status = %d, want 403 (body %s)",
				tc.method, tc.target, rejected.Code, rejected.Body.String())
			continue
		}
		if kind := guardErrorType(t, rejected); kind != "admin_session_required" {
			t.Errorf("%s %s: error type = %q, want admin_session_required", tc.method, tc.target, kind)
		}
		if servesSessionCookie(rejected) {
			t.Errorf("%s %s: a rejected exchange still set the cookie", tc.method, tc.target)
		}
		for _, leak := range []string{token, "wrong"} {
			if strings.Contains(rejected.Body.String(), leak) {
				t.Errorf("%s %s: body %q echoes %q", tc.method, tc.target, rejected.Body.String(), leak)
			}
		}
	}

	// Only POST is accepted.
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		wrong := serveGuard(t, handler, guardRequest{method: method, target: sessionEndpoint})
		if wrong.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", method, sessionEndpoint, wrong.Code)
		}
		if servesSessionCookie(wrong) {
			t.Errorf("%s %s: a non-POST exchange set the cookie", method, sessionEndpoint)
		}
	}
}

// TestSessionEndpointNeedsNoCookieButEveryOtherRouteDoes pins the boundary: the
// bootstrap route is reachable bare, everything else under /api is not.
func TestSessionEndpointNeedsNoCookieButEveryOtherRouteDoes(t *testing.T) {
	const token = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	handler := New(Options{SessionToken: token})

	allowed := serveGuard(t, handler, guardRequest{
		method: http.MethodPost, target: sessionEndpoint + "?" + launchTokenParam + "=" + token,
	})
	if allowed.Code != http.StatusOK {
		t.Fatalf("bare session exchange = %d, want 200", allowed.Code)
	}

	for _, target := range []string{"/api/status", "/api/settings", "/api/plugins", "/api/keys"} {
		bare := serveGuard(t, handler, guardRequest{method: http.MethodGet, target: target})
		if bare.Code != http.StatusForbidden {
			t.Errorf("cookie-less GET %s = %d, want 403", target, bare.Code)
		}
	}
}

// TestSessionRejectsCrossOriginEvenWithTheToken keeps the CSRF layer in front of
// the exchange: knowing the token is not a licence to hand a browser's cookie to
// a foreign origin.
func TestSessionRejectsCrossOriginEvenWithTheToken(t *testing.T) {
	const token = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	handler := New(Options{SessionToken: token})
	recorder := serveGuard(t, handler, guardRequest{
		method: http.MethodPost,
		target: sessionEndpoint + "?" + launchTokenParam + "=" + token,
		origin: "http://evil.example",
	})
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("cross-origin exchange = %d, want 403 (body %s)", recorder.Code, recorder.Body.String())
	}
	if kind := guardErrorType(t, recorder); kind != "cross_origin" {
		t.Errorf("error type = %q, want cross_origin", kind)
	}
}
