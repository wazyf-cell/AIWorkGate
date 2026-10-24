package webui

// Admin API for custom upstream connections (Settings → 自定义模型 → 接入).
// A connection is one (endpoint, API key, protocol) pair that owns many models.
//
//	GET    /api/custom-connections
//	POST   /api/custom-connections {label,protocol,base_url,api_key,enabled?}
//	PUT    /api/custom-connections/{id}
//	DELETE /api/custom-connections/{id}
//	POST   /api/custom-connections/{id}/test          -> model count + latency
//	GET    /api/custom-connections/{id}/remote-models -> upstream model list
//	POST   /api/custom-connections/{id}/models/import -> batch create
//
// Keys are write-only: responses only carry key_set.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"aiworkgate/internal/store"
)

// ErrCustomConnectionExists reports a duplicate endpoint+key pair.
var ErrCustomConnectionExists = errors.New("webui: custom connection already exists")

type CustomConnectionInput struct {
	Label    string
	Protocol string
	BaseURL  string
	APIKey   string
	Enabled  bool
}

type CustomConnectionPatch struct {
	Label    *string
	Protocol *string
	BaseURL  *string
	APIKey   *string
	Enabled  *bool
}

// RemoteModel is one entry returned by the upstream model-list endpoint.
type RemoteModel struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	ContextWindow   int    `json:"context_window,omitempty"`
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
	OwnedBy         string `json:"owned_by,omitempty"`
}

// ImportModelInput is one selected upstream model in a batch import.
type ImportModelInput struct {
	UpstreamModel    string   `json:"upstream_model"`
	Name             string   `json:"name"`
	ContextWindow    int      `json:"context_window"`
	MaxOutputTokens  int      `json:"max_output_tokens"`
	InputTypes       []string `json:"input_types"`
	ReasoningTiers   []string `json:"reasoning_tiers"`
	DefaultReasoning string   `json:"default_reasoning"`
}

type ImportSkip struct {
	UpstreamModel string `json:"upstream_model"`
	Reason        string `json:"reason"`
}

type ImportModelsResult struct {
	Created []store.CustomModel `json:"created"`
	Skipped []ImportSkip        `json:"skipped"`
}

func (s *server) handleCustomConnections(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet, http.MethodPost) {
		return
	}
	if r.Method == http.MethodGet {
		if s.opts.CustomConnections == nil {
			s.unavailable(w)
			return
		}
		rows, err := s.opts.CustomConnections()
		if err != nil {
			writeCustomModelError(w, err)
			return
		}
		if rows == nil {
			rows = []store.CustomConnection{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"connections": rows})
		return
	}
	if s.opts.CreateCustomConnection == nil {
		s.unavailable(w)
		return
	}
	var body map[string]json.RawMessage
	if !decodeJSON(w, r, &body) {
		return
	}
	input := CustomConnectionInput{Enabled: true}
	if err := applyConnectionInput(&input, body); err != nil {
		writeCustomModelError(w, err)
		return
	}
	if err := validateConnectionInput(input, true); err != nil {
		writeCustomModelError(w, err)
		return
	}
	conn, created, err := s.opts.CreateCustomConnection(input)
	if err != nil {
		writeConnectionError(w, err)
		return
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"ok": true, "created": created, "connection": conn})
}

func (s *server) handleCustomConnection(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPut, http.MethodDelete) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "接入 id 不能为空", "bad_request")
		return
	}
	if r.Method == http.MethodDelete {
		if s.opts.DeleteCustomConnection == nil {
			s.unavailable(w)
			return
		}
		found, err := s.opts.DeleteCustomConnection(id)
		if err != nil {
			writeConnectionError(w, err)
			return
		}
		if !found {
			writeError(w, http.StatusNotFound, "自定义接入不存在", "not_found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
		return
	}
	if s.opts.UpdateCustomConnection == nil {
		s.unavailable(w)
		return
	}
	var body map[string]json.RawMessage
	if !decodeJSON(w, r, &body) {
		return
	}
	patch, err := parseConnectionPatch(body)
	if err != nil {
		writeCustomModelError(w, err)
		return
	}
	if err := validateConnectionPatch(patch); err != nil {
		writeCustomModelError(w, err)
		return
	}
	conn, err := s.opts.UpdateCustomConnection(id, patch)
	if err != nil {
		writeConnectionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "connection": conn})
}

func (s *server) handleCustomConnectionTest(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	if s.opts.TestCustomConnection == nil {
		s.unavailable(w)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	latency, count, err := s.opts.TestCustomConnection(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "自定义接入不存在", "not_found")
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"ok": false, "latency_ms": latency, "error": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "latency_ms": latency, "model_count": count,
	})
}

func (s *server) handleCustomConnectionRemoteModels(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if s.opts.FetchCustomConnectionModels == nil {
		s.unavailable(w)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	models, err := s.opts.FetchCustomConnectionModels(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "自定义接入不存在", "not_found")
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"ok": false, "error": err.Error(),
		})
		return
	}
	if models == nil {
		models = []RemoteModel{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

func (s *server) handleCustomConnectionImport(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	if s.opts.ImportCustomConnectionModels == nil {
		s.unavailable(w)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	var body struct {
		Models []ImportModelInput `json:"models"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if len(body.Models) == 0 {
		writeError(w, http.StatusBadRequest, "至少勾选一个模型", "invalid_request")
		return
	}
	result, err := s.opts.ImportCustomConnectionModels(id, body.Models)
	if err != nil {
		writeConnectionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true,
		"created": result.Created, "skipped": result.Skipped})
}

func applyConnectionInput(input *CustomConnectionInput, body map[string]json.RawMessage) error {
	for field, raw := range body {
		var err error
		switch field {
		case "label":
			err = json.Unmarshal(raw, &input.Label)
		case "protocol":
			err = json.Unmarshal(raw, &input.Protocol)
		case "base_url":
			err = json.Unmarshal(raw, &input.BaseURL)
		case "api_key":
			err = json.Unmarshal(raw, &input.APIKey)
		case "enabled":
			err = json.Unmarshal(raw, &input.Enabled)
		}
		if err != nil {
			return fmt.Errorf("%w: 字段 %s 类型不正确", ErrInvalidRequest, field)
		}
	}
	return nil
}

func parseConnectionPatch(body map[string]json.RawMessage) (CustomConnectionPatch, error) {
	var patch CustomConnectionPatch
	for field, raw := range body {
		switch field {
		case "label", "protocol", "base_url", "api_key":
			value, err := customModelString(raw, field)
			if err != nil {
				return patch, err
			}
			switch field {
			case "label":
				patch.Label = &value
			case "protocol":
				patch.Protocol = &value
			case "base_url":
				patch.BaseURL = &value
			default:
				patch.APIKey = &value
			}
		case "enabled":
			var value bool
			if err := json.Unmarshal(raw, &value); err != nil {
				return patch, fmt.Errorf("%w: enabled 必须是布尔值", ErrInvalidRequest)
			}
			patch.Enabled = &value
		}
	}
	return patch, nil
}

func validateConnectionInput(input CustomConnectionInput, requireKey bool) error {
	if _, ok := store.NormalizeCustomProtocol(input.Protocol); !ok {
		return fmt.Errorf("%w: 协议必须是 openai、anthropic 或 gemini", ErrInvalidRequest)
	}
	if err := store.ValidateCustomModelBaseURL(input.BaseURL); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidRequest, customModelFieldMessage(err))
	}
	if requireKey && strings.TrimSpace(input.APIKey) == "" {
		return fmt.Errorf("%w: API Key 不能为空", ErrInvalidRequest)
	}
	return nil
}

func validateConnectionPatch(patch CustomConnectionPatch) error {
	if patch.Protocol != nil {
		if _, ok := store.NormalizeCustomProtocol(*patch.Protocol); !ok {
			return fmt.Errorf("%w: 协议必须是 openai、anthropic 或 gemini", ErrInvalidRequest)
		}
	}
	if patch.BaseURL != nil {
		if err := store.ValidateCustomModelBaseURL(*patch.BaseURL); err != nil {
			return fmt.Errorf("%w: %s", ErrInvalidRequest, customModelFieldMessage(err))
		}
	}
	return nil
}

func writeConnectionError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrCustomConnectionExists) {
		writeError(w, http.StatusConflict, err.Error(), "conflict")
		return
	}
	writeCustomModelError(w, err)
}
