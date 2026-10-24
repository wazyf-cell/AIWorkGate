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

const (
	defaultUsageLatencyDays = 14
	defaultUsageKeyDays     = 30
	defaultUsageModelDays   = 30
	maxUsageDays            = 90
)

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
