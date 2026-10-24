package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aiworkgate/internal/model"
	"aiworkgate/internal/scheduler"
)

// healthzTestApp wires a scheduler plus a fake snapshot provider.
func healthzTestApp(snap *scheduler.Snapshot, err error) *App {
	return &App{
		dispatch: scheduler.New(scheduler.Options{}),
		healthSnapshot: func(context.Context) (*scheduler.Snapshot, error) {
			return snap, err
		},
	}
}

func healthzSnapshot(accounts ...scheduler.Account) *scheduler.Snapshot {
	return &scheduler.Snapshot{
		Pools: map[string]*scheduler.Pool{
			"mock": {
				PluginID: "mock", UpstreamEnabled: true, Accounts: accounts,
			},
		},
		Bindings: map[string]scheduler.Binding{},
	}
}

func healthzAccount(id string, enabled bool) scheduler.Account {
	return scheduler.Account{ID: id, Label: id, Enabled: enabled, CredentialPresent: true}
}

type healthzBody struct {
	Status       string `json:"status"`
	Selectable   int    `json:"selectable"`
	Total        int    `json:"total"`
	EgressFailed int    `json:"egress_failed"`
	Reason       string `json:"reason"`
}

func decodeHealthz(t *testing.T, rec *httptest.ResponseRecorder) healthzBody {
	t.Helper()
	var body healthzBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return body
}

// TestHealthzOKWithSelectableAccount mirrors A's /healthz readiness body:
// 200 plus counts when at least one account can be selected.
func TestHealthzOKWithSelectableAccount(t *testing.T) {
	app := healthzTestApp(healthzSnapshot(
		healthzAccount("secret-account-id", true),
		healthzAccount("disabled-one", false),
	), nil)
	rec := httptest.NewRecorder()
	app.routeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	body := decodeHealthz(t, rec)
	if body.Status != "ok" || body.Selectable != 1 || body.Total != 2 || body.Reason != "" {
		t.Fatalf("body = %+v", body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	// Readiness must never expose account ids (or anything account-shaped).
	if strings.Contains(rec.Body.String(), "secret-account-id") {
		t.Fatalf("body leaks an account id: %s", rec.Body.String())
	}
}

// TestHealthzUnavailableWithoutSelectableAccount matches A: zero selectable
// accounts is a 503 "no_account" so watchdogs can react.
func TestHealthzUnavailableWithoutSelectableAccount(t *testing.T) {
	depleted := healthzAccount("a", true)
	depleted.Remaining, depleted.Total = 0, 100
	cooling := healthzAccount("b", true)
	cooling.CooldownUntil = 1 << 40 // far future: account-level cooldown
	app := healthzTestApp(healthzSnapshot(
		healthzAccount("off", false),
		depleted,
		cooling,
	), nil)

	rec := httptest.NewRecorder()
	app.routeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%s)", rec.Code, rec.Body.String())
	}
	body := decodeHealthz(t, rec)
	if body.Status != "unavailable" || body.Reason != "no_account" {
		t.Fatalf("body = %+v", body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
}

// TestHealthzExpiredPluginAccountIsNotSelectable reproduces the measured defect:
// minimax reports status "expired" together with an empty quota block ({}), so
// the credit filter fails open on 0/0 and /healthz counted it selectable. The
// account reaches the scheduler through the production mapping (schedulerAccount),
// which is exactly where the plugin status used to be dropped.
func TestHealthzExpiredPluginAccountIsNotSelectable(t *testing.T) {
	expired := schedulerAccount(model.Account{
		ID: "minimax", Label: "minimax", Enabled: true, Status: "expired",
	}, model.AccountMeta{}, false)
	healthy := schedulerAccount(model.Account{
		ID: "zcode", Label: "zcode", Enabled: true, Status: "ok",
	}, model.AccountMeta{}, false)
	app := healthzTestApp(healthzSnapshot(expired, healthy), nil)

	rec := httptest.NewRecorder()
	app.routeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	body := decodeHealthz(t, rec)
	if body.Selectable != 1 || body.Total != 2 {
		t.Fatalf("selectable=%d total=%d (%s), want 1/2: an expired plugin account cannot serve",
			body.Selectable, body.Total, rec.Body.String())
	}

	// Watchdogs parse this body: status/selectable/total stay, and the new
	// egress_failed is additive so producers of the old three keep working.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &fields); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	if len(fields) != 4 || fields["status"] == nil || fields["selectable"] == nil ||
		fields["total"] == nil || fields["egress_failed"] == nil {
		t.Fatalf("body shape changed: %v", fields)
	}
}

// TestHealthzStartupGrace documents the chosen behavior: without scheduler
// state (or when the snapshot cannot be read) the probe answers 200 with
// reason "scheduler_unavailable" instead of a false 503.
func TestHealthzStartupGrace(t *testing.T) {
	cases := []struct {
		name string
		app  *App
	}{
		{"no scheduler", &App{}},
		{"snapshot error", healthzTestApp(nil, errors.New("store unavailable"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.app.routeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
			}
			body := decodeHealthz(t, rec)
			if body.Status != "ok" || body.Reason != "scheduler_unavailable" {
				t.Fatalf("body = %+v", body)
			}
		})
	}
}

// TestHealthzEgressFailedIsDegradedNot503: a dead proxy is a degraded exit, not
// a "no account" outage; 503 would make a watchdog restart the host, which
// cannot fix the user's proxy.
func TestHealthzEgressFailedIsDegradedNot503(t *testing.T) {
	dead := healthzAccount("a", true)
	dead.Egress = scheduler.EgressEvidence{Failed: true, Label: "公司代理", Error: "timeout"}
	app := healthzTestApp(healthzSnapshot(dead), nil)

	rec := httptest.NewRecorder()
	app.routeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	body := decodeHealthz(t, rec)
	if body.Status != "degraded" || body.Reason != "egress_failed" ||
		body.Selectable != 0 || body.Total != 1 || body.EgressFailed != 1 {
		t.Fatalf("body = %+v", body)
	}
}

// TestHealthzCountsEgressFailuresNextToHealthyAccounts: when something still
// serves, the body stays "ok" and exposes the degraded exit count.
func TestHealthzCountsEgressFailuresNextToHealthyAccounts(t *testing.T) {
	dead := healthzAccount("dead", true)
	dead.Egress = scheduler.EgressEvidence{Failed: true, Label: "px"}
	app := healthzTestApp(healthzSnapshot(dead, healthzAccount("alive", true)), nil)

	rec := httptest.NewRecorder()
	app.routeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	body := decodeHealthz(t, rec)
	if rec.Code != http.StatusOK || body.Status != "ok" ||
		body.Selectable != 1 || body.Total != 2 || body.EgressFailed != 1 {
		t.Fatalf("status = %d body = %+v", rec.Code, body)
	}
}

func TestHealthzRejectsWritesAndSupportsHead(t *testing.T) {
	app := healthzTestApp(healthzSnapshot(healthzAccount("a", true)), nil)

	rec := httptest.NewRecorder()
	app.routeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", rec.Code)
	}

	rec = httptest.NewRecorder()
	app.routeHTTP(rec, httptest.NewRequest(http.MethodHead, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD body = %q, want empty", rec.Body.String())
	}
}
