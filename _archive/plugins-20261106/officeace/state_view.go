package main

import (
	"time"
)

// Quota is the inline quota block of GET /accounts (§6.3).
// Plan is the v0.3 optional subscription name from the local API.
//
// Remaining/Total are omitted when the plan is unlimited: the host contract has
// no negative sentinel, and reporting -1/-1 made the host store a negative
// snapshot and treat the account as nearly depleted. Unlimited is carried by
// the explicit Unlimited flag instead (the host must honour it; see README).
type Quota struct {
	Remaining      float64 `json:"remaining,omitempty"`
	Total          float64 `json:"total,omitempty"`
	ResetAt        int64   `json:"reset_at,omitempty"`
	ExpiresAt      int64   `json:"expires_at,omitempty"`
	Plan           string  `json:"plan,omitempty"`
	RateLimited    bool    `json:"rate_limited"`
	TodayCheckedIn bool    `json:"today_checked_in"`
	CheckedInAt    int64   `json:"checked_in_at,omitempty"`
	Unlimited      bool    `json:"unlimited,omitempty"`
}

// Account is the persisted, credential-free bookkeeping for the OfficeAce user.
// MaaS keys stay in OfficeAce's own SQLite; this file only keeps ids and
// check-in/quota observations.
type Account struct {
	ID            string `json:"id"`
	Label         string `json:"label"`
	UserID        string `json:"user_id"`
	AccountID     string `json:"account_id,omitempty"`
	Enabled       bool   `json:"enabled"`
	Status        string `json:"status"`
	AddedAt       int64  `json:"added_at"`
	LastSeenAt    int64  `json:"last_seen_at,omitempty"`
	ExpiresAt     int64  `json:"expires_at,omitempty"`
	CheckedInAt   int64  `json:"checked_in_at,omitempty"`
	CheckinResult string `json:"checkin_result,omitempty"`
	LastError     string `json:"last_error,omitempty"`
}

// accountView is the wire shape of GET /accounts.
type accountView struct {
	ID         string          `json:"id"`
	Label      string          `json:"label"`
	Enabled    bool            `json:"enabled"`
	Status     string          `json:"status"`
	AddedAt    int64           `json:"added_at"`
	Quota      *Quota          `json:"quota,omitempty"`
	Credential *credentialView `json:"credential,omitempty"`
	LastError  string          `json:"last_error,omitempty"`
}

// credentialView is the contract v0.3 credential summary: presence and expiry
// facts only, never the credential itself (OfficeAce keeps its own secrets).
type credentialView struct {
	Present     bool  `json:"present"`
	ExpiresAt   int64 `json:"expires_at,omitempty"`
	Refreshable bool  `json:"refreshable"`
}

func (a *Account) view(now time.Time, quota *Quota) accountView {
	v := accountView{
		ID:        a.ID,
		Label:     a.Label,
		Enabled:   a.Enabled,
		Status:    a.Status,
		AddedAt:   a.AddedAt,
		LastError: a.LastError,
	}
	if v.Status == "" {
		v.Status = statusUnknown
	}
	if quota != nil {
		q := *quota
		if q.TodayCheckedIn && !sameDay(q.CheckedInAt, now) {
			q.TodayCheckedIn = false
			q.CheckedInAt = 0
		}
		v.Quota = &q
	}
	return v
}

// todayCheckedIn reports whether this plugin recorded a successful claim today.
// It never guesses: an empty/old record is false.
func (a *Account) todayCheckedIn(now time.Time) (bool, int64) {
	if a.CheckedInAt <= 0 || !sameDay(a.CheckedInAt, now) {
		return false, 0
	}
	switch a.CheckinResult {
	case "ok", "already":
		return true, a.CheckedInAt
	default:
		return false, 0
	}
}

func sameDay(ts int64, now time.Time) bool {
	if ts <= 0 {
		return false
	}
	return time.Unix(ts, 0).In(now.Location()).Format("2006-01-02") == now.Format("2006-01-02")
}
