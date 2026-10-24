package webui

// Admin API for user-defined custom models (Settings → 自定义模型).
//
// Ported from A (TraeWorkAssistant, MIT, 朱天伟) custom_models.rs:118-181:
// the upsert validates name/base_url, keeps the canonical name unique and
// reports a conflict instead of silently overwriting another route. The API
// key is write-only here as well: the host stores it in the DPAPI secret
// store and the list only reports key_set.
//
// Wire shape:
//
//	GET    /api/custom-models
//	       -> {"models":[{"id","name","base_url","upstream_model","enabled",
//	                       "key_set","created_at","updated_at"}]}
//	POST   /api/custom-models {name,base_url,upstream_model,api_key,enabled?}
//	       -> 201 {"ok":true,"model":{...}}; 409 conflict kind on a duplicate
//	          canonical name; 400 invalid_request on any invalid field.
//	PUT    /api/custom-models/{id} any subset of
//	       {name,base_url,upstream_model,enabled,api_key}
//	       -> 200 {"ok":true,"model":{...}}; an absent api_key keeps the
//	          stored key, "" clears it.
//	DELETE /api/custom-models/{id}
//	       -> 200 {"ok":true,"id":"..."}; the host also clears the stored key.
//	POST   /api/custom-models/{id}/test
//	       -> 200 {"ok":true,"latency_ms":N} or 502
//	          {"ok":false,"latency_ms":N,"error":"<redacted reason>"}.
//
// The test call is only ever made by this explicit endpoint; saving a model
// never probes the upstream.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"aiworkgate/internal/store"
)

// CustomModelInput is one create payload. ConnectionID selects the owning
// connection; BaseURL/APIKey remain accepted so an older client that does not
// know about connections still works (the host merges them into one).
type CustomModelInput struct {
	ConnectionID     string
	Name             string
	BaseURL          string
	UpstreamModel    string
	APIKey           string
	Enabled          bool
	ContextWindow    int
	MaxOutputTokens  int
	InputTypes       []string
	ReasoningTiers   []string
	DefaultReasoning string
}

// CustomModelPatch is a partial edit. A nil field keeps the stored value. The
// connection and the upstream model are immutable once created; api_key is
// kept for the legacy no-connection path.
type CustomModelPatch struct {
	Name             *string
	BaseURL          *string
	UpstreamModel    *string
	Enabled          *bool
	APIKey           *string
	ContextWindow    *int
	MaxOutputTokens  *int
	InputTypes       *[]string
	ReasoningTiers   *[]string
	DefaultReasoning *string
}

// handleCustomModels serves GET and POST /api/custom-models.
func (s *server) handleCustomModels(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet, http.MethodPost) {
		return
	}
	if r.Method == http.MethodGet {
		s.listCustomModels(w)
		return
	}
	s.createCustomModel(w, r)
}

// handleCustomModel serves PUT and DELETE /api/custom-models/{id}.
func (s *server) handleCustomModel(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPut, http.MethodDelete) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "模型 id 不能为空", "bad_request")
		return
	}
	if r.Method == http.MethodDelete {
		s.deleteCustomModel(w, id)
		return
	}
	s.updateCustomModel(w, r, id)
}

// handleCustomModelTest serves POST /api/custom-models/{id}/test.
func (s *server) handleCustomModelTest(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	if s.opts.TestCustomModel == nil {
		s.unavailable(w)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "模型 id 不能为空", "bad_request")
		return
	}
	latency, err := s.opts.TestCustomModel(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "自定义模型不存在", "not_found")
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"ok": false, "latency_ms": latency, "error": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "latency_ms": latency})
}

func (s *server) listCustomModels(w http.ResponseWriter) {
	if s.opts.CustomModels == nil {
		s.unavailable(w)
		return
	}
	models, err := s.opts.CustomModels()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取自定义模型失败: "+err.Error(), "internal_error")
		return
	}
	if models == nil {
		models = []store.CustomModel{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

func (s *server) createCustomModel(w http.ResponseWriter, r *http.Request) {
	if s.opts.CreateCustomModel == nil {
		s.unavailable(w)
		return
	}
	var body map[string]json.RawMessage
	if !decodeJSON(w, r, &body) {
		return
	}
	input := CustomModelInput{Enabled: true}
	if err := applyCustomModelInput(&input, body); err != nil {
		writeCustomModelError(w, err)
		return
	}
	if err := validateCustomModelInput(input, true); err != nil {
		writeCustomModelError(w, err)
		return
	}
	model, err := s.opts.CreateCustomModel(input)
	if err != nil {
		writeCustomModelError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "model": model})
}

func (s *server) updateCustomModel(w http.ResponseWriter, r *http.Request, id string) {
	if s.opts.UpdateCustomModel == nil {
		s.unavailable(w)
		return
	}
	var body map[string]json.RawMessage
	if !decodeJSON(w, r, &body) {
		return
	}
	patch, err := parseCustomModelPatch(body)
	if err != nil {
		writeCustomModelError(w, err)
		return
	}
	if err := validateCustomModelPatch(patch); err != nil {
		writeCustomModelError(w, err)
		return
	}
	model, err := s.opts.UpdateCustomModel(id, patch)
	if err != nil {
		writeCustomModelError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "model": model})
}

func (s *server) deleteCustomModel(w http.ResponseWriter, id string) {
	if s.opts.DeleteCustomModel == nil {
		s.unavailable(w)
		return
	}
	removed, err := s.opts.DeleteCustomModel(id)
	if err != nil {
		writeCustomModelError(w, err)
		return
	}
	if !removed {
		writeError(w, http.StatusNotFound, "自定义模型不存在", "not_found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// applyCustomModelInput copies the create body onto input. Unknown fields are
// ignored so a newer UI may send a superset.
func applyCustomModelInput(input *CustomModelInput, body map[string]json.RawMessage) error {
	for field, raw := range body {
		switch field {
		case "name":
			if err := json.Unmarshal(raw, &input.Name); err != nil {
				return fmt.Errorf("%w: 模型名称必须是字符串", ErrInvalidRequest)
			}
		case "base_url":
			if err := json.Unmarshal(raw, &input.BaseURL); err != nil {
				return fmt.Errorf("%w: API 地址必须是字符串", ErrInvalidRequest)
			}
		case "upstream_model":
			if err := json.Unmarshal(raw, &input.UpstreamModel); err != nil {
				return fmt.Errorf("%w: 上游模型名必须是字符串", ErrInvalidRequest)
			}
		case "api_key":
			if err := json.Unmarshal(raw, &input.APIKey); err != nil {
				return fmt.Errorf("%w: API Key 必须是字符串", ErrInvalidRequest)
			}
		case "enabled":
			if err := json.Unmarshal(raw, &input.Enabled); err != nil {
				return fmt.Errorf("%w: enabled 必须是布尔值", ErrInvalidRequest)
			}
		case "connection_id":
			if err := json.Unmarshal(raw, &input.ConnectionID); err != nil {
				return fmt.Errorf("%w: connection_id 必须是字符串", ErrInvalidRequest)
			}
		case "context_window":
			if err := json.Unmarshal(raw, &input.ContextWindow); err != nil {
				return fmt.Errorf("%w: 上下文长度必须是整数", ErrInvalidRequest)
			}
		case "max_output_tokens":
			if err := json.Unmarshal(raw, &input.MaxOutputTokens); err != nil {
				return fmt.Errorf("%w: 最大输出 token 必须是整数", ErrInvalidRequest)
			}
		case "input_types":
			if err := json.Unmarshal(raw, &input.InputTypes); err != nil {
				return fmt.Errorf("%w: 输入类型必须是字符串数组", ErrInvalidRequest)
			}
		case "reasoning_tiers":
			if err := json.Unmarshal(raw, &input.ReasoningTiers); err != nil {
				return fmt.Errorf("%w: 推理档位必须是字符串数组", ErrInvalidRequest)
			}
		case "default_reasoning":
			if err := json.Unmarshal(raw, &input.DefaultReasoning); err != nil {
				return fmt.Errorf("%w: 默认推理档必须是字符串", ErrInvalidRequest)
			}
		}
	}
	return nil
}

// parseCustomModelPatch copies the present fields of an edit body.
func parseCustomModelPatch(body map[string]json.RawMessage) (CustomModelPatch, error) {
	var patch CustomModelPatch
	for field, raw := range body {
		switch field {
		case "name":
			value, err := customModelString(raw, "模型名称")
			if err != nil {
				return patch, err
			}
			patch.Name = &value
		case "base_url":
			value, err := customModelString(raw, "API 地址")
			if err != nil {
				return patch, err
			}
			patch.BaseURL = &value
		case "upstream_model":
			value, err := customModelString(raw, "上游模型名")
			if err != nil {
				return patch, err
			}
			patch.UpstreamModel = &value
		case "api_key":
			value, err := customModelString(raw, "API Key")
			if err != nil {
				return patch, err
			}
			patch.APIKey = &value
		case "enabled":
			var value bool
			if err := json.Unmarshal(raw, &value); err != nil {
				return patch, fmt.Errorf("%w: enabled 必须是布尔值", ErrInvalidRequest)
			}
			patch.Enabled = &value
		case "context_window":
			value, err := customModelInt(raw, "上下文长度")
			if err != nil {
				return patch, err
			}
			patch.ContextWindow = &value
		case "max_output_tokens":
			value, err := customModelInt(raw, "最大输出 token")
			if err != nil {
				return patch, err
			}
			patch.MaxOutputTokens = &value
		case "input_types":
			value, err := customModelStringSlice(raw, "输入类型")
			if err != nil {
				return patch, err
			}
			patch.InputTypes = &value
		case "reasoning_tiers":
			value, err := customModelStringSlice(raw, "推理档位")
			if err != nil {
				return patch, err
			}
			patch.ReasoningTiers = &value
		case "default_reasoning":
			value, err := customModelString(raw, "默认推理档")
			if err != nil {
				return patch, err
			}
			patch.DefaultReasoning = &value
		}
	}
	return patch, nil
}

func customModelInt(raw json.RawMessage, label string) (int, error) {
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, fmt.Errorf("%w: %s 必须是整数", ErrInvalidRequest, label)
	}
	return value, nil
}

func customModelStringSlice(raw json.RawMessage, label string) ([]string, error) {
	var value []string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("%w: %s 必须是字符串数组", ErrInvalidRequest, label)
	}
	return value, nil
}

func customModelString(raw json.RawMessage, label string) (string, error) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("%w: %s 必须是字符串", ErrInvalidRequest, label)
	}
	return value, nil
}

// validateCustomModelInput enforces the create rules. A model that names its
// connection inherits the connection's base URL and key; the legacy no-
// connection payload still has to carry both. requireKey is true on create.
func validateCustomModelInput(input CustomModelInput, requireKey bool) error {
	if err := store.ValidateCustomModelName(input.Name); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidRequest, customModelFieldMessage(err))
	}
	if strings.TrimSpace(input.ConnectionID) == "" {
		if err := validateCustomModelFields(input.Name, input.BaseURL, input.UpstreamModel); err != nil {
			return err
		}
		if requireKey && strings.TrimSpace(input.APIKey) == "" {
			return fmt.Errorf("%w: API Key 不能为空", ErrInvalidRequest)
		}
		return validateCustomModelLimits(input.ContextWindow, input.MaxOutputTokens)
	}
	if strings.TrimSpace(input.UpstreamModel) == "" {
		return fmt.Errorf("%w: 上游模型名不能为空", ErrInvalidRequest)
	}
	if strings.TrimSpace(input.BaseURL) != "" {
		if err := store.ValidateCustomModelBaseURL(input.BaseURL); err != nil {
			return fmt.Errorf("%w: %s", ErrInvalidRequest, customModelFieldMessage(err))
		}
	}
	return validateCustomModelLimits(input.ContextWindow, input.MaxOutputTokens)
}

func validateCustomModelLimits(contextWindow, maxOutput int) error {
	if contextWindow < 0 || contextWindow > store.MaxCustomModelTokens {
		return fmt.Errorf("%w: 上下文长度必须在 0 到 %d 之间", ErrInvalidRequest, store.MaxCustomModelTokens)
	}
	if maxOutput < 0 || maxOutput > store.MaxCustomModelTokens {
		return fmt.Errorf("%w: 最大输出 token 必须在 0 到 %d 之间", ErrInvalidRequest, store.MaxCustomModelTokens)
	}
	return nil
}

func validateCustomModelPatch(patch CustomModelPatch) error {
	var name, baseURL, upstream string
	if patch.Name != nil {
		name = *patch.Name
	}
	if patch.BaseURL != nil {
		baseURL = *patch.BaseURL
	}
	if patch.UpstreamModel != nil {
		upstream = *patch.UpstreamModel
	}
	if patch.Name == nil && patch.BaseURL == nil && patch.UpstreamModel == nil {
		return nil
	}
	// Validate only the provided fields; the host fills the others from the
	// stored row and re-validates the merged entry in the store.
	if patch.Name != nil {
		if err := store.ValidateCustomModelName(name); err != nil {
			return fmt.Errorf("%w: %s", ErrInvalidRequest, customModelFieldMessage(err))
		}
	}
	if patch.BaseURL != nil {
		if err := store.ValidateCustomModelBaseURL(baseURL); err != nil {
			return fmt.Errorf("%w: %s", ErrInvalidRequest, customModelFieldMessage(err))
		}
	}
	if patch.UpstreamModel != nil && strings.TrimSpace(upstream) == "" {
		return fmt.Errorf("%w: 上游模型名不能为空", ErrInvalidRequest)
	}
	if patch.ContextWindow != nil && (*patch.ContextWindow < 0 || *patch.ContextWindow > store.MaxCustomModelTokens) {
		return fmt.Errorf("%w: 上下文长度必须在 0 到 %d 之间", ErrInvalidRequest, store.MaxCustomModelTokens)
	}
	if patch.MaxOutputTokens != nil && (*patch.MaxOutputTokens < 0 || *patch.MaxOutputTokens > store.MaxCustomModelTokens) {
		return fmt.Errorf("%w: 最大输出 token 必须在 0 到 %d 之间", ErrInvalidRequest, store.MaxCustomModelTokens)
	}
	return nil
}

// validateCustomModelFields validates the create fields with the store rules
// so both layers reject the same input. The store message is trimmed of its
// internal "store: invalid argument:" prefix for the UI.
func validateCustomModelFields(name, baseURL, upstream string) error {
	if err := store.ValidateCustomModelName(name); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidRequest, customModelFieldMessage(err))
	}
	if err := store.ValidateCustomModelBaseURL(baseURL); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidRequest, customModelFieldMessage(err))
	}
	if strings.TrimSpace(upstream) == "" {
		return fmt.Errorf("%w: 上游模型名不能为空", ErrInvalidRequest)
	}
	if len([]rune(strings.TrimSpace(upstream))) > store.MaxCustomModelUpstreamRunes {
		return fmt.Errorf("%w: 上游模型名最多 %d 个字符", ErrInvalidRequest, store.MaxCustomModelUpstreamRunes)
	}
	return nil
}

// customModelFieldMessage strips the store prefix, leaving the Chinese reason.
func customModelFieldMessage(err error) string {
	msg := err.Error()
	const marker = "invalid argument: "
	if idx := strings.LastIndex(msg, marker); idx >= 0 {
		return msg[idx+len(marker):]
	}
	return msg
}

// writeCustomModelError maps store and validation errors to the admin API.
func writeCustomModelError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrCustomModelConflict):
		writeError(w, http.StatusConflict, "模型名称已被占用（不区分大小写），请换一个名称", "conflict")
	case errors.Is(err, ErrInvalidRequest):
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request")
	case errors.Is(err, store.ErrInvalidArgument):
		writeError(w, http.StatusBadRequest, customModelFieldMessage(err), "invalid_request")
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "自定义模型不存在", "not_found")
	default:
		writeError(w, http.StatusInternalServerError, "自定义模型操作失败: "+err.Error(), "internal_error")
	}
}
