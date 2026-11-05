package webui

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// Plugin market management routes (P2). All three sit behind the admin session
// guard with the rest of /api and follow the same callback style as every other
// endpoint: the handlers here only decode/validate and shape JSON; the host
// implements the work through Options.MarketSearch / MarketInstall /
// MarketUpdates and is the only party that touches internal/market, the plugin
// manager and the store.
//
// The wire contract is fixed here and mirrored by the frontend (task-17):
//
//	GET  /api/market/search?q=&limit=  -> {"results":[{name,version,description}]}
//	POST /api/market/install           body {"spec","source","localPath"?}
//	                                   -> {"ok":true,"id","version"}
//	GET  /api/market/updates           -> {"updates":[{id,from,to,available}]}

// marketInstallRequest is the POST /api/market/install body.
type marketInstallRequest struct {
	Spec      string `json:"spec"`
	Source    string `json:"source"`
	LocalPath string `json:"localPath,omitempty"`
}

// handleMarketSearch runs the npm search through the host callback.
func (s *server) handleMarketSearch(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if s.opts.MarketSearch == nil {
		s.unavailable(w)
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeError(w, http.StatusBadRequest, `query parameter "q" is required`, "invalid_request_error")
		return
	}
	limit := 20
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > 100 {
			writeError(w, http.StatusBadRequest, `query parameter "limit" must be 1-100`, "invalid_request_error")
			return
		}
		limit = n
	}
	results, err := s.opts.MarketSearch(q, limit)
	if err != nil {
		s.marketError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// handleMarketInstall prepares and installs one market spec through the host
// callback. A fresh market install always lands stopped (the callback decides,
// but the market flow passes Enabled:false per T3).
func (s *server) handleMarketInstall(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	if s.opts.MarketInstall == nil {
		s.unavailable(w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var body marketInstallRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error(), "invalid_request_error")
		return
	}
	if strings.TrimSpace(body.Spec) == "" {
		writeError(w, http.StatusBadRequest, `field "spec" is required`, "invalid_request_error")
		return
	}
	payload, err := s.opts.MarketInstall(
		strings.TrimSpace(body.Spec),
		strings.TrimSpace(body.Source),
		strings.TrimSpace(body.LocalPath),
	)
	if err != nil {
		s.marketError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

// handleMarketUpdates asks the host for the from→to diff of every installed
// market package.
func (s *server) handleMarketUpdates(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if s.opts.MarketUpdates == nil {
		s.unavailable(w)
		return
	}
	updates, err := s.opts.MarketUpdates()
	if err != nil {
		s.marketError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"updates": updates})
}

// marketError maps a host callback error to the admin API error conventions:
// a caller or contract violation is 400, an unknown plugin 404, everything
// else 500.
func (s *server) marketError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidRequest):
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
	case errors.Is(err, ErrPluginNotFound):
		writeError(w, http.StatusNotFound, err.Error(), "not_found")
	default:
		writeError(w, http.StatusInternalServerError, err.Error(), "internal_error")
	}
}
