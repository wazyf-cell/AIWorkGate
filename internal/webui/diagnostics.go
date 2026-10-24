package webui

// Connectivity probe API (Settings → 连通性自检).
//
//	POST /api/diagnostics/probe
//	  {"kind":"a"}
//	  {"kind":"plugin","plugin_id":"officeace","model":"glm-5.3"}
//	  {"kind":"custom_model","custom_model_id":"cm-xxx"}
//	  -> 200 {"ok","kind","target","target_id","status","latency_ms","detail",
//	          "error_code"?,"at","probe_id"}
//	A failed probe is still 200 with ok=false and a redacted detail; validation
//	failures are 400 invalid_request.
//
//	GET /api/diagnostics/probes?limit=20&kind=&target_id=
//	  -> 200 {"probes":[{"id","kind","target","target_id","ok","status",
//	                     "latency_ms","detail","at"}]}
//	Newest first; limit defaults to 20 and is capped at 100. Both probe rows and
//	the history payload never carry a credential or a URL query string.
//
// The transport only validates and shapes; running the probe, redacting its
// detail and persisting the row belong to the host callbacks.

import (
	"net/http"
	"strings"
)

const (
	diagnosticsProbeDefaultLimit = 20
	diagnosticsProbeMaxLimit     = 100
)

// Probe kinds accepted by POST /api/diagnostics/probe.
const (
	ProbeKindA           = "a"
	ProbeKindPlugin      = "plugin"
	ProbeKindCustomModel = "custom_model"
)

// ProbeRequest is a validated probe request. Model is the bare model id (a
// "<plugin>/<id>" form is accepted by the host too); CustomModelID names one
// stored custom model.
type ProbeRequest struct {
	Kind          string
	PluginID      string
	Model         string
	CustomModelID string
}

// ProbeResult is the POST response. ErrorCode is one of unreachable, timeout,
// model_missing, not_configured or upstream_error and stays empty on success.
type ProbeResult struct {
	OK        bool   `json:"ok"`
	Kind      string `json:"kind"`
	Target    string `json:"target"`
	TargetID  string `json:"target_id"`
	Status    int    `json:"status"`
	LatencyMS int64  `json:"latency_ms"`
	Detail    string `json:"detail"`
	ErrorCode string `json:"error_code,omitempty"`
	At        int64  `json:"at"`
	ProbeID   int64  `json:"probe_id"`
}

// probeHistoryItem is one row of GET /api/diagnostics/probes. The list shape is
// deliberately narrower than the store row: error_code is kept for the probe
// response but not part of the history contract.
type probeHistoryItem struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	Target    string `json:"target"`
	TargetID  string `json:"target_id"`
	OK        bool   `json:"ok"`
	Status    int    `json:"status"`
	LatencyMS int64  `json:"latency_ms"`
	Detail    string `json:"detail"`
	At        int64  `json:"at"`
}

// handleDiagnosticsProbe serves POST /api/diagnostics/probe.
func (s *server) handleDiagnosticsProbe(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	if s.opts.ProbeConnectivity == nil {
		s.unavailable(w)
		return
	}
	var body struct {
		Kind          string `json:"kind"`
		PluginID      string `json:"plugin_id"`
		Model         string `json:"model"`
		CustomModelID string `json:"custom_model_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	req := ProbeRequest{
		Kind:          strings.ToLower(strings.TrimSpace(body.Kind)),
		PluginID:      strings.TrimSpace(body.PluginID),
		Model:         strings.TrimSpace(body.Model),
		CustomModelID: strings.TrimSpace(body.CustomModelID),
	}
	switch req.Kind {
	case ProbeKindA:
		// No extra parameters: kind selects A's resolved base URL.
	case ProbeKindPlugin:
		if req.PluginID == "" || req.Model == "" {
			writeError(w, http.StatusBadRequest, "plugin 探测需要 plugin_id 和 model", "invalid_request")
			return
		}
	case ProbeKindCustomModel:
		if req.CustomModelID == "" {
			writeError(w, http.StatusBadRequest, "custom_model 探测需要 custom_model_id", "invalid_request")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "kind 必须是 a、plugin 或 custom_model", "invalid_request")
		return
	}

	result, err := s.opts.ProbeConnectivity(req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "连通性探测失败: "+err.Error(), "internal_error")
		return
	}
	// The response always echoes the validated request kind.
	result.Kind = req.Kind
	writeJSON(w, http.StatusOK, result)
}

// handleDiagnosticsProbes serves GET /api/diagnostics/probes.
func (s *server) handleDiagnosticsProbes(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if s.opts.ProbeHistory == nil {
		s.unavailable(w)
		return
	}
	query := r.URL.Query()
	limit := parseLimit(query.Get("limit"), diagnosticsProbeDefaultLimit, diagnosticsProbeMaxLimit)
	kind := strings.ToLower(strings.TrimSpace(query.Get("kind")))
	switch kind {
	case "", ProbeKindA, ProbeKindPlugin, ProbeKindCustomModel:
	default:
		writeError(w, http.StatusBadRequest, "kind 必须是 a、plugin 或 custom_model", "invalid_query")
		return
	}
	targetID := strings.TrimSpace(query.Get("target_id"))

	rows, err := s.opts.ProbeHistory(limit, kind, targetID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取探测历史失败: "+err.Error(), "internal_error")
		return
	}
	probes := make([]probeHistoryItem, 0, len(rows))
	for _, row := range rows {
		probes = append(probes, probeHistoryItem{
			ID:        row.ID,
			Kind:      row.Kind,
			Target:    row.Target,
			TargetID:  row.TargetID,
			OK:        row.OK,
			Status:    row.Status,
			LatencyMS: row.LatencyMS,
			Detail:    row.Detail,
			At:        row.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"probes": probes})
}
