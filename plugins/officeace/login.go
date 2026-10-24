package main

import (
	"context"
	"net/http"
	"strings"
	"time"
)

const loginInstructions = "请打开 OfficeAce 并登录后重试"

type loginField struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Type        string `json:"type"`
	Required    bool   `json:"required,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
}

type loginSession struct {
	ID           string
	Method       string
	Status       string
	AccountID    string
	Fields       []loginField
	Instructions string
	Error        string
	CreatedAt    int64
}

func (p *Plugin) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method string `json:"method"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	method := strings.TrimSpace(req.Method)
	if method == "" {
		method = "local_state"
	}
	if method != "local_state" {
		writeError(w, http.StatusBadRequest, "unsupported_login_method",
			"login method "+method+" is not supported; OfficeAce only supports local_state")
		return
	}
	s := &loginSession{
		ID:        "ls_" + randomHex(4),
		Method:    method,
		Status:    "pending",
		CreatedAt: p.now().Unix(),
	}
	p.sessionsMu.Lock()
	for id, old := range p.sessions {
		if p.now().Unix()-old.CreatedAt > 3600 {
			delete(p.sessions, id)
		}
	}
	p.sessions[s.ID] = s
	p.sessionsMu.Unlock()

	p.runLoginCheck(r.Context(), s)
	p.writeLoginState(w, s)
}

func (p *Plugin) runLoginCheck(parent context.Context, s *loginSession) {
	ctx, cancel := context.WithTimeout(parent, 25*time.Second)
	defer cancel()

	disc := p.refreshDiscovery()
	if !disc.Found {
		p.sessionsMu.Lock()
		s.Status = "pending"
		s.Instructions = "未找到 OfficeAce：请先安装并启动 OfficeAce 并登录；如为自定义安装目录，请提交 officeace_home"
		s.Fields = loginHomeFields(disc)
		s.Error = ""
		p.sessionsMu.Unlock()
		p.setStatus(pluginStatusNotFound, disc.statusMessage())
		return
	}

	cred, err := p.liveCredential(ctx)
	if err != nil {
		code, msg := statusFromDBError(err)
		p.sessionsMu.Lock()
		s.Status = "pending"
		s.Instructions = loginInstructions
		s.Error = msg
		s.Fields = loginHomeFields(disc)
		p.sessionsMu.Unlock()
		p.setStatus(code, msg)
		return
	}
	acct := p.accountForCredential(cred)

	if !cred.HasUpstreamCredential() {
		msg := "OfficeAce 会话缺少 MaaS 模型凭据，请先在 OfficeAce 中完成模型配置后重试"
		p.sessionsMu.Lock()
		s.Status = "pending"
		s.Instructions = msg
		s.AccountID = acct.ID
		s.Error = ""
		p.sessionsMu.Unlock()
		p.setAccountStatus(acct.ID, statusInvalid, msg)
		p.setStatus(pluginStatusExpired, msg)
		return
	}
	if cred.Expired(p.now()) && p.autoRefreshEnabled() {
		entry, egressErr := p.resolveEgress(acct.ID)
		if egressErr == nil {
			proxyURL := ""
			if entry.Mode == egressProxy {
				proxyURL = entry.URL
			}
			if refreshed, rerr := p.refreshCredential(ctx, cred, acct.ID, proxyURL, false); rerr == nil {
				cred = refreshed
				acct = p.accountForCredential(cred)
			} else {
				p.logLine("info", "login refresh attempt failed", map[string]any{"account": acct.ID, "error": rerr.Error()})
			}
		}
	}
	status, statusMsg := p.accountSessionStatus(cred)
	p.sessionsMu.Lock()
	s.AccountID = acct.ID
	s.Fields = nil
	s.Error = ""
	if status == statusOK {
		s.Status = "success"
		s.Instructions = ""
	} else {
		s.Status = "pending"
		s.Instructions = loginInstructions
		if statusMsg != "" {
			s.Error = statusMsg
		}
	}
	p.sessionsMu.Unlock()
	p.setAccountStatus(acct.ID, status, statusMsg)
	if status == statusOK {
		p.setStatus(pluginStatusOK, "")
	} else {
		p.setStatus(pluginStatusExpired, statusMsg)
	}
}

func loginHomeFields(disc Discovery) []loginField {
	placeholder := defaultOfficeAceHome
	if disc.Home != "" {
		placeholder = disc.Home
	}
	return []loginField{{
		Name:        "officeace_home",
		Label:       "OfficeAce 安装目录",
		Type:        "text",
		Required:    false,
		Placeholder: placeholder,
	}}
}

func (p *Plugin) writeLoginState(w http.ResponseWriter, s *loginSession) {
	p.sessionsMu.Lock()
	resp := map[string]any{
		"session_id":   s.ID,
		"status":       s.Status,
		"method":       s.Method,
		"instructions": s.Instructions,
	}
	if s.AccountID != "" {
		resp["account_id"] = s.AccountID
	}
	if len(s.Fields) > 0 {
		resp["fields"] = s.Fields
	}
	if s.Error != "" {
		resp["error"] = s.Error
	}
	p.sessionsMu.Unlock()
	writeJSON(w, http.StatusOK, resp)
}

func (p *Plugin) getSession(id string) *loginSession {
	p.sessionsMu.Lock()
	defer p.sessionsMu.Unlock()
	return p.sessions[id]
}

func (p *Plugin) handleLoginPoll(w http.ResponseWriter, r *http.Request, sid string) {
	s := p.getSession(sid)
	if s == nil {
		writeError(w, http.StatusNotFound, "login_not_found", "login session "+sid+" not found")
		return
	}
	p.runLoginCheck(r.Context(), s)
	p.writeLoginState(w, s)
	p.sessionsMu.Lock()
	accountID := s.AccountID
	p.sessionsMu.Unlock()
	if accountID != "" {
		setAccount(r, accountID)
	}
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
	if s.Method != "local_state" {
		writeError(w, http.StatusBadRequest, "invalid_login_state", "login method "+s.Method+" does not accept form values")
		return
	}

	home := strings.TrimSpace(req.Values["officeace_home"])
	if home != "" {
		disc := discoverIsolated(home, userClawHome())
		if !disc.Found {
			p.sessionsMu.Lock()
			s.Status = "pending"
			s.Error = "指定的 officeace_home 下未找到 data\\storage.sqlite：" + strings.Join(disc.Tried, "; ")
			s.Fields = loginHomeFields(disc)
			p.sessionsMu.Unlock()
			p.writeLoginState(w, s)
			return
		}
		p.cfgMu.Lock()
		p.cfg.OfficeAceHome = home
		cfg := p.cfg
		p.cfgMu.Unlock()
		if err := saveConfig(p.DataDir, cfg); err != nil {
			p.logLine("warn", "save config failed", map[string]any{"error": err.Error()})
			p.sessionsMu.Lock()
			s.Error = "保存 officeace_home 失败：" + err.Error()
			p.sessionsMu.Unlock()
			p.writeLoginState(w, s)
			return
		}
		p.refreshDiscovery()
		p.logLine("info", "officeace home configured via login", map[string]any{"home": disc.Home})
	}
	p.runLoginCheck(r.Context(), s)
	p.writeLoginState(w, s)
}

func (p *Plugin) handleLoginCancel(w http.ResponseWriter, r *http.Request, sid string) {
	p.sessionsMu.Lock()
	_, ok := p.sessions[sid]
	if ok {
		delete(p.sessions, sid)
	}
	p.sessionsMu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "login_not_found", "login session "+sid+" not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
