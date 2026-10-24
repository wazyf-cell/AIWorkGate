package webui

// Model override editor (plan 2026-10-21 §3). The admin API lets the user pin
// display name / vendor / context window / vision / thinking tiers / multiplier
// / free for one model of one plugin through a human override layer; the
// gateway merges it over whatever the plugin reports (see
// internal/gateway/models.go and internal/store/model_overrides.go).
//
// Wire shape:
//
//	GET    /api/pools/{plugin}/models/overrides
//	       -> {"plugin_id": "...", "overrides": {"<model>": {
//	              "display_name": ..., "vendor": ..., "context_length": ...,
//	              "vision": ..., "thinking": [...], "multiplier": ...,
//	              "free": ..., "updated_at": ...,
//	              "overridden": ["display_name", ...]}}}
//	PUT    /api/pools/{plugin}/models/overrides/{model}
//	       body: any subset of the seven fields; a missing field keeps the
//	       stored value, an explicit null clears it (A's "None = 未人工指定"
//	       convention, unified_catalog.rs:165-210).
//	DELETE /api/pools/{plugin}/models/overrides/{model}
//
// The host injects the three storage callbacks through Options; an unwired
// callback answers 501 so this package stays transport-only.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"unicode/utf8"

	"aiworkgate/internal/store"
)

const (
	// maxModelOverrideNameRunes bounds display_name; the store enforces the
	// same width so a rejected request never reaches the database.
	maxModelOverrideNameRunes = 128
	// maxModelOverrideVendorRunes bounds vendor.
	maxModelOverrideVendorRunes = 64
	// maxModelOverrideContext bounds context_length; 100M is far above any
	// real model and keeps a typo from poisoning the catalog.
	maxModelOverrideContext = 100_000_000
	// maxModelOverrideMultiplier bounds a hand-entered price multiplier.
	maxModelOverrideMultiplier = 1000.0
	// maxModelOverrideLevels bounds the thinking tiers of one override.
	maxModelOverrideLevels = 12
	// maxModelOverrideLevelRunes bounds one thinking tier literal.
	maxModelOverrideLevelRunes = 32
)

// modelOverrideView is one stored override plus the list of fields the user
// actually pinned, so the resource page can badge them.
type modelOverrideView struct {
	store.ModelOverride
	Overridden []string `json:"overridden"`
}

func newModelOverrideView(o store.ModelOverride) modelOverrideView {
	return modelOverrideView{ModelOverride: o, Overridden: o.Fields()}
}

// modelOverrideReady answers 501 when the host did not wire the overrides.
func (s *server) modelOverrideReady(w http.ResponseWriter) bool {
	if s.opts.ModelOverrides == nil || s.opts.PutModelOverride == nil || s.opts.DeleteModelOverride == nil {
		s.unavailable(w)
		return false
	}
	return true
}

// modelOverrideTarget reads the {plugin} and {model} path values.
func modelOverrideTarget(w http.ResponseWriter, r *http.Request, needModel bool) (string, string, bool) {
	pluginID := strings.TrimSpace(r.PathValue("plugin"))
	if pluginID == "" {
		writeError(w, http.StatusBadRequest, "plugin id is required", "bad_request")
		return "", "", false
	}
	modelID := strings.TrimSpace(r.PathValue("model"))
	if needModel && modelID == "" {
		writeError(w, http.StatusBadRequest, "model id is required", "bad_request")
		return "", "", false
	}
	return pluginID, modelID, true
}

// handleModelOverrides serves GET /api/pools/{plugin}/models/overrides.
func (s *server) handleModelOverrides(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if !s.modelOverrideReady(w) {
		return
	}
	pluginID, _, ok := modelOverrideTarget(w, r, false)
	if !ok {
		return
	}
	overrides, err := s.opts.ModelOverrides(pluginID)
	if err != nil {
		writeSchedulerError(w, "model overrides", err)
		return
	}
	views := make(map[string]modelOverrideView, len(overrides))
	for modelID, override := range overrides {
		views[modelID] = newModelOverrideView(override)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"plugin_id": pluginID,
		"overrides": views,
	})
}

// handleModelOverride serves PUT and DELETE for one model.
func (s *server) handleModelOverride(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPut, http.MethodDelete) {
		return
	}
	if !s.modelOverrideReady(w) {
		return
	}
	pluginID, modelID, ok := modelOverrideTarget(w, r, true)
	if !ok {
		return
	}
	if r.Method == http.MethodDelete {
		s.deleteModelOverride(w, pluginID, modelID)
		return
	}
	s.putModelOverride(w, r, pluginID, modelID)
}

// putModelOverride applies a partial update: absent fields keep the stored
// value, explicit null (or an empty string / list) clears the field, and a body
// that clears everything removes the row.
func (s *server) putModelOverride(w http.ResponseWriter, r *http.Request, pluginID, modelID string) {
	var body map[string]json.RawMessage
	if !decodeJSON(w, r, &body) {
		return
	}
	patch, present, err := parseModelOverridePatch(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request")
		return
	}
	if len(present) == 0 {
		writeError(w, http.StatusBadRequest,
			"body must contain at least one of display_name, vendor, context_length, vision, thinking, multiplier, free",
			"invalid_request")
		return
	}

	current, err := s.opts.ModelOverrides(pluginID)
	if err != nil {
		writeSchedulerError(w, "model overrides", err)
		return
	}
	merged := current[modelID]
	applyModelOverridePatch(&merged, patch, present)

	if merged.Empty() {
		if err := s.opts.DeleteModelOverride(pluginID, modelID); err != nil {
			writeSchedulerError(w, "clear model override", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":        true,
			"plugin_id": pluginID,
			"model":     modelID,
			"override":  nil,
		})
		return
	}
	if err := s.opts.PutModelOverride(pluginID, modelID, merged); err != nil {
		if errors.Is(err, store.ErrInvalidArgument) {
			writeError(w, http.StatusBadRequest, err.Error(), "invalid_request")
			return
		}
		writeSchedulerError(w, "save model override", err)
		return
	}

	// Re-read so the response carries the timestamp the store wrote.
	if saved, err := s.opts.ModelOverrides(pluginID); err == nil {
		if override, ok := saved[modelID]; ok {
			merged = override
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"plugin_id": pluginID,
		"model":     modelID,
		"override":  newModelOverrideView(merged),
	})
}

// deleteModelOverride clears one row. Deleting twice is not an error; the
// response reports whether a row existed.
func (s *server) deleteModelOverride(w http.ResponseWriter, pluginID, modelID string) {
	current, err := s.opts.ModelOverrides(pluginID)
	if err != nil {
		writeSchedulerError(w, "model overrides", err)
		return
	}
	_, removed := current[modelID]
	if err := s.opts.DeleteModelOverride(pluginID, modelID); err != nil {
		writeSchedulerError(w, "clear model override", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"plugin_id": pluginID,
		"model":     modelID,
		"removed":   removed,
	})
}

// parseModelOverridePatch validates the PUT body. Every value is checked here
// so a rejected request never reaches the store.
func parseModelOverridePatch(body map[string]json.RawMessage) (store.ModelOverride, map[string]bool, error) {
	var patch store.ModelOverride
	present := make(map[string]bool, len(body))
	for key, raw := range body {
		switch key {
		case "display_name":
			present[key] = true
			value, err := optionalText(key, raw, maxModelOverrideNameRunes)
			if err != nil {
				return store.ModelOverride{}, nil, err
			}
			patch.DisplayName = value
		case "vendor":
			present[key] = true
			value, err := optionalText(key, raw, maxModelOverrideVendorRunes)
			if err != nil {
				return store.ModelOverride{}, nil, err
			}
			patch.Vendor = value
		case "context_length":
			present[key] = true
			value, err := optionalPositiveInt(key, raw)
			if err != nil {
				return store.ModelOverride{}, nil, err
			}
			patch.ContextLength = value
		case "vision":
			present[key] = true
			value, err := optionalBool(key, raw)
			if err != nil {
				return store.ModelOverride{}, nil, err
			}
			patch.Vision = value
		case "thinking":
			present[key] = true
			value, err := optionalLevels(key, raw)
			if err != nil {
				return store.ModelOverride{}, nil, err
			}
			patch.Thinking = value
		case "multiplier":
			present[key] = true
			value, err := optionalMultiplier(key, raw)
			if err != nil {
				return store.ModelOverride{}, nil, err
			}
			patch.Multiplier = value
		case "free":
			present[key] = true
			value, err := optionalBool(key, raw)
			if err != nil {
				return store.ModelOverride{}, nil, err
			}
			patch.Free = value
		default:
			// Unknown keys are ignored so a newer UI may send a superset.
		}
	}
	return patch, present, nil
}

// applyModelOverridePatch copies the fields present in the body onto target.
func applyModelOverridePatch(target *store.ModelOverride, patch store.ModelOverride, fields map[string]bool) {
	if fields["display_name"] {
		target.DisplayName = patch.DisplayName
	}
	if fields["vendor"] {
		target.Vendor = patch.Vendor
	}
	if fields["context_length"] {
		target.ContextLength = patch.ContextLength
	}
	if fields["vision"] {
		target.Vision = patch.Vision
	}
	if fields["thinking"] {
		target.Thinking = patch.Thinking
	}
	if fields["multiplier"] {
		target.Multiplier = patch.Multiplier
	}
	if fields["free"] {
		target.Free = patch.Free
	}
}

// ---------------------------------------------------------------- values

// jsonIsNull reports an explicit JSON null, which clears the override.
func jsonIsNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

func optionalText(field string, raw json.RawMessage, limit int) (*string, error) {
	if jsonIsNull(raw) {
		return nil, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("%w: %s must be a string or null", ErrInvalidRequest, field)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	if utf8.RuneCountInString(value) > limit {
		return nil, fmt.Errorf("%w: %s must be at most %d characters", ErrInvalidRequest, field, limit)
	}
	return &value, nil
}

func optionalPositiveInt(field string, raw json.RawMessage) (*int, error) {
	if jsonIsNull(raw) {
		return nil, nil
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("%w: %s must be an integer or null", ErrInvalidRequest, field)
	}
	if value <= 0 || value > maxModelOverrideContext {
		return nil, fmt.Errorf("%w: %s must be an integer between 1 and %d",
			ErrInvalidRequest, field, maxModelOverrideContext)
	}
	return &value, nil
}

func optionalBool(field string, raw json.RawMessage) (*bool, error) {
	if jsonIsNull(raw) {
		return nil, nil
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("%w: %s must be true, false or null", ErrInvalidRequest, field)
	}
	return &value, nil
}

func optionalMultiplier(field string, raw json.RawMessage) (*float64, error) {
	if jsonIsNull(raw) {
		return nil, nil
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("%w: %s must be a number or null", ErrInvalidRequest, field)
	}
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > maxModelOverrideMultiplier {
		return nil, fmt.Errorf("%w: %s must be between 0 and %g",
			ErrInvalidRequest, field, maxModelOverrideMultiplier)
	}
	return &value, nil
}

func optionalLevels(field string, raw json.RawMessage) (*[]string, error) {
	if jsonIsNull(raw) {
		return nil, nil
	}
	var levels []string
	if err := json.Unmarshal(raw, &levels); err != nil {
		return nil, fmt.Errorf("%w: %s must be an array of strings or null", ErrInvalidRequest, field)
	}
	out := make([]string, 0, len(levels))
	seen := make(map[string]bool, len(levels))
	for _, level := range levels {
		level = strings.TrimSpace(level)
		if level == "" || seen[level] {
			continue
		}
		if utf8.RuneCountInString(level) > maxModelOverrideLevelRunes {
			return nil, fmt.Errorf("%w: %s entries must be at most %d characters",
				ErrInvalidRequest, field, maxModelOverrideLevelRunes)
		}
		seen[level] = true
		out = append(out, level)
	}
	if len(out) == 0 {
		return nil, nil
	}
	if len(out) > maxModelOverrideLevels {
		return nil, fmt.Errorf("%w: %s accepts at most %d tiers",
			ErrInvalidRequest, field, maxModelOverrideLevels)
	}
	return &out, nil
}
