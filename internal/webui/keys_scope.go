package webui

// Client key scopes and quotas (plan 2026-10-21 §1).
//
// Ported from A's sub-key fields (TraeWorkAssistant, MIT, 朱天伟;
// src-tauri/src/api_server/api_keys.rs:55-78,254-293 and the whitelist rejection
// in routes.rs:390-424): a key may list the models it may call, bind one
// resource pool and cap its own daily request count.
//
// Wire shape:
//
//	GET  /api/keys
//	     -> [ClientKey...] where model_scope is []string|null, bind_pool ""=any,
//	        daily_quota 0=unlimited, used_today = today's requests and
//	        quota_left = -1 when unlimited else max(0, quota-used_today).
//	POST /api/keys
//	     body: {"name":"...", "model_scope":["p/*","m"], "bind_pool":"p",
//	            "daily_quota":100}
//	     -> 201 {"ok":true,"secret":"agk_...","key":{...}}
//	PUT  /api/keys/{id}
//	     body: any subset of {"name","enabled","model_scope","bind_pool",
//	            "daily_quota"}; an absent field keeps the stored value,
//	            null/[] clears model_scope and "" clears bind_pool.
//	     -> 200 {"ok":true,"key":{...}}
//	DELETE /api/keys/{id} -> 200 {"ok":true,"id":"..."}
//
// The legacy Options.Keys / CreateKey / DeleteKey callbacks keep working: a POST
// without the scope fields on a host that only wired CreateKey behaves exactly
// as before.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"aiworkgate/internal/model"
	"aiworkgate/internal/store"
)

const (
	maxKeyNameRunes = 64
)

// KeySpec is one validated key definition (create or full replacement).
type KeySpec struct {
	Name       string
	Enabled    bool
	ModelScope []string
	BindPool   string
	DailyQuota int
}

// scope returns the stored triple of a spec.
func (s KeySpec) scope() store.ClientKeyScope {
	return store.ClientKeyScope{ModelScope: s.ModelScope, BindPool: s.BindPool, DailyQuota: s.DailyQuota}
}

// handleKeysAPI serves GET and POST /api/keys.
func (s *server) handleKeysAPI(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet, http.MethodPost) {
		return
	}
	if r.Method == http.MethodGet {
		s.listClientKeys(w)
		return
	}
	s.createClientKey(w, r)
}

// handleKeyAPI serves PUT and DELETE /api/keys/{id}.
func (s *server) handleKeyAPI(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPut, http.MethodDelete) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "key id is required", "bad_request")
		return
	}
	if r.Method == http.MethodDelete {
		s.deleteClientKey(w, id)
		return
	}
	s.updateClientKey(w, r, id)
}

// listClientKeys returns the host's list. The host fills model_scope,
// bind_pool, daily_quota, used_today and quota_left; a key object from an older
// host is normalised here so the UI always sees the five fields.
func (s *server) listClientKeys(w http.ResponseWriter) {
	if s.opts.Keys == nil {
		s.unavailable(w)
		return
	}
	keys := s.opts.Keys()
	if keys == nil {
		keys = []model.ClientKey{}
	}
	for i := range keys {
		if keys[i].ModelScope == nil {
			keys[i].ModelScope = []string{}
		}
		keys[i].QuotaLeft = store.ClientKeyQuotaLeft(keys[i].DailyQuota, keys[i].UsedToday)
	}
	writeJSON(w, http.StatusOK, keys)
}

// createClientKey validates the optional scope fields and mints the secret
// through the host.
func (s *server) createClientKey(w http.ResponseWriter, r *http.Request) {
	var body map[string]json.RawMessage
	if !decodeJSON(w, r, &body) {
		return
	}
	spec, err := s.parseKeyCreate(body)
	if err != nil {
		writeKeySpecError(w, err)
		return
	}

	if s.opts.CreateKeySpec != nil {
		secret, key, err := s.opts.CreateKeySpec(spec)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "create key: "+err.Error(), "internal_error")
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "secret": secret, "key": key})
		return
	}
	if s.opts.CreateKey == nil {
		s.unavailable(w)
		return
	}
	if !spec.scope().Empty() {
		// The legacy callback cannot store a scope; silently dropping it would
		// hand the user an unrestricted key that looks restricted.
		writeError(w, http.StatusNotImplemented,
			"this host does not support key scopes; clear model_scope, bind_pool and daily_quota",
			"not_implemented")
		return
	}
	secret, key, err := s.opts.CreateKey(spec.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create key: "+err.Error(), "internal_error")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "secret": secret, "key": key})
}

// updateClientKey applies a partial edit over the stored key.
func (s *server) updateClientKey(w http.ResponseWriter, r *http.Request, id string) {
	if s.opts.UpdateKey == nil || s.opts.Keys == nil {
		s.unavailable(w)
		return
	}
	current, ok := findKey(s.opts.Keys(), id)
	if !ok {
		writeError(w, http.StatusNotFound, "key not found: "+id, "not_found")
		return
	}

	var body map[string]json.RawMessage
	if !decodeJSON(w, r, &body) {
		return
	}
	spec := KeySpec{
		Name:       current.Name,
		Enabled:    current.Enabled,
		ModelScope: current.ModelScope,
		BindPool:   current.BindPool,
		DailyQuota: current.DailyQuota,
	}
	if err := applyKeyPatch(&spec, body); err != nil {
		writeKeySpecError(w, err)
		return
	}
	spec, err := s.normalizeKeySpec(spec)
	if err != nil {
		writeKeySpecError(w, err)
		return
	}

	updated, err := s.opts.UpdateKey(id, spec)
	if err != nil {
		if errors.Is(err, store.ErrInvalidArgument) || errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusBadRequest, err.Error(), "invalid_request")
			return
		}
		writeError(w, http.StatusInternalServerError, "update key: "+err.Error(), "internal_error")
		return
	}
	if updated.ID == "" {
		// A host callback may only persist; fill the echo from what we sent.
		updated = model.ClientKey{
			ID: id, Name: spec.Name, Prefix: current.Prefix, Enabled: spec.Enabled,
			CreatedAt: current.CreatedAt, LastUsedAt: current.LastUsedAt,
			ModelScope: spec.ModelScope, BindPool: spec.BindPool, DailyQuota: spec.DailyQuota,
			UsedToday: current.UsedToday,
		}
	}
	updated.QuotaLeft = store.ClientKeyQuotaLeft(updated.DailyQuota, updated.UsedToday)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": updated})
}

// deleteClientKey removes the key from the secret store and the database.
func (s *server) deleteClientKey(w http.ResponseWriter, id string) {
	if s.opts.DeleteKey == nil {
		s.unavailable(w)
		return
	}
	if err := s.opts.DeleteKey(id); err != nil {
		writeError(w, http.StatusInternalServerError, "delete key: "+err.Error(), "internal_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// findKey looks a key up in the list the host already returns.
func findKey(keys []model.ClientKey, id string) (model.ClientKey, bool) {
	for _, key := range keys {
		if key.ID == id {
			return key, true
		}
	}
	return model.ClientKey{}, false
}

// parseKeyCreate reads a create body. name is required; the three scope fields
// are optional and default to "unrestricted", enabled defaults to true.
func (s *server) parseKeyCreate(body map[string]json.RawMessage) (KeySpec, error) {
	spec := KeySpec{Enabled: true}
	if err := applyKeyPatch(&spec, body); err != nil {
		return KeySpec{}, err
	}
	return s.normalizeKeySpec(spec)
}

// applyKeyPatch copies the scope fields present in a JSON body onto spec. An
// unknown key is ignored so a newer UI may send a superset.
func applyKeyPatch(spec *KeySpec, body map[string]json.RawMessage) error {
	for key, raw := range body {
		switch key {
		case "model_scope":
			scope, err := keyScopeList(raw)
			if err != nil {
				return err
			}
			spec.ModelScope = scope
		case "bind_pool":
			pool, err := keyBindPool(raw)
			if err != nil {
				return err
			}
			spec.BindPool = pool
		case "daily_quota":
			quota, err := keyDailyQuota(raw)
			if err != nil {
				return err
			}
			spec.DailyQuota = quota
		case "name":
			var name string
			if err := json.Unmarshal(raw, &name); err != nil {
				return fmt.Errorf("%w: name must be a string", ErrInvalidRequest)
			}
			spec.Name = name
		case "enabled":
			var enabled bool
			if err := json.Unmarshal(raw, &enabled); err != nil {
				return fmt.Errorf("%w: enabled must be true or false", ErrInvalidRequest)
			}
			spec.Enabled = enabled
		}
	}
	return nil
}

// normalizeKeySpec trims, dedupes and bounds a spec, then checks bind_pool
// against the installed plugins.
func (s *server) normalizeKeySpec(spec KeySpec) (KeySpec, error) {
	spec.Name = strings.TrimSpace(spec.Name)
	if spec.Name == "" {
		return KeySpec{}, fmt.Errorf("%w: name is required", ErrInvalidRequest)
	}
	if utf8.RuneCountInString(spec.Name) > maxKeyNameRunes {
		return KeySpec{}, fmt.Errorf("%w: name must be at most %d characters", ErrInvalidRequest, maxKeyNameRunes)
	}

	scope := make([]string, 0, len(spec.ModelScope))
	seen := make(map[string]bool, len(spec.ModelScope))
	for _, entry := range spec.ModelScope {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			return KeySpec{}, fmt.Errorf("%w: model_scope entries must not be empty", ErrInvalidRequest)
		}
		if utf8.RuneCountInString(entry) > store.MaxClientKeyScopeRunes {
			return KeySpec{}, fmt.Errorf("%w: model_scope entries must be at most %d characters",
				ErrInvalidRequest, store.MaxClientKeyScopeRunes)
		}
		if seen[entry] {
			continue
		}
		seen[entry] = true
		scope = append(scope, entry)
	}
	if len(scope) > store.MaxClientKeyModelScope {
		return KeySpec{}, fmt.Errorf("%w: model_scope accepts at most %d entries",
			ErrInvalidRequest, store.MaxClientKeyModelScope)
	}
	if len(scope) == 0 {
		scope = nil
	}
	spec.ModelScope = scope

	spec.BindPool = strings.TrimSpace(spec.BindPool)
	if spec.BindPool != "" {
		ids, err := s.installedPluginIDs()
		if err != nil {
			return KeySpec{}, err
		}
		if !ids[spec.BindPool] {
			return KeySpec{}, fmt.Errorf("%w: bind_pool %q is not an installed plugin", ErrInvalidRequest, spec.BindPool)
		}
	}
	return spec, nil
}

// installedPluginIDs returns the ids a key may bind to. A host without the
// plugin list cannot validate the binding, so the request is refused instead of
// storing an unchecked id.
func (s *server) installedPluginIDs() (map[string]bool, error) {
	if s.opts.Plugins == nil {
		return nil, fmt.Errorf("%w: the host does not expose the installed plugin list", ErrInvalidRequest)
	}
	statuses := s.opts.Plugins()
	ids := make(map[string]bool, len(statuses))
	for _, status := range statuses {
		if id := strings.TrimSpace(status.Manifest.ID); id != "" {
			ids[id] = true
		}
	}
	return ids, nil
}

// writeKeySpecError maps a validation error to 400.
func writeKeySpecError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrInvalidRequest) {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request")
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error(), "internal_error")
}

// ---------------------------------------------------------------- values

// keyScopeList accepts an array of strings, null or [] (both clear the scope).
func keyScopeList(raw json.RawMessage) ([]string, error) {
	if jsonIsNull(raw) {
		return nil, nil
	}
	var entries []string
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("%w: model_scope must be an array of strings or null", ErrInvalidRequest)
	}
	return entries, nil
}

// keyBindPool accepts a plugin id or "" (null also clears it).
func keyBindPool(raw json.RawMessage) (string, error) {
	if jsonIsNull(raw) {
		return "", nil
	}
	var pool string
	if err := json.Unmarshal(raw, &pool); err != nil {
		return "", fmt.Errorf("%w: bind_pool must be a plugin id or empty", ErrInvalidRequest)
	}
	return strings.TrimSpace(pool), nil
}

// keyDailyQuota accepts 0 (unlimited) up to the store bound.
func keyDailyQuota(raw json.RawMessage) (int, error) {
	if jsonIsNull(raw) {
		return 0, nil
	}
	var quota int
	if err := json.Unmarshal(raw, &quota); err != nil {
		return 0, fmt.Errorf("%w: daily_quota must be an integer", ErrInvalidRequest)
	}
	if quota < 0 || quota > store.MaxClientKeyDailyQuota {
		return 0, fmt.Errorf("%w: daily_quota must be between 0 and %d",
			ErrInvalidRequest, store.MaxClientKeyDailyQuota)
	}
	return quota, nil
}
