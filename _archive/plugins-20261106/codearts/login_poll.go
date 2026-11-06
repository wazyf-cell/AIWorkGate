package main

import (
	"net/http"
)

func (p *Plugin) getSession(id string) *loginSession {
	p.sessionsMu.Lock()
	defer p.sessionsMu.Unlock()
	return p.sessions[id]
}

// sessionLive reports whether the id is still the registered, uncancelled session.
func (p *Plugin) sessionLive(id string) bool {
	p.sessionsMu.Lock()
	defer p.sessionsMu.Unlock()
	_, ok := p.sessions[id]
	return ok
}

func (p *Plugin) handleLoginPoll(w http.ResponseWriter, r *http.Request, sid string) {
	s := p.getSession(sid)
	if s == nil {
		writeError(w, http.StatusNotFound, "login_not_found", "login session "+sid+" not found")
		return
	}
	now := p.now()
	if now.Unix() > s.ExpiresAt {
		p.failSession(s, "CodeArts 登录超时（15 分钟）")
	}
	status, accountID, errMsg := s.snapshot()
	resp := map[string]any{"session_id": sid, "status": status}
	if s.URL != "" {
		resp["url"] = s.URL
	}
	if accountID != "" {
		resp["account_id"] = accountID
		setAccount(r, accountID)
	}
	if errMsg != "" {
		resp["error"] = errMsg
	}
	writeJSON(w, http.StatusOK, resp)
}

func (p *Plugin) handleLoginCancel(w http.ResponseWriter, r *http.Request, sid string) {
	p.sessionsMu.Lock()
	s, ok := p.sessions[sid]
	if ok {
		delete(p.sessions, sid)
	}
	p.sessionsMu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "login_not_found", "login session "+sid+" not found")
		return
	}
	s.closeListener()
	p.dropTransport(sid)
	p.logLine("info", "login session cancelled", map[string]any{"session": sid})
	w.WriteHeader(http.StatusNoContent)
}
