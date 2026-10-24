package app

// Connection-level operations: upstream model discovery, the explicit
// connectivity probe and batch import. These run only on user action; saving a
// connection never touches the network.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"aiworkgate/internal/gateway"
	"aiworkgate/internal/store"
	"aiworkgate/internal/webui"
)

const (
	customAdminTimeout    = 20 * time.Second
	customRemoteBodyBytes = 4 << 20
	customErrorReadBytes  = 8 << 10
)

// newCustomAdminClient builds the client used for discovery and probes. The
// upstream is user-configured, so environment proxies are ignored and
// redirects are not followed (a 3xx would replay the stored key elsewhere).
func newCustomAdminClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   timeout,
			ResponseHeaderTimeout: timeout,
			IdleConnTimeout:       timeout,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// fetchCustomConnectionModels calls the protocol's model-list endpoint.
func (a *App) fetchCustomConnectionModels(id string) ([]webui.RemoteModel, error) {
	conn, key, err := a.customConnectionForCall(id)
	if err != nil {
		return nil, err
	}
	endpoint, err := gateway.CustomProtocolModelsURL(conn.Protocol, conn.BaseURL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, errors.New("接入的 API 地址无效")
	}
	req.Header.Set("Accept", "application/json")
	for name, value := range gateway.CustomProtocolHeaders(conn.Protocol, key) {
		req.Header.Set(name, value)
	}
	client := newCustomAdminClient(customAdminTimeout)
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("无法连接该接入（请检查地址、Key 与网络）")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, customRemoteBodyBytes+1))
	if err != nil {
		return nil, errors.New("读取上游模型列表失败")
	}
	if len(body) > customRemoteBodyBytes {
		return nil, errors.New("上游模型列表过大")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("上游返回状态 %d：%s", resp.StatusCode, redactConnectionKey(errorSnippet(body), key))
	}
	models, err := parseRemoteModels(conn.Protocol, body)
	if err != nil {
		return nil, err
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models, nil
}

func (a *App) testCustomConnection(id string) (int64, int, error) {
	start := time.Now()
	models, err := a.fetchCustomConnectionModels(id)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return latency, 0, err
	}
	return latency, len(models), nil
}

func (a *App) customConnectionForCall(id string) (store.CustomConnection, string, error) {
	if a.store == nil {
		return store.CustomConnection{}, "", errors.New("app: custom connection store is not available")
	}
	conn, found, err := a.store.GetCustomConnection(id)
	if err != nil {
		return store.CustomConnection{}, "", err
	}
	if !found {
		return store.CustomConnection{}, "", store.ErrNotFound
	}
	key, _ := a.connectionKey(conn.ID)
	if strings.TrimSpace(key) == "" {
		return store.CustomConnection{}, "", errors.New("该接入还没有配置 API Key")
	}
	return conn, key, nil
}

func parseRemoteModels(protocol string, body []byte) ([]webui.RemoteModel, error) {
	switch gateway.NormalizeCustomProtocolName(protocol) {
	case "anthropic":
		var payload struct {
			Data []struct {
				ID          string `json:"id"`
				DisplayName string `json:"display_name"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, errors.New("上游模型列表不是预期的 Anthropic 格式")
		}
		out := make([]webui.RemoteModel, 0, len(payload.Data))
		for _, item := range payload.Data {
			name := strings.TrimSpace(item.DisplayName)
			if name == "" {
				name = item.ID
			}
			out = append(out, webui.RemoteModel{ID: item.ID, Name: name})
		}
		return out, nil
	case "gemini":
		var payload struct {
			Models []struct {
				Name             string `json:"name"`
				DisplayName      string `json:"displayName"`
				InputTokenLimit  int    `json:"inputTokenLimit"`
				OutputTokenLimit int    `json:"outputTokenLimit"`
			} `json:"models"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, errors.New("上游模型列表不是预期的 Gemini 格式")
		}
		out := make([]webui.RemoteModel, 0, len(payload.Models))
		for _, item := range payload.Models {
			id := strings.TrimPrefix(strings.TrimSpace(item.Name), "models/")
			if id == "" {
				continue
			}
			name := strings.TrimSpace(item.DisplayName)
			if name == "" {
				name = id
			}
			out = append(out, webui.RemoteModel{
				ID: id, Name: name,
				ContextWindow: item.InputTokenLimit, MaxOutputTokens: item.OutputTokenLimit,
			})
		}
		return out, nil
	default:
		var payload struct {
			Data []struct {
				ID      string `json:"id"`
				OwnedBy string `json:"owned_by"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, errors.New("上游模型列表不是预期的 OpenAI 格式")
		}
		out := make([]webui.RemoteModel, 0, len(payload.Data))
		for _, item := range payload.Data {
			out = append(out, webui.RemoteModel{ID: item.ID, Name: item.ID, OwnedBy: item.OwnedBy})
		}
		return out, nil
	}
}

// importCustomConnectionModels creates one entry per selected upstream model.
// Existing upstream ids in the connection are skipped; display names are
// sanitized (no "/") and made unique across all custom models.
func (a *App) importCustomConnectionModels(id string, inputs []webui.ImportModelInput) (webui.ImportModelsResult, error) {
	result := webui.ImportModelsResult{Created: []store.CustomModel{}, Skipped: []webui.ImportSkip{}}
	conn, found, err := a.store.GetCustomConnection(id)
	if err != nil {
		return result, err
	}
	if !found {
		return result, store.ErrNotFound
	}
	existing, err := a.store.ListCustomModelsByConnection(conn.ID)
	if err != nil {
		return result, err
	}
	upstreamSeen := map[string]bool{}
	usedNames := map[string]bool{}
	for _, row := range existing {
		upstreamSeen[strings.TrimSpace(row.UpstreamModel)] = true
		usedNames[store.CanonicalCustomModelName(row.Name)] = true
	}
	all, err := a.store.ListCustomModels()
	if err != nil {
		return result, err
	}
	for _, row := range all {
		usedNames[store.CanonicalCustomModelName(row.Name)] = true
	}

	for _, input := range inputs {
		upstream := strings.TrimSpace(input.UpstreamModel)
		if upstream == "" {
			continue
		}
		if upstreamSeen[upstream] {
			result.Skipped = append(result.Skipped, webui.ImportSkip{
				UpstreamModel: upstream, Reason: "该接入里已经有这个上游模型",
			})
			continue
		}
		name := uniqueCustomModelName(sanitizeCustomModelName(input.Name, upstream), usedNames)
		row, err := a.store.UpsertCustomModel(store.CustomModel{
			ConnectionID: conn.ID, Name: name, BaseURL: conn.BaseURL,
			UpstreamModel: upstream, Enabled: true,
			ContextWindow: input.ContextWindow, MaxOutputTokens: input.MaxOutputTokens,
			InputTypes: input.InputTypes, ReasoningTiers: input.ReasoningTiers,
			DefaultReasoning: input.DefaultReasoning,
		})
		if err != nil {
			result.Skipped = append(result.Skipped, webui.ImportSkip{
				UpstreamModel: upstream, Reason: customModelImportReason(err),
			})
			continue
		}
		row.ConnectionEnabled = conn.Enabled
		upstreamSeen[upstream] = true
		usedNames[store.CanonicalCustomModelName(row.Name)] = true
		result.Created = append(result.Created, row)
	}
	return result, nil
}

func sanitizeCustomModelName(name, upstream string) string {
	value := strings.TrimSpace(name)
	if value == "" {
		value = strings.TrimSpace(upstream)
	}
	value = strings.ReplaceAll(value, "/", "-")
	value = strings.Join(strings.Fields(value), "-")
	value = strings.Trim(value, "-")
	runes := []rune(value)
	if len(runes) > store.MaxCustomModelNameRunes {
		value = string(runes[:store.MaxCustomModelNameRunes])
	}
	if value == "" {
		value = "model"
	}
	return value
}

func uniqueCustomModelName(base string, used map[string]bool) string {
	if !used[store.CanonicalCustomModelName(base)] {
		return base
	}
	runes := []rune(base)
	for i := 2; i < 1000; i++ {
		suffix := fmt.Sprintf("-%d", i)
		trimmed := runes
		if max := store.MaxCustomModelNameRunes - len([]rune(suffix)); len(trimmed) > max {
			trimmed = trimmed[:max]
		}
		candidate := string(trimmed) + suffix
		if !used[store.CanonicalCustomModelName(candidate)] {
			return candidate
		}
	}
	return base
}

func customModelImportReason(err error) string {
	if errors.Is(err, store.ErrCustomModelConflict) {
		return "模型名称已被占用，已跳过"
	}
	if errors.Is(err, store.ErrInvalidArgument) {
		return "参数不合法，已跳过"
	}
	return "写入失败，已跳过"
}

// customProbePayload builds the smallest chat request each protocol accepts.
func customProbePayload(protocol, upstreamModel string) ([]byte, error) {
	switch gateway.NormalizeCustomProtocolName(protocol) {
	case "anthropic":
		return json.Marshal(map[string]any{
			"model": upstreamModel, "max_tokens": 1,
			"messages": []map[string]string{{"role": "user", "content": "ping"}},
		})
	case "gemini":
		return json.Marshal(map[string]any{
			"contents": []map[string]any{{
				"role": "user", "parts": []map[string]string{{"text": "ping"}},
			}},
			"generationConfig": map[string]any{"maxOutputTokens": 1},
		})
	default:
		return json.Marshal(map[string]any{
			"model": upstreamModel, "max_tokens": 1, "stream": false,
			"messages": []map[string]string{{"role": "user", "content": "ping"}},
		})
	}
}

func errorSnippet(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > 300 {
		text = text[:300]
	}
	if text == "" {
		return "（无响应内容）"
	}
	return text
}

func redactConnectionKey(text, key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return text
	}
	text = strings.ReplaceAll(text, "Bearer "+key, "Bearer ***")
	return strings.ReplaceAll(text, key, "***")
}
