package main

import (
	"net/http"
	"strings"
)

// handleLoginPoll reports whether a credential has been stored.
func (s *server) handleLoginPoll(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/accounts/login/")
	parts := strings.SplitN(strings.Trim(rest, "/"), "/", 2)
	if len(parts) == 2 && parts[1] == "submit" {
		s.handleLoginSubmit(w, r, parts[0])
		return
	}
	sid := parts[0]
	if r.Method == http.MethodDelete {
		s.login.markCancelled(sid)
		writeJSON(w, http.StatusOK, map[string]any{"status": "failed", "error": "login cancelled"})
		return
	}
	if sess, ok := s.login.get(sid); ok {
		if sess.Cancelled {
			writeJSON(w, http.StatusOK, map[string]any{"status": "failed", "error": "login cancelled"})
			return
		}
		if !sess.Done {
			// A credential may already exist, but this dialog was opened to add
			// a new one: reporting success here would close it immediately.
			writeJSON(w, http.StatusOK, map[string]any{
				"status":  "pending",
				"message": "等待门户登录回调，或粘贴回调中的 code",
			})
			return
		}
		// This dialog finished its own exchange, so success is earned here.
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "success",
			"account_id": "default",
		})
		return
	}
	// An unknown sid (expired, cancelled, or opened before a restart) must not
	// inherit success from a credential stored by an earlier login.
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "failed",
		"message": "登录会话不存在或已过期，请重新发起 /accounts/login",
	})
}
