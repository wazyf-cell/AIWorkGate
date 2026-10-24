package main

// Chat entry point: validate, pick an account, rotate on failure, and hand the
// request to the channel ladder in engine.go.
//
// Ported from chat.go Engine.chat / Engine.pickFor. The reference resolves the
// region and then always rotates over cn; this port honours the region hint,
// which is a strict superset of the reference's behaviour for cn callers.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// handleChat is the entry point: validate, pick, prepare, dispatch with rotation.
func (e *engine) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "read body failed")
		return
	}
	if verr := validateChatBody(body); verr != "" {
		writeError(w, http.StatusBadRequest, "invalid_request", verr)
		return
	}
	var obj map[string]any
	_ = json.Unmarshal(body, &obj)
	rawModel := strings.TrimSpace(toString(obj["model"]))
	// The reference resolves the region but then always rotates over cn; this
	// port uses the hint only to filter, which is the honest subset.
	region := resolveRegion(r.Header, obj, rawModel)
	model := stripRegionPrefix(rawModel)
	var want struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &want)

	tried := map[string]bool{}
	var lastErr error
	for i := 0; i < e.cfg.MaxRotate; i++ {
		a := e.pickFor(region, tried)
		if a == nil {
			break
		}
		tried[a.snapshot().UserID] = true
		if perr := e.prepare(a); perr != nil {
			lastErr = perr
			if e.handleErr(a, perr) {
				// refresh rescued the session: retry this same account
				delete(tried, a.snapshot().UserID)
				i--
			}
			continue
		}
		served, _ := e.dispatch(a, w, body, model, want.Stream)
		if served {
			return
		}
	}
	msg := "all accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
}

// pickFor picks an account, preferring the requested region when one is known.
func (e *engine) pickFor(region string, tried map[string]bool) *account {
	if a := e.st.pick(tried); a != nil {
		if region == "" || a.snapshot().Region == region {
			return a
		}
	}
	// Fall back to any healthy account of the wanted region.
	for _, c := range e.st.candidates(tried) {
		if region == "" || c.snapshot().Region == region {
			return c
		}
	}
	return nil
}

// refreshOne refreshes a token, falling back to a balance read so the account
// page still shows a fresh number when the refresh token is gone.
func (e *engine) refreshOne(a *account) {
	if err := e.up.refresh(a); err == nil {
		_ = e.st.syncFlush()
		return
	}
	if pts, err := e.up.checkPoints(a); err == nil {
		e.st.setPoints(a, pts)
	}
}

func shortUID(a *account) string {
	uid := a.snapshot().UserID
	return uid[:minLen(uid, 8)]
}
