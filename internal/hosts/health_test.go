package hosts

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func statusServer(code int, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if code != http.StatusOK {
			w.WriteHeader(code)
			return
		}
		io.WriteString(w, body)
	}))
}

func TestHealthStateThreeStates(t *testing.T) {
	cases := []struct {
		name string
		code int
		body string
		want AState
	}{
		{"ok", http.StatusOK, `{"status":"ok"}`, AStateOK},
		{"401 is unauthorized", http.StatusUnauthorized, `{"error":"unauthorized"}`, AStateUnauthorized},
		{"403 is unauthorized", http.StatusForbidden, `{"error":"forbidden"}`, AStateUnauthorized},
		{"500 is down", http.StatusInternalServerError, "", AStateDown},
	}
	for _, tc := range cases {
		srv := statusServer(tc.code, tc.body)
		state, detail := NewClient(srv.URL, "k").HealthState(context.Background())
		srv.Close()
		if state != tc.want {
			t.Fatalf("%s: state = %q, want %q (detail %q)", tc.name, state, tc.want, detail)
		}
		if tc.want == AStateOK && detail != "" {
			t.Fatalf("%s: ok must carry no detail, got %q", tc.name, detail)
		}
		if tc.want != AStateOK && detail == "" {
			t.Fatalf("%s: expected a detail for %q", tc.name, state)
		}
	}
}

func TestHealthStateDownWhenRefused(t *testing.T) {
	srv := statusServer(http.StatusOK, `{"status":"ok"}`)
	url := srv.URL
	srv.Close() // nothing listens on the port anymore

	state, detail := NewClient(url, "").HealthState(context.Background())
	if state != AStateDown {
		t.Fatalf("state = %q, want down", state)
	}
	if detail == "" {
		t.Fatalf("expected a transport detail, got %q", detail)
	}
}

func TestHealthStateUnhealthy200IsDown(t *testing.T) {
	srv := statusServer(http.StatusOK, `{"status":"degraded"}`)
	defer srv.Close()
	state, _ := NewClient(srv.URL, "").HealthState(context.Background())
	if state != AStateDown {
		t.Fatalf("state = %q, want down for a non-ok health payload", state)
	}
}

func TestHealthStateEmptyBaseURLIsDown(t *testing.T) {
	state, detail := NewClient("", "").HealthState(context.Background())
	if state != AStateDown || detail == "" {
		t.Fatalf("state/detail = %q/%q, want down with a detail", state, detail)
	}
}

func TestGetReturnsTypedHTTPError(t *testing.T) {
	srv := statusServer(http.StatusUnauthorized, `{"error":"bad key"}`)
	defer srv.Close()

	err := NewClient(srv.URL, "bad").Health(context.Background())
	if err == nil {
		t.Fatal("expected an error for HTTP 401")
	}
	var he *HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("error %v is not a *HTTPError", err)
	}
	if he.StatusCode != http.StatusUnauthorized || he.Path != "/health" {
		t.Fatalf("HTTPError = %+v", he)
	}
	// The message format is unchanged for existing string matchers.
	if !strings.HasPrefix(err.Error(), "hosts: GET /health: HTTP 401") {
		t.Fatalf("message = %q", err.Error())
	}
}
