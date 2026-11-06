package main

// Account scheduler: points-weighted picking, cooldowns, consecutive-error
// accounting. Ported from pool.go, with one deliberate change of storage.
//
// The reference keeps this state in state.<region>.json as plain JSON because it
// runs server-side. This port folds it into the same DPAPI-sealed accounts.dat:
// a cooldown timer is not a credential, but the file that contains it sits next
// to one, and splitting them would leave an operator-identifying artifact in the
// clear (uid, phone nickname, points).

import (
	"math/rand"
	"sort"
	"strings"
	"time"
)

// accountState is the persisted scheduler row for one account.
type accountState struct {
	Region       string  `json:"region"`
	Points       int     `json:"points"`
	Disabled     bool    `json:"disabled"`
	Reason       string  `json:"reason"`
	UntilUnix    float64 `json:"until_unix"`
	CoolKind     string  `json:"cool_kind"`
	SuccessCount int     `json:"success_count"`
	ErrCount     int     `json:"err_count"`

	lastUsed    time.Time
	lastErr     time.Time
	lastSuccess time.Time
}

// stateKey scopes scheduler rows per region, since the same userId can exist in
// both the cn and the global directory.
func stateKey(region, uid string) string {
	if region == "" {
		region = regionCN
	}
	return region + "/" + uid
}

func (st *accountState) healthy(now time.Time) bool {
	if st.Disabled {
		return false
	}
	if st.UntilUnix > 0 && now.Before(time.Unix(0, int64(st.UntilUnix*1e9))) {
		return false
	}
	return true
}

func (st *accountState) until() time.Time {
	if st.UntilUnix <= 0 {
		return time.Time{}
	}
	return time.Unix(0, int64(st.UntilUnix*1e9))
}

// state returns the row for one account, creating it on first sight. The caller
// holds s.mu.
func (s *store) stateLocked(a *account, now time.Time) *accountState {
	snap := a.snapshot()
	k := stateKey(snap.Region, snap.UserID)
	st, ok := s.states[k]
	if !ok {
		st = &accountState{Region: snap.Region}
		s.states[k] = st
	}
	_ = now
	return st
}

// candidates returns healthy accounts in pick order, honouring the caller's
// exclusion set.
func (s *store) candidates(tried map[string]bool) []*account {
	s.mu.Lock()
	now := time.Now()
	type cand struct {
		a  *account
		st *accountState
	}
	list := make([]cand, 0, len(s.accounts))
	for _, a := range s.accounts {
		snap := a.snapshot()
		if strings.TrimSpace(snap.AccessToken) == "" {
			continue
		}
		if tried != nil && tried[snap.UserID] {
			continue
		}
		st := s.stateLocked(a, now)
		if !st.healthy(now) {
			continue
		}
		list = append(list, cand{a: a, st: st})
	}
	if len(list) == 0 {
		s.mu.Unlock()
		return nil
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].st.Points != list[j].st.Points {
			return list[i].st.Points > list[j].st.Points
		}
		return list[i].a.snapshot().UserID < list[j].a.snapshot().UserID
	})
	if len(list) > 5 {
		list = list[:5]
	}
	eligible := make([]cand, 0, len(list))
	for _, c := range list {
		if now.Sub(c.st.lastUsed) >= 100*time.Millisecond {
			eligible = append(eligible, c)
		}
	}
	var chosen cand
	if len(eligible) == 0 {
		chosen = list[0]
		for _, c := range list[1:] {
			if c.st.lastUsed.Before(chosen.st.lastUsed) {
				chosen = c
			}
		}
	} else {
		total := 0
		for _, c := range eligible {
			total += c.st.Points
		}
		if total <= 0 {
			chosen = eligible[rand.Intn(len(eligible))]
		} else {
			r := rand.Intn(total)
			acc := 0
			chosen = eligible[len(eligible)-1]
			for _, c := range eligible {
				acc += c.st.Points
				if r < acc {
					chosen = c
					break
				}
			}
		}
	}
	chosen.st.lastUsed = time.Now()
	s.mu.Unlock()
	return []*account{chosen.a}
}

// pick returns one account or nil.
func (s *store) pick(tried map[string]bool) *account {
	c := s.candidates(tried)
	if len(c) == 0 {
		return nil
	}
	return c[0]
}
