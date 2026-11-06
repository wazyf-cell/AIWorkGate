package main

// Scheduler bookkeeping: balance updates, cooldowns, disable/enable, error
// accounting and the status view.
//
// Ported from pool.go setPoints/cooldown/disable/reenable/reenableIfPoints/
// noteError/noteSuccess/counts/statusList. State lives in the DPAPI-sealed
// accounts.dat (see store.go) instead of the reference's plain state file.

import (
	"time"
)

// setPoints records a freshly read balance.
func (s *store) setPoints(a *account, points int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stateLocked(a, time.Now()).Points = points
	_ = s.flushLocked()
}

// cooldown parks an account for a number of seconds.
func (s *store) cooldown(a *account, kind string, seconds float64, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stateLocked(a, time.Now())
	st.UntilUnix = float64(time.Now().Add(time.Duration(seconds*float64(time.Second))).UnixNano()) / 1e9
	st.CoolKind = kind
	st.Reason = reason
	st.ErrCount = 0
	_ = s.flushLocked()
}

// disable retires an account until re-enabled.
func (s *store) disable(a *account, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stateLocked(a, time.Now())
	st.Disabled = true
	st.Reason = reason
	_ = s.flushLocked()
}

// reenable clears both the disabled flag and any cooldown.
func (s *store) reenable(a *account) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stateLocked(a, time.Now())
	st.Disabled = false
	st.UntilUnix = 0
	st.CoolKind = ""
	st.Reason = ""
	st.ErrCount = 0
	_ = s.flushLocked()
}

// reenableIfPoints refreshes points and, for a live account, clears the cooldown
// (a positive balance means the hard_credit park is over).
func (s *store) reenableIfPoints(a *account, points int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stateLocked(a, time.Now())
	st.Points = points
	if !st.Disabled {
		st.UntilUnix = 0
		st.CoolKind = ""
		st.Reason = ""
		st.ErrCount = 0
	}
	_ = s.flushLocked()
}

// noteError counts a failure and parks the account once the threshold is hit.
func (s *store) noteError(a *account, threshold int, seconds float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stateLocked(a, time.Now())
	st.ErrCount++
	st.lastErr = time.Now()
	if threshold > 0 && st.ErrCount >= threshold {
		st.UntilUnix = float64(time.Now().Add(time.Duration(seconds*float64(time.Second))).UnixNano()) / 1e9
		st.CoolKind = "error_threshold"
		st.Reason = "consecutive errors"
		st.ErrCount = 0
	}
	_ = s.flushLocked()
}

// noteSuccess records a success and clears the error streak.
func (s *store) noteSuccess(a *account) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stateLocked(a, time.Now())
	st.SuccessCount++
	st.lastSuccess = time.Now()
	st.ErrCount = 0
	_ = s.flushLocked()
}

// counts summarises the pool for /health.
type poolCounts struct {
	Total, Healthy, Cooling, Disabled int
}

func (s *store) counts() poolCounts {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	c := poolCounts{Total: len(s.accounts)}
	for _, a := range s.accounts {
		st := s.stateLocked(a, now)
		switch {
		case st.Disabled:
			c.Disabled++
		case st.UntilUnix > 0 && now.Before(st.until()):
			c.Cooling++
		default:
			c.Healthy++
		}
	}
	return c
}

// statusRow is one account's scheduler view.
type statusRow struct {
	ID               string `json:"id"`
	UID              string `json:"uid"`
	Region           string `json:"region"`
	Nickname         string `json:"nickname"`
	Points           int    `json:"points"`
	Cooling          bool   `json:"cooling"`
	CoolKind         string `json:"cool_kind"`
	CoolRemainingSec int64  `json:"cool_remaining_sec"`
	Reason           string `json:"reason"`
	Disabled         bool   `json:"disabled"`
	SuccessCount     int    `json:"success_count"`
	ErrCount         int    `json:"err_count"`
}

// statusList returns every account, sorted for a stable page.
func (s *store) statusList() []statusRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	rows := make([]statusRow, 0, len(s.accounts))
	for _, a := range s.accounts {
		snap := a.snapshot()
		st := s.stateLocked(a, now)
		cooling := st.UntilUnix > 0 && now.Before(st.until()) && !st.Disabled
		row := statusRow{
			ID:           stateKey(snap.Region, snap.UserID),
			UID:          snap.UserID,
			Region:       snap.Region,
			Nickname:     snap.UserName,
			Points:       st.Points,
			Cooling:      cooling,
			Reason:       st.Reason,
			Disabled:     st.Disabled,
			SuccessCount: st.SuccessCount,
			ErrCount:     st.ErrCount,
		}
		if cooling {
			row.CoolKind = st.CoolKind
			row.CoolRemainingSec = int64(st.until().Sub(now).Seconds())
			if row.CoolRemainingSec < 0 {
				row.CoolRemainingSec = 0
			}
		}
		rows = append(rows, row)
	}
	return rows
}
