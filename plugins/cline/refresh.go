package main

// Token renewal. WorkOS refresh tokens are rotated on use, so two renewals of the
// same token racing each other can invalidate the account; every caller goes
// through refreshCredential, which lets a burst share one upstream call.

import (
	"net/http"
	"time"
)

// refreshCall is one in-flight renewal that concurrent callers can join.
type refreshCall struct {
	done chan struct{}
	cred *clineCredential
	err  error
}

// refreshCredential renews one refresh token at most once per concurrent burst.
// The first caller performs the upstream call; later callers wait for its result
// instead of spending the token again.
//
// Every caller gets its own copy: callers merge fields into the credential and
// persist it, so sharing one struct across goroutines would be a data race.
func (s *server) refreshCredential(refreshToken string) (*clineCredential, error) {
	s.refreshMu.Lock()
	if call, ok := s.refreshing[refreshToken]; ok {
		s.refreshMu.Unlock()
		<-call.done
		return cloneCredential(call.cred), call.err
	}
	call := &refreshCall{done: make(chan struct{})}
	if s.refreshing == nil {
		s.refreshing = map[string]*refreshCall{}
	}
	s.refreshing[refreshToken] = call
	s.refreshMu.Unlock()

	cred, err := s.client.refresh(refreshToken)
	call.cred, call.err = cred, err
	close(call.done)

	s.refreshMu.Lock()
	delete(s.refreshing, refreshToken)
	s.refreshMu.Unlock()
	return cloneCredential(cred), err
}

// cloneCredential detaches one caller's result from the shared in-flight value.
func cloneCredential(c *clineCredential) *clineCredential {
	if c == nil {
		return nil
	}
	clone := *c
	return &clone
}

// handleRefresh renews the access token through WorkOS.
//
// Refusing when there is no refresh token is deliberate: an sk-less token cannot
// be renewed, and a fake "ok" would leave the operator with a dead login that
// looks healthy.
func (s *server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	creds, ok := s.store.get()
	if !ok {
		writeError(w, http.StatusConflict, "account_unavailable", "尚未登录 Cline")
		return
	}
	if !creds.refreshable() {
		writeJSON(w, http.StatusOK, map[string]any{
			"result":  "fail",
			"message": "这条凭据没有 refresh_token，无法续期；请重新登录",
			"at":      time.Now().Unix(),
		})
		return
	}
	if err := s.egressError(); err != nil {
		writeError(w, http.StatusForbidden, "egress_missing", err.Error())
		return
	}
	renewed, err := s.refreshCredential(creds.RefreshToken)
	if err != nil {
		s.log("warn", "refresh_failed", map[string]any{"error": err.Error()})
		writeJSON(w, http.StatusOK, map[string]any{
			"result": "fail", "message": err.Error(), "at": time.Now().Unix(),
		})
		return
	}
	// Preserve fields the refresh response may omit.
	if renewed.RefreshToken == "" {
		renewed.RefreshToken = creds.RefreshToken
	}
	if renewed.AccountID == "" {
		renewed.AccountID = creds.AccountID
	}
	if renewed.Nickname == "" {
		renewed.Nickname = creds.Nickname
	}
	if err := s.store.set(renewed); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"result": "fail", "message": err.Error(), "at": time.Now().Unix(),
		})
		return
	}
	s.log("info", "refresh_succeeded", map[string]any{"fingerprint": fingerprint(renewed.AccessToken)})
	writeJSON(w, http.StatusOK, map[string]any{
		"result":     "ok",
		"expires_at": renewed.ExpireTime / 1000,
		"message":    "续期成功",
		"at":         time.Now().Unix(),
	})
}
