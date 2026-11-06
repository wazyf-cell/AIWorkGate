package main

import (
	"context"
	"fmt"
	"net/http"
)

// handleRefresh renews the stored credential through /oauth2/token.
func (s *server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	creds, ok := s.store.get()
	if !ok {
		writeError(w, http.StatusConflict, "account_unavailable", "尚未登录 MiniMax")
		return
	}
	if err := s.egressError(); err != nil {
		writeError(w, http.StatusForbidden, "egress_missing", err.Error())
		return
	}
	next, err := s.refreshCredentials(r.Context(), creds)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"result": "fail", "message": err.Error()})
		return
	}
	exp, _ := next.expiresAtMs()
	writeJSON(w, http.StatusOK, map[string]any{
		"result":     "ok",
		"expires_at": exp / 1000,
	})
}

// refreshCredentials exchanges the refresh_token and persists the result.
func (s *server) refreshCredentials(ctx context.Context, creds credentials) (credentials, error) {
	if !creds.refreshable() {
		return creds, fmt.Errorf("minimax: credential has no refresh_token; log in again")
	}
	ctx, cancel := context.WithTimeout(ctx, oauthTimeout)
	defer cancel()
	grant, err := s.client.refresh(ctx, creds.AccountBaseURL, creds.RefreshToken)
	if err != nil {
		return creds, err
	}
	next := grantToCredentials(grant, creds)
	if err := s.store.set(next); err != nil {
		return creds, err
	}
	return next, nil
}
