package webui

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// Per-plugin settings admin API (GAP 1). GET /api/plugins/{id}/settings returns
// the editable view; PUT writes a whole update. Validation lives here so the
// host callback only ever sees well-formed values, matching the pool handlers.
//
// Precedence note surfaced to the UI: the persisted row overrides the global
// config only where the user set a value. concurrency == 0 and strict ==
// "default" mean "跟随全局" (the global plugins.concurrency /
// plugins.strict_egress win); an existing install row carries a concrete copied
// value and therefore keeps overriding until the user chooses 跟随全局.

// strictStates are the accepted strict_egress settings values: "on"/"off" are
// explicit per-plugin overrides, "" / "default" mean 跟随全局.
var strictStates = map[string]bool{"": true, "default": true, "on": true, "off": true}

// maxPluginConcurrency mirrors config.MaxConcurr; the transport bounds stay in
// this package so the validation is self-contained.
const maxPluginConcurrency = 32

// settingsTimeOfDayPattern matches an HH:MM clock used by checkin_time.
var settingsTimeOfDayPattern = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

// PluginSettingsUpdate is the body of PUT /api/plugins/{id}/settings. Concurrency
// 0 and Strict "default" mean "跟随全局"; Mode and CheckinTime accept "" for the
// same reason. Enabled is optional: nil leaves the current value untouched.
type PluginSettingsUpdate struct {
	Enabled     *bool  `json:"enabled"`
	Mode        string `json:"mode"`         // always | on_demand | "" (跟随全局)
	Concurrency int    `json:"concurrency"`  // 0 = 跟随全局, else 1..32
	CheckinTime string `json:"checkin_time"` // HH:MM | "" (跟随全局)
	Strict      string `json:"strict"`       // on | off | default (跟随全局)
}

// validatePluginSettings rejects any out-of-range value before the host runs.
func validatePluginSettings(update PluginSettingsUpdate) error {
	switch update.Mode {
	case "", "always", "on_demand":
	default:
		return fmt.Errorf("mode must be always, on_demand or empty, got %q", update.Mode)
	}
	if update.Concurrency < 0 || update.Concurrency > maxPluginConcurrency {
		return fmt.Errorf("concurrency must be 0 (跟随全局) or 1..%d, got %d", maxPluginConcurrency, update.Concurrency)
	}
	if checkin := strings.TrimSpace(update.CheckinTime); checkin != "" && !settingsTimeOfDayPattern.MatchString(checkin) {
		return fmt.Errorf("checkin_time must be HH:MM or empty, got %q", update.CheckinTime)
	}
	if !strictStates[update.Strict] {
		return fmt.Errorf("strict must be on, off or default, got %q", update.Strict)
	}
	return nil
}

func (s *server) handlePluginSettings(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet, http.MethodPut) {
		return
	}
	if s.opts.GetPluginSettings == nil || s.opts.SavePluginSettings == nil || s.opts.Plugins == nil {
		s.unavailable(w)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "plugin id is required", "bad_request")
		return
	}
	if _, ok := s.pluginStatus(id); !ok {
		writeError(w, http.StatusNotFound, "plugin not found: "+id, "not_found")
		return
	}

	if r.Method == http.MethodGet {
		payload, err := s.opts.GetPluginSettings(id)
		if err != nil {
			writePluginSettingsError(w, "read plugin settings", err)
			return
		}
		if payload == nil {
			payload = map[string]any{}
		}
		writeJSON(w, http.StatusOK, payload)
		return
	}

	var body PluginSettingsUpdate
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := validatePluginSettings(body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "bad_request")
		return
	}
	payload, err := s.opts.SavePluginSettings(id, body)
	if err != nil {
		writePluginSettingsError(w, "save plugin settings", err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

// writePluginSettingsError maps the host sentinels onto the admin API contract:
// an unknown plugin is 404, an invalid value 400, anything else 500.
func writePluginSettingsError(w http.ResponseWriter, action string, err error) {
	switch {
	case errors.Is(err, ErrPluginNotFound):
		writeError(w, http.StatusNotFound, err.Error(), "not_found")
	case errors.Is(err, ErrInvalidRequest):
		writeError(w, http.StatusBadRequest, action+": "+err.Error(), "bad_request")
	default:
		writeError(w, http.StatusInternalServerError, action+": "+err.Error(), "internal_error")
	}
}
