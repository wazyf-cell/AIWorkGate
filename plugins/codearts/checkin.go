package main

import (
	"errors"
	"strings"
	"time"
)

type checkinResult struct {
	Result        string   `json:"result"`
	CreditsBefore *float64 `json:"credits_before,omitempty"`
	CreditsAfter  *float64 `json:"credits_after,omitempty"`
	Message       string   `json:"message,omitempty"`
	At            int64    `json:"at"`
	// Optional failure attribution (contract v0.4, plan 2026-10-21 §4,
	// mirroring A's tasks/trae_checkin.rs classify_error). Only unambiguous
	// causes are reported; the host otherwise counts the failure.
	Kind    string `json:"kind,omitempty"`
	ResetAt int64  `json:"reset_at,omitempty"`
}

// Check-in failure kinds this plugin can prove.
const (
	kindSessionDead = "session_dead"
	kindRateLimited = "rate_limited"
	kindQuota       = "quota"
)

// checkinFailure carries the optional attribution through the claim flow. The
// message keeps the user-facing text the handler already produced.
type checkinFailure struct {
	Kind    string
	ResetAt int64
	Message string
}

func (e *checkinFailure) Error() string { return e.Message }

// checkinErrorAttribution extracts (kind, reset_at) from a claim-flow error.
func checkinErrorAttribution(err error) (string, int64) {
	var cf *checkinFailure
	if errors.As(err, &cf) {
		return cf.Kind, cf.ResetAt
	}
	return kindFromMessage(err.Error()), 0
}

// credentialCheckinFailure classifies a credential error that happened before
// the claim flow. An expired refresh token, or the explicit "请重新登录"
// message the credential layer emits, is a dead session; anything else (a
// network error while refreshing, for instance) stays unattributed.
func credentialCheckinFailure(err error, now time.Time) checkinResult {
	kind := ""
	switch {
	case errors.Is(err, errRefreshTokenExpired):
		kind = kindSessionDead
	case strings.Contains(err.Error(), "请重新登录"):
		kind = kindSessionDead
	default:
		kind = kindFromMessage(err.Error())
	}
	return checkinResult{Result: "fail", Message: err.Error(), At: now.Unix(), Kind: kind}
}

type refreshResult struct {
	Result    string `json:"result"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
	Message   string `json:"message,omitempty"`
	At        int64  `json:"at"`
}

type packageInfo struct {
	IsCreditPackage bool
	IsTokenPackage  bool
	PackageName     string
	Remaining       float64
	Total           float64
	HasCredit       bool
}

type opsActivity struct {
	CampaignID any
	Type       string
	Title      string
	Claimable  bool
	Status     string
	Amount     float64
}

type snapResult struct {
	ok      bool
	data    map[string]any
	message string
	// status is the HTTP status of the failed response (0 when the request
	// never reached a decision); resetAt is an absolute Unix reset time derived
	// from Retry-After when the upstream sent one.
	status  int
	resetAt int64
}

var creditMetricLabels = map[string]bool{
	"usageTotalPackageCredit":    true,
	"usageBasicPackageCredit":    true,
	"usageOnDemandPackageCredit": true,
	"usageBonusPackageCredit":    true,
}

const totalCreditMetric = "usageTotalPackageCredit"
