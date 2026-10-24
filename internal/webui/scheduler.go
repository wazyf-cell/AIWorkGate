package webui

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"aiworkgate/internal/store"
)

// Scheduling admin API (§5.4, §5.5). The transport layer validates every
// editable field before the host callback runs, so a rejected request never
// partially applies.

// Errors the scheduler adapters wrap to map onto HTTP statuses.
var (
	// ErrUnknownModel reports an explain request for an unbound model.
	ErrUnknownModel = errors.New("unknown model")
	// ErrNoCandidate reports that all candidate accounts were filtered out.
	ErrNoCandidate = errors.New("no candidate account")
)

const (
	maxPoolPriority      = 1000
	maxSchedulerTTL      = 86400
	maxSchedulerConcur   = 32
	maxSchedulerItems    = 200
	maxPoolMembers       = 5000
	minHedgeMS           = 1000
	maxHedgeMS           = 8000
	maxLogicalModelRunes = 128
)

// poolStrategies is the accepted in-pool strategy set (§6.3).
var poolStrategies = map[string]bool{
	"credit_first": true, "remaining_first": true, "random": true,
	"weighted3": true, "p2c": true, "fixed": true,
}

// interPoolModes is the accepted inter-pool mode set (§6.2).
var interPoolModes = map[string]bool{"smart": true, "fixed": true}

// reasoningEfforts is the accepted default_reasoning set (§6.5).
var reasoningEfforts = map[string]bool{"": true, "off": true, "low": true, "medium": true, "high": true}

// schedulerPresets are the one-click combinations of §7.4.
var schedulerPresets = map[string]bool{
	"balanced": true, "freshness": true, "amortized": true, "latency": true, "fixed": true,
}

// SchedulerDefaultsPatch is the optional defaults object of PUT
// /api/scheduler/global. Pointers distinguish "omitted" from an explicit zero.
type SchedulerDefaultsPatch struct {
	Strategy            *string `json:"strategy"`
	AccountConcurrency  *int    `json:"account_concurrency"`
	PoolStickyTTL       *int    `json:"pool_sticky_ttl"`
	ConvStickyTTL       *int    `json:"conv_sticky_ttl"`
	HedgeMS             *int    `json:"hedge_ms"`
	DefaultReasoning    *string `json:"default_reasoning"`
	BackgroundDowngrade *bool   `json:"background_downgrade"`
	LongctxDowngrade    *bool   `json:"longctx_downgrade"`
}

// SchedulerGlobalUpdate is the body of PUT /api/scheduler/global.
type SchedulerGlobalUpdate struct {
	InterPool *string                 `json:"inter_pool"`
	Defaults  *SchedulerDefaultsPatch `json:"defaults"`
}

// SchedulerPoolUpdate is the body of PUT /api/scheduler/pools/{plugin}.
type SchedulerPoolUpdate struct {
	UpstreamEnabled     *bool   `json:"upstream_enabled"`
	Strategy            *string `json:"strategy"`
	PoolPriority        *int    `json:"pool_priority"`
	AccountConcurrency  *int    `json:"account_concurrency"`
	PoolStickyTTL       *int    `json:"pool_sticky_ttl"`
	ConvStickyTTL       *int    `json:"conv_sticky_ttl"`
	HedgeMS             *int    `json:"hedge_ms"`
	DefaultReasoning    *string `json:"default_reasoning"`
	BackgroundDowngrade *bool   `json:"background_downgrade"`
	LongctxDowngrade    *bool   `json:"longctx_downgrade"`
	ModelSyncTime       *string `json:"model_sync_time"`
}

// PoolSettingsUpdate is the body of PUT /api/pools/{plugin}.
type PoolSettingsUpdate struct {
	UpstreamEnabled     *bool   `json:"upstream_enabled"`
	AccountConcurrency  *int    `json:"account_concurrency"`
	PoolStickyTTL       *int    `json:"pool_sticky_ttl"`
	ConvStickyTTL       *int    `json:"conv_sticky_ttl"`
	HedgeMS             *int    `json:"hedge_ms"`
	DefaultReasoning    *string `json:"default_reasoning"`
	BackgroundDowngrade *bool   `json:"background_downgrade"`
	LongctxDowngrade    *bool   `json:"longctx_downgrade"`
	ToolExec            *bool   `json:"tool_exec"`
	ModelSyncTime       *string `json:"model_sync_time"`
}

// SchedulerModelSource is one physical source of a logical model.
type SchedulerModelSource struct {
	PluginID string `json:"plugin_id"`
	Model    string `json:"model"`
}

// SchedulerModelUpdate is the body of PUT /api/scheduler/models/{logical}.
type SchedulerModelUpdate struct {
	DisplayName string                 `json:"display_name"`
	Items       []SchedulerModelSource `json:"items"`
}

// validateStrategy validates an optional strategy field.
func validateStrategy(value *string, field string) error {
	if value == nil {
		return nil
	}
	strategy := strings.TrimSpace(*value)
	if strategy == "" || !poolStrategies[strategy] {
		return fmt.Errorf("%s must be one of credit_first, remaining_first, random, weighted3, p2c, fixed", field)
	}
	return nil
}

// validateTTL validates one 0–86400 sticky TTL.
func validateTTL(value *int, field string) error {
	if value == nil {
		return nil
	}
	if *value < 0 || *value > maxSchedulerTTL {
		return fmt.Errorf("%s must be between 0 and %d seconds", field, maxSchedulerTTL)
	}
	return nil
}

// validateConcurrency validates 0–32 (0 = unlimited).
func validateConcurrency(value *int) error {
	if value == nil {
		return nil
	}
	if *value < 0 || *value > maxSchedulerConcur {
		return fmt.Errorf("account_concurrency must be between 0 and %d", maxSchedulerConcur)
	}
	return nil
}

// validateHedge validates the 0-or-1000–8000 hedge window.
func validateHedge(value *int) error {
	if value == nil || *value == 0 {
		return nil
	}
	if *value < minHedgeMS || *value > maxHedgeMS {
		return fmt.Errorf("hedge_ms must be 0 or between %d and %d", minHedgeMS, maxHedgeMS)
	}
	return nil
}

// validateReasoning validates default_reasoning.
func validateReasoning(value *string, field string) error {
	if value == nil {
		return nil
	}
	if !reasoningEfforts[strings.ToLower(strings.TrimSpace(*value))] {
		return fmt.Errorf("%s must be empty, off, low, medium or high", field)
	}
	return nil
}

// validateSyncTime validates the optional HH:MM model sync time.
func validateSyncTime(value *string) error {
	if value == nil {
		return nil
	}
	raw := strings.TrimSpace(*value)
	if raw == "" {
		return nil
	}
	if _, err := time.Parse("15:04", raw); err != nil {
		return fmt.Errorf("model_sync_time must be HH:MM")
	}
	return nil
}

func validateSchedulerDefaults(patch *SchedulerDefaultsPatch) error {
	if patch == nil {
		return nil
	}
	if err := validateStrategy(patch.Strategy, "defaults.strategy"); err != nil {
		return err
	}
	if err := validateConcurrency(patch.AccountConcurrency); err != nil {
		return err
	}
	if err := validateTTL(patch.PoolStickyTTL, "defaults.pool_sticky_ttl"); err != nil {
		return err
	}
	if err := validateTTL(patch.ConvStickyTTL, "defaults.conv_sticky_ttl"); err != nil {
		return err
	}
	if err := validateHedge(patch.HedgeMS); err != nil {
		return err
	}
	return validateReasoning(patch.DefaultReasoning, "defaults.default_reasoning")
}

func validateSchedulerPoolUpdate(update SchedulerPoolUpdate) error {
	if err := validateStrategy(update.Strategy, "strategy"); err != nil {
		return err
	}
	if err := validateConcurrency(update.AccountConcurrency); err != nil {
		return err
	}
	if err := validateTTL(update.PoolStickyTTL, "pool_sticky_ttl"); err != nil {
		return err
	}
	if err := validateTTL(update.ConvStickyTTL, "conv_sticky_ttl"); err != nil {
		return err
	}
	if err := validateHedge(update.HedgeMS); err != nil {
		return err
	}
	if err := validateReasoning(update.DefaultReasoning, "default_reasoning"); err != nil {
		return err
	}
	if update.PoolPriority != nil && (*update.PoolPriority < 0 || *update.PoolPriority > maxPoolPriority) {
		return fmt.Errorf("pool_priority must be between 0 and %d", maxPoolPriority)
	}
	if update.ModelSyncTime != nil {
		if _, err := time.Parse("15:04", strings.TrimSpace(*update.ModelSyncTime)); strings.TrimSpace(*update.ModelSyncTime) != "" && err != nil {
			return fmt.Errorf("model_sync_time must be HH:MM")
		}
	}
	return nil
}

func validatePoolSettingsUpdate(update PoolSettingsUpdate) error {
	if err := validateConcurrency(update.AccountConcurrency); err != nil {
		return err
	}
	if err := validateTTL(update.PoolStickyTTL, "pool_sticky_ttl"); err != nil {
		return err
	}
	if err := validateTTL(update.ConvStickyTTL, "conv_sticky_ttl"); err != nil {
		return err
	}
	if err := validateHedge(update.HedgeMS); err != nil {
		return err
	}
	if err := validateReasoning(update.DefaultReasoning, "default_reasoning"); err != nil {
		return err
	}
	return validateSyncTime(update.ModelSyncTime)
}

// validateModelUpdate validates one logical-model binding. Empty items means
// "delete the binding" and is accepted by PUT as well.
func validateModelUpdate(update SchedulerModelUpdate) error {
	if utf8.RuneCountInString(strings.TrimSpace(update.DisplayName)) > maxLogicalModelRunes {
		return fmt.Errorf("display_name must be at most %d characters", maxLogicalModelRunes)
	}
	if len(update.Items) > maxSchedulerItems {
		return fmt.Errorf("at most %d sources per model", maxSchedulerItems)
	}
	seen := make(map[string]bool, len(update.Items))
	for _, item := range update.Items {
		pluginID := strings.TrimSpace(item.PluginID)
		modelID := strings.TrimSpace(item.Model)
		if pluginID == "" || modelID == "" {
			return errors.New("every source needs plugin_id and model")
		}
		key := pluginID + "\x00" + modelID
		if seen[key] {
			return fmt.Errorf("duplicate source %s/%s", pluginID, modelID)
		}
		seen[key] = true
	}
	return nil
}

// writeSchedulerError maps scheduler adapter errors onto HTTP statuses.
func writeSchedulerError(w http.ResponseWriter, action string, err error) {
	switch {
	case errors.Is(err, ErrInvalidRequest):
		writeError(w, http.StatusBadRequest, action+": "+err.Error(), "invalid_request")
	case errors.Is(err, ErrUnknownModel):
		writeError(w, http.StatusBadRequest, action+": "+err.Error(), "unknown_model")
	case errors.Is(err, ErrNoCandidate):
		writeError(w, http.StatusUnprocessableEntity, action+": "+err.Error(), "no_candidate")
	case errors.Is(err, ErrPluginNotFound):
		writeError(w, http.StatusNotFound, action+": "+err.Error(), "not_found")
	default:
		writeError(w, http.StatusInternalServerError, action+": "+err.Error(), "internal_error")
	}
}

// ---------------------------------------------------------------- scheduler

func (s *server) handleScheduler(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if s.opts.SchedulerOverview == nil {
		s.unavailable(w)
		return
	}
	payload, err := s.opts.SchedulerOverview()
	if err != nil {
		writeSchedulerError(w, "scheduler overview", err)
		return
	}
	if payload == nil {
		payload = map[string]any{}
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *server) handleSchedulerGlobal(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPut) {
		return
	}
	if s.opts.SaveSchedulerGlobal == nil {
		s.unavailable(w)
		return
	}
	var body SchedulerGlobalUpdate
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.InterPool == nil && body.Defaults == nil {
		writeError(w, http.StatusBadRequest, "inter_pool or defaults is required", "bad_request")
		return
	}
	if body.InterPool != nil {
		mode := strings.ToLower(strings.TrimSpace(*body.InterPool))
		if !interPoolModes[mode] {
			writeError(w, http.StatusBadRequest, "inter_pool must be smart or fixed", "invalid_inter_pool")
			return
		}
		body.InterPool = &mode
	}
	if err := validateSchedulerDefaults(body.Defaults); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_scheduler_defaults")
		return
	}
	payload, err := s.opts.SaveSchedulerGlobal(body)
	if err != nil {
		writeSchedulerError(w, "save scheduler global", err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *server) handleSchedulerPool(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPut) {
		return
	}
	if s.opts.SaveSchedulerPool == nil {
		s.unavailable(w)
		return
	}
	pluginID := strings.TrimSpace(r.PathValue("plugin"))
	if pluginID == "" {
		writeError(w, http.StatusBadRequest, "plugin id is required", "bad_request")
		return
	}
	var body SchedulerPoolUpdate
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := validateSchedulerPoolUpdate(body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_pool_settings")
		return
	}
	payload, err := s.opts.SaveSchedulerPool(pluginID, body)
	if err != nil {
		writeSchedulerError(w, "save pool", err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *server) handleSchedulerPreset(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	if s.opts.ApplySchedulerPreset == nil {
		s.unavailable(w)
		return
	}
	name := strings.ToLower(strings.TrimSpace(r.PathValue("name")))
	if !schedulerPresets[name] {
		writeError(w, http.StatusBadRequest,
			"preset must be balanced, freshness, amortized, latency or fixed", "invalid_preset")
		return
	}
	payload, err := s.opts.ApplySchedulerPreset(name)
	if err != nil {
		writeSchedulerError(w, "apply preset", err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *server) handleSchedulerModels(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if s.opts.SchedulerModels == nil {
		s.unavailable(w)
		return
	}
	payload, err := s.opts.SchedulerModels()
	if err != nil {
		writeSchedulerError(w, "list models", err)
		return
	}
	if payload == nil {
		payload = map[string]any{"models": []any{}}
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *server) handleSchedulerModel(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPut, http.MethodDelete) {
		return
	}
	if r.Method == http.MethodDelete {
		if s.opts.DeleteSchedulerModel == nil {
			s.unavailable(w)
			return
		}
	} else if s.opts.SaveSchedulerModel == nil {
		s.unavailable(w)
		return
	}
	logical := strings.TrimSpace(r.PathValue("logical"))
	if logical == "" {
		writeError(w, http.StatusBadRequest, "logical model is required", "bad_request")
		return
	}
	if utf8.RuneCountInString(logical) > maxLogicalModelRunes {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("logical model must be at most %d characters", maxLogicalModelRunes), "bad_request")
		return
	}
	if r.Method == http.MethodDelete {
		if err := s.opts.DeleteSchedulerModel(logical); err != nil {
			writeSchedulerError(w, "delete model", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}

	var body SchedulerModelUpdate
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := validateModelUpdate(body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_model_binding")
		return
	}
	payload, err := s.opts.SaveSchedulerModel(logical, body)
	if err != nil {
		writeSchedulerError(w, "save model", err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *server) handleSchedulerExplain(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	if s.opts.SchedulerExplain == nil {
		s.unavailable(w)
		return
	}
	var body struct {
		Model          string `json:"model"`
		ConversationID string `json:"conversation_id"`
		ClientKey      string `json:"client_key"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	modelID := strings.TrimSpace(body.Model)
	if modelID == "" {
		writeError(w, http.StatusBadRequest, "model is required", "bad_request")
		return
	}
	payload, err := s.opts.SchedulerExplain(modelID,
		strings.TrimSpace(body.ConversationID), strings.TrimSpace(body.ClientKey))
	if err != nil {
		writeSchedulerError(w, "explain", err)
		return
	}
	if payload == nil {
		payload = map[string]any{}
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *server) handleSchedulerSticky(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet, http.MethodDelete) {
		return
	}
	if r.Method == http.MethodGet {
		if s.opts.SchedulerSticky == nil {
			s.unavailable(w)
			return
		}
		payload, err := s.opts.SchedulerSticky()
		if err != nil {
			writeSchedulerError(w, "list sticky", err)
			return
		}
		if payload == nil {
			payload = map[string]any{"bindings": []any{}, "count": 0}
		}
		writeJSON(w, http.StatusOK, payload)
		return
	}

	if s.opts.DeleteSchedulerSticky == nil {
		s.unavailable(w)
		return
	}
	query := r.URL.Query()
	scope := strings.ToLower(strings.TrimSpace(query.Get("scope")))
	key := strings.TrimSpace(query.Get("key"))
	if scope != "" && scope != "conv" && scope != "pool" {
		writeError(w, http.StatusBadRequest, `scope must be "conv" or "pool"`, "bad_request")
		return
	}
	if key != "" && scope == "" {
		writeError(w, http.StatusBadRequest, "scope is required when key is given", "bad_request")
		return
	}
	removed, err := s.opts.DeleteSchedulerSticky(scope, key)
	if err != nil {
		writeSchedulerError(w, "delete sticky", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": removed})
}

// ---------------------------------------------------------------- pools

func (s *server) handlePool(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet, http.MethodPut) {
		return
	}
	pluginID := strings.TrimSpace(r.PathValue("plugin"))
	if pluginID == "" {
		writeError(w, http.StatusBadRequest, "plugin id is required", "bad_request")
		return
	}
	if r.Method == http.MethodGet {
		if s.opts.PoolDetail == nil {
			s.unavailable(w)
			return
		}
		payload, err := s.opts.PoolDetail(pluginID)
		if err != nil {
			writeSchedulerError(w, "pool detail", err)
			return
		}
		if payload == nil {
			payload = map[string]any{}
		}
		writeJSON(w, http.StatusOK, payload)
		return
	}

	if s.opts.SavePoolSettings == nil {
		s.unavailable(w)
		return
	}
	var body PoolSettingsUpdate
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := validatePoolSettingsUpdate(body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_pool_settings")
		return
	}
	payload, err := s.opts.SavePoolSettings(pluginID, body)
	if err != nil {
		writeSchedulerError(w, "save pool", err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *server) handlePoolMembers(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPut) {
		return
	}
	if s.opts.SavePoolMembers == nil {
		s.unavailable(w)
		return
	}
	pluginID := strings.TrimSpace(r.PathValue("plugin"))
	if pluginID == "" {
		writeError(w, http.StatusBadRequest, "plugin id is required", "bad_request")
		return
	}
	var body struct {
		Items []string `json:"items"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if len(body.Items) > maxPoolMembers {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("at most %d members per pool", maxPoolMembers), "bad_request")
		return
	}
	seen := make(map[string]bool, len(body.Items))
	items := make([]string, 0, len(body.Items))
	for _, item := range body.Items {
		accountID := strings.TrimSpace(item)
		if accountID == "" {
			writeError(w, http.StatusBadRequest, "every item must be a non-empty account id", "bad_request")
			return
		}
		if seen[accountID] {
			continue
		}
		seen[accountID] = true
		items = append(items, accountID)
	}
	payload, err := s.opts.SavePoolMembers(pluginID, items)
	if err != nil {
		writeSchedulerError(w, "save pool members", err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *server) handlePoolSync(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	if s.opts.SyncPoolCatalog == nil {
		s.unavailable(w)
		return
	}
	pluginID := strings.TrimSpace(r.PathValue("plugin"))
	if pluginID == "" {
		writeError(w, http.StatusBadRequest, "plugin id is required", "bad_request")
		return
	}
	payload, err := s.opts.SyncPoolCatalog(pluginID)
	if err != nil {
		writeSchedulerError(w, "sync catalog", err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *server) handlePoolCatalog(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if s.opts.PoolCatalog == nil {
		s.unavailable(w)
		return
	}
	pluginID := strings.TrimSpace(r.PathValue("plugin"))
	if pluginID == "" {
		writeError(w, http.StatusBadRequest, "plugin id is required", "bad_request")
		return
	}
	payload, err := s.opts.PoolCatalog(pluginID)
	if err != nil {
		writeSchedulerError(w, "pool catalog", err)
		return
	}
	if payload == nil {
		payload = map[string]any{"catalog": []any{}}
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *server) handlePoolModelEnabled(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPut) {
		return
	}
	if s.opts.SetPoolModelEnabled == nil {
		s.unavailable(w)
		return
	}
	pluginID := strings.TrimSpace(r.PathValue("plugin"))
	modelID := strings.TrimSpace(r.PathValue("model"))
	if pluginID == "" || modelID == "" {
		writeError(w, http.StatusBadRequest, "plugin and model are required", "bad_request")
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Enabled == nil {
		writeError(w, http.StatusBadRequest, "enabled is required", "bad_request")
		return
	}
	payload, err := s.opts.SetPoolModelEnabled(pluginID, modelID, *body.Enabled)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "model not found in catalog", "model_not_found")
			return
		}
		writeSchedulerError(w, "set model enabled", err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}
