package webui

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"aiworkgate/internal/store"
)

// Observability endpoints over raw request logs (plan 2026-10-21 §5). They are
// separate from the usage_daily views because latency percentiles need the
// per-request rows and key accounting lives in client_key_usage.

// RetentionInfo is the body of GET /api/usage/retention (D-32⑦): the log
// retention windows in days, mirrored from the host config so the usage/log
// pages can flag a query window that reaches past the stored data. The
// aggregate window (usage_daily) is not pruned by these numbers, so the UI
// compares only the request-log window against its queries.
type RetentionInfo struct {
	APIRequestDays  int `json:"api_request_days"`
	BodyPreviewDays int `json:"body_preview_days"`
	HostLogDays     int `json:"host_log_days"`
	ProxyLogDays    int `json:"proxy_log_days"`
}

const (
	defaultUsageLatencyDays = 14
	defaultUsageKeyDays     = 30
	defaultUsageModelDays   = 30
	defaultUsageHeatmapDays = 26 * 7
	maxUsageDays            = 90
)

// handleRetention serves GET /api/usage/retention: a read-only mirror of the
// retention windows (D-32⑦). A nil callback answers 501 and the UI stays
// silent instead of guessing a default.
func (s *server) handleRetention(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if s.opts.Retention == nil {
		s.unavailable(w)
		return
	}
	info, err := s.opts.Retention()
	if err != nil {
		writeUsageError(w, "usage retention", err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *server) handleUsageLatency(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if s.opts.UsageLatency == nil {
		s.unavailable(w)
		return
	}
	days, err := parseUsageDays(r.URL.Query().Get("days"), defaultUsageLatencyDays)
	if err != nil {
		writeError(w, http.StatusBadRequest, "usage latency: "+err.Error(), "invalid_query")
		return
	}
	rows, err := s.opts.UsageLatency(days)
	if err != nil {
		writeUsageError(w, "usage latency", err)
		return
	}
	if rows == nil {
		rows = []store.DayLatency{}
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *server) handleUsageKeys(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if s.opts.UsageKeyUsage == nil {
		s.unavailable(w)
		return
	}
	days, err := parseUsageDays(r.URL.Query().Get("days"), defaultUsageKeyDays)
	if err != nil {
		writeError(w, http.StatusBadRequest, "usage keys: "+err.Error(), "invalid_query")
		return
	}
	rows, err := s.opts.UsageKeyUsage(days)
	if err != nil {
		writeUsageError(w, "usage keys", err)
		return
	}
	if rows == nil {
		rows = []store.KeyUsageRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// handleUsageModels serves GET /api/usage/models: the per-model consumption
// ranking (A: CreditsTab.tsx:533-563, usage.rs:143-145). The window defaults to
// 30 days; rows are already ordered by requests.
func (s *server) handleUsageModels(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if s.opts.UsageModels == nil {
		s.unavailable(w)
		return
	}
	days, err := parseUsageDays(r.URL.Query().Get("days"), defaultUsageModelDays)
	if err != nil {
		writeError(w, http.StatusBadRequest, "usage models: "+err.Error(), "invalid_query")
		return
	}
	rows, err := s.opts.UsageModels(days)
	if err != nil {
		writeUsageError(w, "usage models", err)
		return
	}
	if rows == nil {
		rows = []store.ModelUsageRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// handleUsageModelLatency serves GET /api/usage/latency/models: the same
// nearest-rank percentiles as /api/usage/latency, grouped by model
// (A: usage.rs:191-196). The window defaults to 14 days.
func (s *server) handleUsageModelLatency(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if s.opts.UsageModelLatency == nil {
		s.unavailable(w)
		return
	}
	days, err := parseUsageDays(r.URL.Query().Get("days"), defaultUsageLatencyDays)
	if err != nil {
		writeError(w, http.StatusBadRequest, "usage model latency: "+err.Error(), "invalid_query")
		return
	}
	rows, err := s.opts.UsageModelLatency(days)
	if err != nil {
		writeUsageError(w, "usage model latency", err)
		return
	}
	if rows == nil {
		rows = []store.ModelLatencyRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// handleUsageHeatmap serves GET /api/usage/heatmap: per-day request/error
// totals from usage_daily for the calendar heatmap (D-32①). The window
// defaults to the 26 weeks the UI draws.
func (s *server) handleUsageHeatmap(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if s.opts.UsageHeatmap == nil {
		s.unavailable(w)
		return
	}
	days, err := parseUsageDays(r.URL.Query().Get("days"), defaultUsageHeatmapDays)
	if err != nil {
		writeError(w, http.StatusBadRequest, "usage heatmap: "+err.Error(), "invalid_query")
		return
	}
	rows, err := s.opts.UsageHeatmap(days)
	if err != nil {
		writeUsageError(w, "usage heatmap", err)
		return
	}
	if rows == nil {
		rows = []store.DailyCount{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// handleUsageReasoning serves GET /api/usage/reasoning: the (model ×
// reasoning_tokens>0) split of the raw request logs (D-32⑤).
func (s *server) handleUsageReasoning(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if s.opts.UsageReasoning == nil {
		s.unavailable(w)
		return
	}
	days, err := parseUsageDays(r.URL.Query().Get("days"), defaultUsageModelDays)
	if err != nil {
		writeError(w, http.StatusBadRequest, "usage reasoning: "+err.Error(), "invalid_query")
		return
	}
	rows, err := s.opts.UsageReasoning(days)
	if err != nil {
		writeUsageError(w, "usage reasoning", err)
		return
	}
	if rows == nil {
		rows = []store.ReasoningSplitRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// parseUsageDays accepts an optional 1..90 day window. An absent value takes
// the endpoint default; anything else malformed or out of range is a 400 so a
// typo never silently returns a different window.
func parseUsageDays(raw string, fallback int) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}
	days, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("days must be an integer between 1 and %d, got %q", maxUsageDays, raw)
	}
	if days < 1 || days > maxUsageDays {
		return 0, fmt.Errorf("days must be between 1 and %d, got %d", maxUsageDays, days)
	}
	return days, nil
}
