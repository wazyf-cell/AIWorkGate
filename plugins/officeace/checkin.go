package main

import (
	"errors"
	"net/http"
	"strings"
)

// Failure attribution for POST /accounts/{id}/checkin (contract v0.4, plan
// 2026-10-21 §4, mirroring A's tasks/trae_checkin.rs classify_error).
//
// Only unambiguous causes are reported. In particular a local service that is
// not running (localHTTPError.Status == 0) is NOT a dead session: the stored
// session may still be valid and the user only has to start OfficeAce again.
// Everything this plugin cannot prove stays unattributed (empty kind) and the
// host only counts the failure.

// Check-in failure kinds this plugin can prove.
const (
	kindSessionDead = "session_dead"
	kindRateLimited = "rate_limited"
	kindQuota       = "quota"
)

// quotaHints are upstream wordings that mean "balance/quota exhausted" rather
// than a transient error.
var quotaHints = []string{
	"余额不足", "额度不足", "积分不足", "配额不足",
	"insufficient quota", "insufficient balance", "insufficient credit",
	"quota exceeded", "quota exhausted", "credits exhausted",
}

// classifyLocalFailure maps an OfficeAce local API error to (kind, resetAt).
// resetAt is only set when the service sent a Retry-After header.
func (p *Plugin) classifyLocalFailure(err error) (string, int64) {
	if err == nil {
		return "", 0
	}
	var le *localHTTPError
	if errors.As(err, &le) {
		if le.Status == 0 {
			// "OfficeAce 未运行": not a session problem.
			return "", 0
		}
		switch le.Status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return kindSessionDead, 0
		case http.StatusTooManyRequests:
			return kindRateLimited, retryAfterReset(p.now().Unix(), le.RetryAfter)
		case http.StatusPaymentRequired:
			return kindQuota, 0
		}
		return kindFromMessage(le.Message), 0
	}
	return kindFromMessage(err.Error()), 0
}

// kindFromMessage derives a kind from the upstream text alone. Anything else is
// left unattributed instead of guessed.
func kindFromMessage(message string) string {
	lower := strings.ToLower(message)
	for _, hint := range quotaHints {
		if strings.Contains(lower, hint) {
			return kindQuota
		}
	}
	return ""
}

// retryAfterReset turns a Retry-After delay (seconds) into an absolute Unix
// reset time. Non-positive or absent values mean "no reset time given".
func retryAfterReset(now, seconds int64) int64 {
	if seconds <= 0 {
		return 0
	}
	return now + seconds
}
