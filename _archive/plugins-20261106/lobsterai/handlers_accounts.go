package main

import (
	"net/http"
	"strings"
)

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"status":     "ok",
		"version":    pluginVersion,
		"configured": s.store.configured(),
		"sealing":    s.store.sealKind,
	})
}

func (s *server) handleMeta(w http.ResponseWriter, r *http.Request) {
	creds, ok := s.store.get()
	out := map[string]any{
		"id":            s.cfg.ID,
		"name":          "LobsterAI（有道）",
		"version":       pluginVersion,
		"api_level":     s.cfg.APILevel,
		"capabilities":  capabilities,
		"account_count": boolToInt(ok),
		"configured":    ok,
		"sealing":       s.store.sealKind,
		"base_url":      originOf(creds.BaseURL),
		"client":        userAgent,
	}
	if label := lobsteraiDisplayNickname(creds); label != "" {
		out["label"] = label
	}
	writeJSON(w, http.StatusOK, out)
}

// lobsteraiDisplayNickname ports lobsteraiDisplayNickname + maskLobsteraiPhoneTail:
// the account label is the masked nickname, falling back to the user id.
func lobsteraiDisplayNickname(c credentials) string {
	nickname := strings.TrimSpace(c.Nickname)
	if nickname == "" {
		return strings.TrimSpace(firstNonEmpty(c.Label, c.UserID))
	}
	// Mask a phone-like nickname: keep the first 3 chars and the last 2.
	if len(nickname) == 11 && nickname[0] == '1' {
		for _, ch := range nickname {
			if ch < '0' || ch > '9' {
				return nickname
			}
		}
		return nickname[:3] + strings.Repeat("*", 6) + nickname[9:]
	}
	return nickname
}

// handleAccounts reports one account while a token is stored. The account keeps
// the host scheduler supplied: an empty list with an api_level 2 plugin makes
// every request fail 503 ErrNoCandidate before it reaches this plugin. quota is
// left empty (unknown) rather than fabricated.
func (s *server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	creds, ok := s.store.get()
	accounts := []map[string]any{}
	if ok {
		accounts = append(accounts, map[string]any{
			"id":      "default",
			"label":   firstNonEmpty(lobsteraiDisplayNickname(creds), "LobsterAI"),
			"enabled": true,
			"status":  "ok",
			"credential": map[string]any{
				// refreshable mirrors isLobsteraiRefreshable: true only when a
				// refresh token was stored.
				"present":     true,
				"refreshable": strings.TrimSpace(creds.RefreshToken) != "",
			},
			"identity": s.identityOf(creds),
			"quota":    map[string]any{},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
}
