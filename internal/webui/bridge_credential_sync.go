// task-90：宿主→桥凭据转发端点。
//
// P4 接收端把凭据写宿主 secrets extra（credential_sync.go 的
// credential.<plugin_id>.<account_id> 键），但桥凭据库在桥进程自己的
// credentials.json 里——两者之间无自动转发通道。本端点补上：
//
//	POST /api/plugins/{id}/credentials/sync
//
// 从宿主 extra 读该插件的凭据 → 经 supervisor 已知的插件动态端口 + token →
// POST 到桥 /admin/credentials。响应只含成功/失败计数 + 脱敏字段名，凭据值
// 绝不回显、绝不进日志。桥的 credential store put 是 upsert，重复 sync 幂等。
package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// BridgeCredentialItem 是转发到桥的单条凭据（形状 = 桥 /admin/credentials
// 注入端点的 body 字段）。复用 SyncCredentialItem 的 AccountID/Secret/ExpiresAt
// 字段，不走类型别名（两个包各自维护，宿主不把 SyncCredentialItem 直接暴露给
// 桥消费者）。
type BridgeCredentialItem = SyncCredentialItem

// BridgeSyncChannelResult 是一个渠道（账号）的转发结果。
type BridgeSyncChannelResult struct {
	AccountID string   `json:"account_id"`
	OK        bool     `json:"ok"`
	Error     string   `json:"error,omitempty"`
	Keys      []string `json:"keys,omitempty"` // 脱敏后的字段名列表
}

// BridgeSyncResult 是 POST /api/plugins/{id}/credentials/sync 的响应体。
// 凭据值绝不出现。
type BridgeSyncResult struct {
	PluginID string                     `json:"plugin_id"`
	Sent     int                        `json:"sent"`
	Failed   int                        `json:"failed"`
	Channels []BridgeSyncChannelResult  `json:"channels"`
}

// Options 追加的两个回调（task-90）：
//   - BridgeCredentials 读宿主 extra 里该插件的凭据（按渠道拆好的列表）。
//   - PluginBridgeEndpoint 返回该插件桥的 base URL（含动态端口）和 bearer token。
//
// 两个 nil = 端点 501（与其他可选端点一致）。

// bridgeCredentialTimeout bounds one POST to the bridge.
const bridgeCredentialTimeout = 10 * time.Second

// handleBridgeCredentialSync serves POST /api/plugins/{id}/credentials/sync.
func (s *server) handleBridgeCredentialSync(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	pluginID := strings.TrimSpace(r.PathValue("id"))
	if pluginID == "" {
		writeError(w, http.StatusBadRequest, "plugin id is required", "bad_request")
		return
	}
	if s.opts.BridgeCredentials == nil || s.opts.PluginBridgeEndpoint == nil {
		s.unavailable(w)
		return
	}

	// ① 读宿主 extra 里的凭据（按渠道拆好的列表）。
	items, err := s.opts.BridgeCredentials(pluginID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "bridge credential sync: read: "+err.Error(), "internal_error")
		return
	}
	if len(items) == 0 {
		writeJSON(w, http.StatusOK, BridgeSyncResult{PluginID: pluginID, Channels: []BridgeSyncChannelResult{}})
		return
	}

	// ② 拿桥的 base URL + bearer token。
	baseURL, token, ok := s.opts.PluginBridgeEndpoint(pluginID)
	if !ok {
		writeError(w, http.StatusBadGateway, "插件未运行或端点不可用", "bridge_unreachable")
		return
	}

	// ③ 逐条 POST 到桥 /admin/credentials。
	client := &http.Client{Timeout: bridgeCredentialTimeout}
	result := BridgeSyncResult{PluginID: pluginID, Channels: make([]BridgeSyncChannelResult, 0, len(items))}
	for _, item := range items {
		ch := BridgeSyncChannelResult{AccountID: item.AccountID}
		body := map[string]any{
			"plugin_id":   pluginID,
			"account_id":  item.AccountID,
			"credentials": item.Secret,
		}
		raw, err := json.Marshal(body)
		if err != nil {
			ch.Error = "encode: " + err.Error()
			result.Channels = append(result.Channels, ch)
			result.Failed++
			continue
		}
		resp, err := forwardToBridge(client, baseURL, token, raw)
		if err != nil {
			ch.Error = err.Error()
			result.Channels = append(result.Channels, ch)
			result.Failed++
			continue
		}
		// 脱敏：只回字段名，不回值。
		keys := make([]string, 0, len(item.Secret))
		for k := range item.Secret {
			keys = append(keys, k)
		}
		ch.OK = true
		ch.Keys = keys
		result.Channels = append(result.Channels, ch)
		result.Sent++
		_ = resp
	}
	writeJSON(w, http.StatusOK, result)
}

// forwardToBridge POSTs one credential to the bridge's /admin/credentials.
// It returns the response body for error diagnostics; the caller decides what
// to do with non-2xx statuses. The credential value is never logged.
func forwardToBridge(client *http.Client, baseURL, token string, body []byte) (respBody []byte, err error) {
	url := strings.TrimRight(baseURL, "/") + "/admin/credentials"
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bridge unreachable: %w", err)
	}
	defer resp.Body.Close()
	respBody = make([]byte, 4096)
	n, _ := resp.Body.Read(respBody)
	respBody = respBody[:n]
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return respBody, fmt.Errorf("bridge rejected token (401)")
	case resp.StatusCode == http.StatusNotFound:
		return respBody, fmt.Errorf("bridge endpoint not found (404; old bridge version?)")
	case resp.StatusCode >= 400:
		return respBody, fmt.Errorf("bridge error (HTTP %d)", resp.StatusCode)
	}
	return respBody, nil
}
