package main

import (
	"net/http"
	"strings"
)

func (p *Plugin) route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if len(path) > 1 {
		path = strings.TrimSuffix(path, "/")
	}
	switch path {
	case "/health":
		if r.Method == http.MethodGet {
			p.handleHealth(w, r)
			return
		}
	case "/meta":
		if r.Method == http.MethodGet {
			p.handleMeta(w, r)
			return
		}
	case "/v1/models":
		if r.Method == http.MethodGet {
			p.handleModels(w, r)
			return
		}
	case "/v1/chat/completions":
		if r.Method == http.MethodPost {
			p.handleChat(w, r)
			return
		}
	case "/reload":
		if r.Method == http.MethodPost {
			p.handleReload(w, r)
			return
		}
	}
	segs := strings.Split(strings.Trim(path, "/"), "/")
	if len(segs) > 0 && segs[0] == "accounts" {
		p.routeAccounts(w, r, segs)
		return
	}
	p.handleNotFound(w, r)
}

func (p *Plugin) routeAccounts(w http.ResponseWriter, r *http.Request, segs []string) {
	switch {
	case len(segs) == 1 && r.Method == http.MethodGet:
		p.handleAccounts(w, r)
	case len(segs) == 2 && segs[1] == "login" && r.Method == http.MethodPost:
		p.handleLoginStart(w, r)
	case len(segs) == 2 && r.Method == http.MethodPatch:
		p.handleAccountPatch(w, r, segs[1])
	case len(segs) == 2 && r.Method == http.MethodDelete:
		p.handleAccountDelete(w, r, segs[1])
	case len(segs) == 3 && segs[1] == "login" && r.Method == http.MethodGet:
		p.handleLoginPoll(w, r, segs[2])
	case len(segs) == 3 && segs[1] == "login" && r.Method == http.MethodDelete:
		p.handleLoginCancel(w, r, segs[2])
	case len(segs) == 3:
		id, action := segs[1], segs[2]
		switch {
		case action == "checkin" && r.Method == http.MethodPost:
			p.handleCheckin(w, r, id)
		case action == "refresh" && r.Method == http.MethodPost:
			p.handleRefresh(w, r, id)
		default:
			p.handleNotFound(w, r)
		}
	case len(segs) == 4 && segs[1] == "login" && segs[3] == "submit" && r.Method == http.MethodPost:
		p.handleLoginSubmit(w, r, segs[2])
	case len(segs) == 4 && segs[2] == "identity" && segs[3] == "reset" && r.Method == http.MethodPost:
		p.handleIdentityReset(w, r, segs[1])
	default:
		p.handleNotFound(w, r)
	}
}

func (p *Plugin) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"version":  Version,
		"uptime_s": p.uptimeSeconds(),
	})
}

func (p *Plugin) handleMeta(w http.ResponseWriter, r *http.Request) {
	p.mu.RLock()
	n := len(p.accounts)
	p.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            p.ID,
		"name":          "CodeArts",
		"version":       Version,
		"api_level":     APIVersion,
		"capabilities":  capabilities,
		"account_count": n,
		"uptime_s":      p.uptimeSeconds(),
		"started_at":    p.startedAt.Unix(),
	})
}

func (p *Plugin) handleReload(w http.ResponseWriter, r *http.Request) {
	res, err := p.reload()
	if err != nil {
		res.Errors = append(res.Errors, err.Error())
	}
	writeJSON(w, http.StatusOK, res)
}

func (p *Plugin) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "not_found", "no such endpoint: "+r.URL.Path)
}
