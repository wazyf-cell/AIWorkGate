package app

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// /healthz is the readiness probe of A (TraeWorkAssistant, MIT, 朱天伟;
// api_server/routes.rs:489-524, registered at api_server/server.rs:176): the
// host answers 503 when no account can be selected. AIWorkGate keeps the same
// shape but reports counts only — never account ids, labels or credentials.
//
// Startup grace: when no scheduler/snapshot is wired yet (or the snapshot
// cannot be read right now) the endpoint answers 200 with
// {"status":"ok","reason":"scheduler_unavailable"}. That is deliberate and
// consistent: a host that is still coming up must not look "no account" to a
// launcher/watchdog, because the usual reaction to 503 is a restart.
const healthzSnapshotTimeout = 3 * time.Second

// healthzOK is the ready body: at least one selectable account exists.
// EgressFailed counts accounts that would be selectable but for a proxy that
// failed inside the evidence window; a non-zero value with Selectable > 0 means
// "serving, with a degraded exit".
type healthzOK struct {
	Status       string `json:"status"`
	Selectable   int    `json:"selectable"`
	Total        int    `json:"total"`
	EgressFailed int    `json:"egress_failed"`
}

// healthzReason is the 503 body and the startup-grace body.
type healthzReason struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// healthzDegraded reports "every usable account's exit is down". It stays 200
// on purpose: routing still attempts those accounts (the evidence can be stale
// and the proxy may recover), and a launcher/watchdog reacts to 503 by
// restarting — which cannot fix a user's proxy.
type healthzDegraded struct {
	Status       string `json:"status"`
	Reason       string `json:"reason"`
	Selectable   int    `json:"selectable"`
	Total        int    `json:"total"`
	EgressFailed int    `json:"egress_failed"`
}

// handleHealthz serves GET /healthz. It is routed before the admin auth gate.
func (a *App) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	code, payload := a.healthzPayload(r.Context())
	w.WriteHeader(code)
	if r.Method != http.MethodGet {
		return
	}
	_ = json.NewEncoder(w).Encode(payload)
}

// healthzPayload maps the selectable count to A's status codes: 200 when one
// account is selectable, 503 "no_account" when none is. A dead egress proxy is
// reported as 200 "degraded" instead of 503 (see healthzDegraded).
func (a *App) healthzPayload(ctx context.Context) (int, any) {
	selectable, total, egressFailed, ok := a.healthzCounts(ctx)
	if !ok {
		return http.StatusOK, healthzReason{Status: "ok", Reason: "scheduler_unavailable"}
	}
	if selectable > 0 {
		return http.StatusOK, healthzOK{
			Status: "ok", Selectable: selectable, Total: total, EgressFailed: egressFailed,
		}
	}
	if egressFailed > 0 {
		return http.StatusOK, healthzDegraded{
			Status: "degraded", Reason: "egress_failed",
			Selectable: 0, Total: total, EgressFailed: egressFailed,
		}
	}
	return http.StatusServiceUnavailable, healthzReason{Status: "unavailable", Reason: "no_account"}
}

// healthzCounts computes the selectable count from the scheduling snapshot.
// ok is false when there is no scheduler state to judge (startup grace).
func (a *App) healthzCounts(ctx context.Context) (selectable, total, egressFailed int, ok bool) {
	if a == nil || a.dispatch == nil {
		return 0, 0, 0, false
	}
	snapshot := a.healthSnapshot
	if snapshot == nil {
		snapshot = a.schedulerSnapshot
	}
	if ctx == nil {
		ctx = context.Background()
	}
	callCtx, cancel := context.WithTimeout(ctx, healthzSnapshotTimeout)
	defer cancel()
	snap, err := snapshot(callCtx)
	if err != nil || snap == nil {
		return 0, 0, 0, false
	}
	selectable, total, egressFailed = a.dispatch.HealthCounts(snap)
	return selectable, total, egressFailed, true
}
