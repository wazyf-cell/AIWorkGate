package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// canAttemptAccount reports whether an account is worth trying: enabled callers
// still decide that, but an invalid or non-refreshable expired credential is
// skipped during automatic selection and rotation.
func (p *Plugin) canAttemptAccount(a *Account) bool {
	if a == nil || a.Invalid {
		return false
	}
	if !a.Credential.usable() && !a.Credential.refreshable() {
		return false
	}
	exp := a.Credential.expiry()
	if !exp.IsZero() && !p.now().Before(exp) && !a.Credential.refreshable() {
		return false
	}
	return true
}

// accountServable reports whether a pinned account may serve a chat request.
// Contract v0.3 requires 409 for unknown, disabled or credential-less accounts
// instead of quietly falling back to another account.
func accountServable(a *Account) bool {
	if a == nil || !a.Enabled || a.Invalid {
		return false
	}
	return a.Credential.usable() || a.Credential.refreshable()
}

// pickChatAccount resolves the account for a chat request. The contract v0.3
// header X-Aiworkgate-Account wins; the older X-CodeArts-Account / X-Account-Id
// headers stay as a debug alias. Without a pinned id it keeps the v1 behavior:
// first enabled account with usable or refreshable credentials.
func (p *Plugin) pickChatAccount(r *http.Request) (*Account, error) {
	id := forcedAccountID(r)
	if id == "" {
		id = strings.TrimSpace(r.Header.Get("X-CodeArts-Account"))
	}
	if id == "" {
		id = strings.TrimSpace(r.Header.Get("X-Account-Id"))
	}
	snaps := p.accountSnapshots()
	if id != "" {
		for _, a := range snaps {
			if a.ID == id {
				if !a.Enabled {
					return nil, fmt.Errorf("account %s is disabled", id)
				}
				return a, nil
			}
		}
		return nil, fmt.Errorf("account %s not found", id)
	}
	var fallback *Account
	for _, a := range snaps {
		if !a.Enabled {
			continue
		}
		if fallback == nil && (a.Credential.usable() || a.Credential.refreshable()) {
			fallback = a
		}
		if !p.canAttemptAccount(a) {
			continue
		}
		if _, err := p.resolveEgress(a.ID); err != nil {
			continue
		}
		return a, nil
	}
	if fallback != nil {
		// No better candidate: let the caller surface the concrete error
		// (egress_missing or a credential refresh failure).
		return fallback, nil
	}
	return nil, errors.New("没有可用的 CodeArts 账号，请先登录")
}

// nextChatAccount returns a sibling account for one-shot rotation. Accounts
// whose egress is blocked under strict mode are skipped.
func (p *Plugin) nextChatAccount(exclude map[string]bool) *Account {
	for _, a := range p.accountSnapshots() {
		if !a.Enabled || exclude[a.ID] || !p.canAttemptAccount(a) {
			continue
		}
		if _, err := p.resolveEgress(a.ID); err != nil {
			continue
		}
		return a
	}
	return nil
}
