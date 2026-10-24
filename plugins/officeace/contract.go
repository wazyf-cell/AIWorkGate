package main

import (
	"net/http"
	"strings"
)

// Contract v0.3 headers: the host pins the account in the request, the plugin
// reports the account that actually served it.
const (
	headerAccount       = "X-Aiworkgate-Account"
	headerRequestID     = "X-Aiworkgate-Request-Id"
	headerHedge         = "X-Aiworkgate-Hedge"
	headerCredits       = "X-Aiworkgate-Credits"
	headerAccountStatus = "X-Aiworkgate-Account-Status"
	headerCooldown      = "X-Aiworkgate-Cooldown"
)

// forcedAccountID returns the host-selected account id, or "" when the plugin
// is allowed to pick the account itself (v1 behavior).
func forcedAccountID(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get(headerAccount))
}

// isHedge reports whether the host sent this request as a hedged attempt: it
// must not trigger retries, credential refresh or any other extra upstream call.
func isHedge(r *http.Request) bool {
	return strings.TrimSpace(r.Header.Get(headerHedge)) == "1"
}

func requestID(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get(headerRequestID))
}

// setAccountHeader must run before the first response byte, including SSE.
func setAccountHeader(w http.ResponseWriter, accountID string) {
	if accountID == "" {
		return
	}
	w.Header().Set(headerAccount, accountID)
}

// setRateLimitHeaders marks a 429 so the host can cool the account down.
func setRateLimitHeaders(w http.ResponseWriter, accountID string) {
	setAccountHeader(w, accountID)
	w.Header().Set(headerAccountStatus, "rate_limited")
}

// writeAccountUnavailable is the contract's 409 for a pinned account that this
// plugin cannot serve. OfficeAce has a single account, so any mismatch ends
// here instead of falling back.
func writeAccountUnavailable(w http.ResponseWriter, accountID, msg string) {
	if strings.TrimSpace(msg) == "" {
		msg = "account " + accountID + " is not available"
	}
	writeError(w, http.StatusConflict, "account_unavailable", msg)
}
