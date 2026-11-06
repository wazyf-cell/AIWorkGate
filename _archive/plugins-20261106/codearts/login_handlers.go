package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

func (p *Plugin) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method      string            `json:"method"`
		Code        string            `json:"code"`
		URL         string            `json:"url"`
		CallbackURL string            `json:"callback_url"`
		Values      map[string]string `json:"values"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	method := strings.TrimSpace(strings.ToLower(req.Method))
	if method == "" {
		method = "oauth"
	}
	switch method {
	case "oauth", "paste_code", "ide_plugin":
	default:
		writeError(w, http.StatusBadRequest, "unsupported_login_method", "login method "+method+" is not supported (use oauth, paste_code or ide_plugin)")
		return
	}
	session, err := p.startLoginSession(method)
	if err != nil {
		var em *egressMissingError
		if errors.As(err, &em) {
			writeError(w, http.StatusForbidden, "egress_missing", em.Error())
			return
		}
		writeError(w, http.StatusBadGateway, "login_start_failed", err.Error())
		return
	}
	if method == "oauth" || method == "ide_plugin" {
		go func() {
			opener := p.opts.OpenBrowser
			if opener == nil {
				opener = openBrowserDefault
			}
			if err := opener(session.URL); err != nil {
				p.logLine("warn", "open browser failed", map[string]any{"error": err.Error()})
			}
		}()
	}
	// A one-shot paste fallback: {"method":"paste_code","code":"..."} (or a
	// full callback URL) performs the exchange immediately.
	raw := firstNonEmpty(req.Code, req.URL, req.CallbackURL)
	if raw == "" && req.Values != nil {
		raw = firstNonEmpty(req.Values["code"], req.Values["url"], req.Values["callback_url"])
	}
	if method == "paste_code" && raw != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		cred, cerr := p.credentialFromPastedCode(ctx, session, raw)
		if cerr != nil {
			p.failSession(session, cerr.Error())
			writeJSON(w, http.StatusOK, map[string]any{
				"session_id": session.ID, "status": "failed", "method": method, "error": cerr.Error(),
			})
			return
		}
		accountID, cerr := p.completeLogin(session, cred)
		if cerr != nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"session_id": session.ID, "status": "failed", "method": method, "error": cerr.Error(),
			})
			return
		}
		setAccount(r, accountID)
		writeJSON(w, http.StatusOK, map[string]any{
			"session_id": session.ID, "status": "success", "method": method, "account_id": accountID,
		})
		return
	}
	// failSession runs from the TTL timer and completeLogin from callbacks: read
	// the session under its lock so the response is one consistent snapshot.
	session.mu.Lock()
	resp := map[string]any{
		"session_id":   session.ID,
		"status":       session.Status,
		"method":       method,
		"url":          session.URL,
		"instructions": session.Instructions,
	}
	if len(session.Fields) > 0 {
		resp["fields"] = session.Fields
	}
	session.mu.Unlock()
	writeJSON(w, http.StatusOK, resp)
}

func (p *Plugin) handleLoginSubmit(w http.ResponseWriter, r *http.Request, sid string) {
	var req struct {
		Values map[string]string `json:"values"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	s := p.getSession(sid)
	if s == nil {
		writeError(w, http.StatusNotFound, "login_not_found", "login session "+sid+" not found")
		return
	}
	status, accountID, errMsg := s.snapshot()
	if status == "success" {
		writeJSON(w, http.StatusOK, map[string]any{"session_id": sid, "status": "success", "account_id": accountID})
		return
	}
	if status == "failed" {
		writeJSON(w, http.StatusOK, map[string]any{"session_id": sid, "status": "failed", "error": errMsg})
		return
	}
	raw := ""
	for _, key := range []string{"code", "url", "callback_url", "callback"} {
		if v := strings.TrimSpace(req.Values[key]); v != "" {
			raw = v
			break
		}
	}
	if raw == "" {
		writeJSON(w, http.StatusOK, map[string]any{"session_id": sid, "status": "failed", "error": "code is required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	cred, err := p.credentialFromPastedCode(ctx, s, raw)
	if err != nil {
		p.failSession(s, err.Error())
		writeJSON(w, http.StatusOK, map[string]any{"session_id": sid, "status": "failed", "error": err.Error()})
		return
	}
	accountID, err = p.completeLogin(s, cred)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"session_id": sid, "status": "failed", "error": err.Error()})
		return
	}
	setAccount(r, accountID)
	writeJSON(w, http.StatusOK, map[string]any{"session_id": sid, "status": "success", "account_id": accountID})
}
