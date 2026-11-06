package main

import (
	"context"
	"net/http"
	"strings"
	"time"
)

func (p *Plugin) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"version":  Version,
		"uptime_s": p.uptimeSeconds(),
	})
}

func (p *Plugin) handleMeta(w http.ResponseWriter, r *http.Request) {
	disc := p.discovery()
	code, msg := p.status()
	serviceUp, serviceAt := p.service()
	var serviceChecked int64
	if !serviceAt.IsZero() {
		serviceChecked = serviceAt.Unix()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            p.ID,
		"version":       Version,
		"api_level":     APIVersion,
		"capabilities":  capabilities,
		"account_count": p.accountCount(),
		"uptime_s":      p.uptimeSeconds(),
		"status":        code,
		"message":       msg,
		"officeace": map[string]any{
			"found":            disc.Found,
			"home":             disc.Home,
			"source":           disc.Source,
			"db_path":          disc.DBPath,
			"service_up":       serviceUp,
			"service_checked":  serviceChecked,
			"user_claw_home":   disc.UserClawHome,
			"session_db_reads": "read-only",
		},
	})
}

func (p *Plugin) handleModels(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	// The host's "立即同步" passes refresh=1 to bypass the 6h plugin cache.
	refresh := r.URL.Query().Get("refresh")
	force := refresh == "1" || strings.EqualFold(refresh, "true")
	models, source, err := modelsSnapshotFunc(p, ctx, force)
	if err != nil && len(models) == 0 {
		// Never answer 200 with an empty catalog: the host caches any 200 as a
		// known model list for 30s and then 404s every officeace/<model> request.
		p.logLine("warn", "model catalog unavailable", map[string]any{"error": err.Error()})
		writeError(w, http.StatusServiceUnavailable, "models_unavailable", err.Error())
		return
	}
	resp := map[string]any{
		"object": "list",
		"data":   modelWireObjects(models, p.startedAt.Unix()),
		"source": source,
	}
	if err != nil {
		resp["officeace_error"] = err.Error()
		p.logLine("warn", "model catalog unavailable", map[string]any{"error": err.Error()})
	}
	writeJSON(w, http.StatusOK, resp)
}

func (p *Plugin) handleReload(w http.ResponseWriter, r *http.Request) {
	p.refreshDiscovery()
	res, err := p.reload()
	if err != nil {
		res.Errors = append(res.Errors, err.Error())
	}
	p.initializeSession()
	writeJSON(w, http.StatusOK, res)
}

func (p *Plugin) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "not_found", "no such endpoint: "+r.URL.Path)
}
