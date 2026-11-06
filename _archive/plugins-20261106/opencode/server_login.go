package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

type loginSession struct {
	status string
	at     time.Time
}

// rememberLogin marks an api_key session as waiting for the user's submit.
func (s *server) rememberLogin(sid string) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	s.pruneLoginsLocked(time.Now())
	s.logins[sid] = loginSession{status: "pending", at: time.Now()}
}

// forgetLogin drops a session once it has been submitted.
func (s *server) forgetLogin(sid string) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	delete(s.logins, sid)
}

// cancelLogin marks the session cancelled. It must not touch the stored
// credential: dismissing the dialog is not "log out", and a user who already
// pasted a key would otherwise lose it without warning.
func (s *server) cancelLogin(sid string) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	s.pruneLoginsLocked(time.Now())
	s.logins[sid] = loginSession{status: "cancelled", at: time.Now()}
}

func (s *server) pruneLoginsLocked(now time.Time) {
	for id, sess := range s.logins {
		if now.Sub(sess.at) > 15*time.Minute {
			delete(s.logins, id)
		}
	}
}

func (s *server) loginStatus(sid string) string {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	return s.logins[sid].status
}

func (s *server) loginPending(sid string) bool {
	return s.loginStatus(sid) == "pending"
}

// handleLoginStart begins a login flow: api_key (paste an sk- key) or
// local_state (accept the anonymous literal).
func (s *server) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	var in struct {
		Method string `json:"method"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in)
	method := strings.ToLower(strings.TrimSpace(in.Method))
	if method == "" {
		method = "api_key"
	}
	session := newUUID()
	switch method {
	case "local_state":
		if err := s.store.set(credentials{APIKey: anonymousKey, Label: "匿名通道"}); err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"session_id":   session,
			"status":       "success",
			"method":       "local_state",
			"account_id":   "default",
			"instructions": "已启用匿名免费通道（凭据是上游字面量 public），无需 API key。",
		})
		return
	case "api_key":
		s.rememberLogin(session)
		writeJSON(w, http.StatusOK, map[string]any{
			"session_id": session,
			"status":     "pending",
			"method":     "api_key",
			"fields": []map[string]any{{
				"name":        "api_key",
				"label":       "OpenCode API Key",
				"type":        "password",
				"required":    false,
				"placeholder": "sk-...",
				"help":        "留空则用匿名免费通道（字面量 public）；付费 key 以 sk- 开头，可在 https://opencode.ai/auth 生成",
			}},
			"instructions": "粘贴 sk- 开头的 key 后提交。不填也能用免费模型。",
		})
		return
	}
	writeError(w, http.StatusBadRequest, "invalid_request_error",
		"method must be api_key or local_state, got "+method)
}

// handleLoginPoll answers one login session and accepts the paste-key submit.
func (s *server) handleLoginPoll(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/accounts/login/"), "/"), "/")
	sid := ""
	if len(parts) > 0 {
		sid = parts[0]
	}
	if sid == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "session id is required")
		return
	}
	if len(parts) > 1 && parts[1] == "submit" {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
			return
		}
		var in struct {
			Values map[string]string `json:"values"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in)
		key := strings.TrimSpace(in.Values["api_key"])
		if key == "" {
			// An empty value is an explicit choice for the anonymous lane.
			key = anonymousKey
		} else if !strings.HasPrefix(key, "sk-") {
			// Anything else is neither an explicit anonymous choice nor a paid
			// key; silently treating it as anonymous would hide the mistake.
			writeError(w, http.StatusBadRequest, "invalid_request_error",
				"OpenCode 付费 key 以 sk- 开头；要用免费匿名通道请留空提交")
			return
		} else if _, err := s.client.catalog(r.Context(), key); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_credential",
				"上游拒绝了这把 key："+err.Error())
			return
		}
		if err := s.store.set(credentials{APIKey: key, Label: loginLabel(key)}); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		s.log("info", "credential_stored", map[string]any{
			"anonymous": key == anonymousKey, "fingerprint": fingerprint(key),
		})
		s.forgetLogin(sid)
		writeJSON(w, http.StatusOK, map[string]any{"status": "success", "account_id": "default"})
		return
	}
	if r.Method == http.MethodDelete {
		s.cancelLogin(sid)
		writeJSON(w, http.StatusOK, map[string]any{"status": "failed", "error": "login cancelled"})
		return
	}
	if s.loginStatus(sid) == "cancelled" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "failed", "error": "login cancelled"})
		return
	}
	if s.loginPending(sid) {
		// The anonymous lane is always usable, but this session exists because the
		// user asked to paste a paid key. Reporting success here would close the
		// dialog before they could type it.
		writeJSON(w, http.StatusOK, map[string]any{"status": "pending"})
		return
	}
	creds := s.store.current()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "success",
		"account_id": "default",
		"detected": []map[string]any{{
			"label": firstNonEmpty(creds.Label, "OpenCode 匿名通道"),
			"hint":  keyHint(creds.APIKey),
		}},
	})
}
