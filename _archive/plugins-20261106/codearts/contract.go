package main

import (
	"net/http"
	"strconv"
	"strings"
)

// Contract v0.3 headers. The request side carries the host's account choice,
// the response side reports the account that actually served the request so
// the host can keep its own ledger (design §3.1/§3.2).
const (
	headerAccount       = "X-Aiworkgate-Account"
	headerRequestID     = "X-Aiworkgate-Request-Id"
	headerHedge         = "X-Aiworkgate-Hedge"
	headerCredits       = "X-Aiworkgate-Credits"
	headerAccountStatus = "X-Aiworkgate-Account-Status"
	headerCooldown      = "X-Aiworkgate-Cooldown"
)

// forcedAccountID returns the host-selected account id, or "" when the host
// left the choice to the plugin (v1 behavior).
func forcedAccountID(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get(headerAccount))
}

// isHedge reports whether this is a hedged request: the plugin must not retry,
// refresh and replay, rotate or otherwise amplify cost.
func isHedge(r *http.Request) bool {
	return strings.TrimSpace(r.Header.Get(headerHedge)) == "1"
}

func requestID(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get(headerRequestID))
}

// setAccountHeader reports the account that is actually serving the response.
// It must run before the first response byte, including SSE chunks.
func setAccountHeader(w http.ResponseWriter, accountID string) {
	if accountID == "" {
		return
	}
	w.Header().Set(headerAccount, accountID)
}

// setRateLimitHeaders annotates a 429/quota failure so the host can cool the
// account down without parsing the body.
func (p *Plugin) setRateLimitHeaders(w http.ResponseWriter, accountID string) {
	setAccountHeader(w, accountID)
	w.Header().Set(headerAccountStatus, "rate_limited")
	now := p.now()
	secs := nextUTC8DayStart(now) - now.Unix()
	if secs > 0 {
		w.Header().Set(headerCooldown, strconv.FormatInt(secs, 10))
	}
}

// writeAccountUnavailable returns the contract's 409 for a forced account that
// cannot serve the request. The plugin must never silently switch accounts.
func writeAccountUnavailable(w http.ResponseWriter, accountID, msg string) {
	if strings.TrimSpace(msg) == "" {
		msg = "account " + accountID + " is not available"
	}
	writeError(w, http.StatusConflict, "account_unavailable", msg)
}
