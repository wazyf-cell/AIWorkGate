package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

const (
	accountsFileName    = "accounts.dat"
	stateFileName       = "state.json"
	modelsCacheFileName = "models-cache.json"
)

// Quota mirrors the inline quota block of the plugin contract (section 6.3).
// Plan is the v0.3 optional package name parsed from the statistics endpoint.
type Quota struct {
	Remaining      float64 `json:"remaining"`
	Total          float64 `json:"total"`
	ResetAt        int64   `json:"reset_at,omitempty"`
	ExpiresAt      int64   `json:"expires_at,omitempty"`
	Plan           string  `json:"plan,omitempty"`
	RateLimited    bool    `json:"rate_limited"`
	TodayCheckedIn bool    `json:"today_checked_in"`
	CheckedInAt    int64   `json:"checked_in_at,omitempty"`
}

// Credential is the CodeArts STS credential shape from the reference
// implementation (oauth.ts credentialFromTokenResponse). expires_at stays a
// string because the STS returns an RFC3339 timestamp.
type Credential struct {
	AccessKeyID     string          `json:"access_key_id"`
	SecretAccessKey string          `json:"secret_access_key"`
	SecurityToken   string          `json:"security_token"`
	ExpiresAt       string          `json:"expires_at"`
	RefreshToken    string          `json:"refresh_token,omitempty"`
	CodeVerifier    string          `json:"code_verifier,omitempty"`
	DPoPPrivateJWK  json.RawMessage `json:"dpop_private_key_jwk,omitempty"`
	DomainID        string          `json:"domain_id,omitempty"`
	UserID          string          `json:"user_id,omitempty"`
	UserName        string          `json:"user_name,omitempty"`
}

func (c Credential) usable() bool {
	return c.AccessKeyID != "" && c.SecretAccessKey != "" && c.SecurityToken != ""
}

func (c Credential) refreshable() bool {
	return c.RefreshToken != "" && c.CodeVerifier != "" && len(c.DPoPPrivateJWK) > 0
}

// expiry returns the parsed expiration, or the zero time when unknown.
func (c Credential) expiry() time.Time {
	s := strings.TrimSpace(c.ExpiresAt)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z0700"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n > 1e12 {
			return time.UnixMilli(n)
		}
		return time.Unix(n, 0)
	}
	return time.Time{}
}

// identity is the plugin-owned, request-level identity. CodeArts has no
// machine/device header, so only the session id is reported (never invent a
// machine id that the upstream would ignore).
type accountIdentity struct {
	SessionID string `json:"session_id"`
	ChatID    string `json:"chat_id,omitempty"`
	Source    string `json:"source"`
	UpdatedAt int64  `json:"updated_at"`
}

type checkinState struct {
	At            int64    `json:"at,omitempty"`
	Result        string   `json:"result,omitempty"`
	CreditsBefore *float64 `json:"credits_before,omitempty"`
	CreditsAfter  *float64 `json:"credits_after,omitempty"`
	Message       string   `json:"message,omitempty"`
}

type Account struct {
	ID         string          `json:"id"`
	Label      string          `json:"label"`
	Enabled    bool            `json:"enabled"`
	AddedAt    int64           `json:"added_at"`
	Credential Credential      `json:"credential"`
	Identity   accountIdentity `json:"identity"`
	Quota      Quota           `json:"quota"`
	QuotaAt    int64           `json:"quota_at,omitempty"`
	Checkin    checkinState    `json:"checkin,omitempty"`
	Invalid    bool            `json:"invalid,omitempty"`
	LastError  string          `json:"last_error,omitempty"`
}

// status maps the persisted account to the contract enum ok/expired/invalid.
func (a *Account) status(now time.Time) string {
	if a.Invalid {
		return "invalid"
	}
	exp := a.Credential.expiry()
	if !exp.IsZero() && !now.Before(exp) && !a.Credential.refreshable() {
		return "expired"
	}
	return "ok"
}

type accountView struct {
	ID         string          `json:"id"`
	Label      string          `json:"label"`
	Enabled    bool            `json:"enabled"`
	Status     string          `json:"status"`
	AddedAt    int64           `json:"added_at"`
	Quota      Quota           `json:"quota"`
	Credential *credentialView `json:"credential,omitempty"`
	// CooldownUntil is only reported while the account is rate limited; there
	// is no model-level cooldown tracking in this plugin.
	CooldownUntil int64     `json:"cooldown_until,omitempty"`
	Identity      *Identity `json:"identity,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
}

// credentialView is the contract v0.3 credential summary. It never carries the
// credential itself, only presence/expiry facts.
type credentialView struct {
	Present     bool  `json:"present"`
	ExpiresAt   int64 `json:"expires_at,omitempty"`
	Refreshable bool  `json:"refreshable"`
}

// Identity is the wire shape returned by GET /accounts.
type Identity struct {
	SessionID string `json:"session_id"`
	Source    string `json:"source"`
	UpdatedAt int64  `json:"updated_at"`
}

func (a *Account) view(now time.Time) accountView {
	q := a.Quota
	if q.TodayCheckedIn && !sameDay(q.CheckedInAt, now) {
		q.TodayCheckedIn = false
		q.CheckedInAt = 0
	}
	cred := &credentialView{
		Present:     a.Credential.usable() || a.Credential.refreshable(),
		Refreshable: a.Credential.refreshable(),
	}
	if exp := a.Credential.expiry(); !exp.IsZero() {
		cred.ExpiresAt = exp.Unix()
	}
	var cooldown int64
	if q.RateLimited {
		cooldown = q.ResetAt
	}
	return accountView{
		ID:            a.ID,
		Label:         a.Label,
		Enabled:       a.Enabled,
		Status:        a.status(now),
		AddedAt:       a.AddedAt,
		Quota:         q,
		Credential:    cred,
		CooldownUntil: cooldown,
		Identity:      &Identity{SessionID: a.Identity.SessionID, Source: "derived", UpdatedAt: a.Identity.UpdatedAt},
		LastError:     a.LastError,
	}
}

func sameDay(ts int64, now time.Time) bool {
	if ts <= 0 {
		return false
	}
	return time.Unix(ts, 0).In(now.Location()).Format("2006-01-02") == now.Format("2006-01-02")
}

// State is the non-secret state stored in state.json.
type State struct {
	Version    int   `json:"version"`
	Seq        int   `json:"seq"`
	RuleHits   int64 `json:"rule_hits"`
	StripHits  int64 `json:"strip_hits"`
	LastReload int64 `json:"last_reload_at"`
	RulesCount int   `json:"last_reload_rules"`
	StripCount int   `json:"last_reload_strip"`
	EgressAccs int   `json:"last_reload_egress_accounts"`
}

type accountsFile struct {
	Version  int        `json:"version"`
	Accounts []*Account `json:"accounts"`
}
