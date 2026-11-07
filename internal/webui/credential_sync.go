package webui

import (
	"crypto/subtle"
	"encoding/json"
	"net"
	"net/http"
	"regexp"
	"strings"
)

// P4 凭据同步器（服务器接收 + 本机推送）。
//
// 两个端点，方向相反、安全模型不同：
//
//	POST /api/internal/credentials   接收端。服务器（或本机，同一份代码）收到
//	                                 另一台机器经 ssh 推来的凭据。机器对机器，
//	                                 没有 admin 会话 cookie，因此被 guard 显式
//	                                 豁免，改用「仅回环来源 + X-Sync-Token」。
//	POST /api/credentials/sync       推送端。普通管理端点（带会话 cookie），
//	                                 由宿主把本机插件凭据经 ssh + curl 推到目标。
//
// token 存 secrets extra（notify.url 先例）；未配置 → 503 明确文案。
// 响应一律脱敏：只回 account_id / keys / present，永不含 secret 值。

// internalCredentialsEndpoint is the machine-to-machine receive route. The
// guard exempts it from the session-cookie requirement because an ssh-driven
// curl has no browser session; its own checks (loopback + sync token) are
// stricter than a cookie for this payload.
const internalCredentialsEndpoint = "/api/internal/credentials"

// ErrSyncTokenMissing reports that the secrets store has no sync token. The
// handler maps it to 503 so the caller knows the endpoint is usable but
// unconfigured, rather than unauthorized.
type syncTokenMissingError struct{}

func (syncTokenMissingError) Error() string { return "凭据同步令牌未配置" }

// ErrSyncTokenMissing is the sentinel the host wraps when the token is absent.
var ErrSyncTokenMissing = syncTokenMissingError{}

// SyncCredentialItem is one credential inside a sync payload. Secret is the
// raw material (the same shape the bridge's /admin/credentials inject takes)
// and is only ever written into the target store, never echoed back.
type SyncCredentialItem struct {
	AccountID string         `json:"account_id"`
	Secret    map[string]any `json:"secret"`
	// ExpiresAt is unix seconds; 0 / omitted means "no expiry known".
	ExpiresAt int64 `json:"expires_at,omitempty"`
}

// syncCredentialRequest is the body of POST /api/internal/credentials.
type syncCredentialRequest struct {
	PluginID    string               `json:"plugin_id"`
	Credentials []SyncCredentialItem `json:"credentials"`
}

// SyncCredentialView is the redacted per-item response row.
type SyncCredentialView struct {
	AccountID string   `json:"account_id"`
	Keys      []string `json:"keys"`
	Present   bool     `json:"present"`
}

// SyncResult is what both endpoints answer with; Secrets never appear.
type SyncResult struct {
	PluginID    string               `json:"plugin_id,omitempty"`
	Count       int                  `json:"count"`
	Credentials []SyncCredentialView `json:"credentials"`
}

// isLoopbackRemoteAddr reports whether the request arrived from a loopback
// source. Unparseable addresses fail closed, like internal/app.isLoopbackRemote.
func isLoopbackRemoteAddr(remote string) bool {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return false
	}
	host := remote
	if h, _, err := net.SplitHostPort(remote); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// handleInternalCredentials serves POST /api/internal/credentials.
func (s *server) handleInternalCredentials(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	// 仅回环来源放行（与桥 /admin 的 loopback 拒绝同语义）：ssh 端点绑定
	// 127.0.0.1:7865，跨机调用必须经 ssh 隧道，这个检查保证绕不过去。
	if !isLoopbackRemoteAddr(r.RemoteAddr) {
		writeError(w, http.StatusForbidden, "凭据同步接收端点仅接受本机回环来源", "loopback_only")
		return
	}
	if s.opts.CredentialSyncToken == nil || s.opts.SyncCredentials == nil {
		s.unavailable(w)
		return
	}
	expected, ok := s.opts.CredentialSyncToken()
	if !ok || strings.TrimSpace(expected) == "" {
		// 未配置 token：端点存在但不可用（503，明确文案），不是 401——
		// 401 会把「没配置」伪装成「配置错了」，排障时浪费一轮。
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]string{
				"message": ErrSyncTokenMissing.Error(),
				"type":    "sync_token_unconfigured",
			},
		})
		return
	}
	presented := strings.TrimSpace(r.Header.Get("X-Sync-Token"))
	if presented == "" ||
		subtle.ConstantTimeCompare([]byte(presented), []byte(strings.TrimSpace(expected))) != 1 {
		writeError(w, http.StatusUnauthorized, "凭据同步令牌缺失或不匹配", "invalid_sync_token")
		return
	}

	var body syncCredentialRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	body.PluginID = strings.TrimSpace(body.PluginID)
	if body.PluginID == "" {
		writeError(w, http.StatusBadRequest, "plugin_id is required", "invalid_request")
		return
	}
	if len(body.Credentials) == 0 {
		writeError(w, http.StatusBadRequest, "credentials must be a non-empty array", "invalid_request")
		return
	}
	for i, item := range body.Credentials {
		if strings.TrimSpace(item.AccountID) == "" {
			writeError(w, http.StatusBadRequest, "credentials["+jsonInt(i)+"].account_id is required", "invalid_request")
			return
		}
		if item.Secret == nil {
			writeError(w, http.StatusBadRequest, "credentials["+jsonInt(i)+"].secret must be an object", "invalid_request")
			return
		}
	}

	result, err := s.opts.SyncCredentials(body.PluginID, body.Credentials)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "写入同步凭据失败："+err.Error(), "internal_error")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// jsonInt renders an array index for an error message (no formatting verbs).
func jsonInt(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

// ---------------------------------------------------------------- push side

// syncTargetPattern whitelists the ssh target shape: user@host, where host is
// a hostname, an IPv4 literal or a bracketed IPv6 literal, optionally followed
// by :port (1..65535). Anything carrying shell metacharacters is rejected
// before it can reach argv.
var syncTargetPattern = regexp.MustCompile(
	`^[A-Za-z0-9._-]{1,64}@([A-Za-z0-9._-]{1,255}|\[[0-9a-fA-F:.]{2,45}\])(:[1-9][0-9]{0,4})?$`)

// ValidSyncTarget reports whether target is an acceptable user@host ssh
// destination. Exported so the host side and its tests share one verdict.
func ValidSyncTarget(target string) bool {
	if !syncTargetPattern.MatchString(strings.TrimSpace(target)) {
		return false
	}
	// Bound the port inside 1..65535 (the regexp only bounds the digit count).
	if i := strings.LastIndexByte(target, ':'); i >= 0 && !strings.Contains(target[i:], "]") {
		port := 0
		for _, d := range target[i+1:] {
			port = port*10 + int(d-'0')
		}
		if port > 65535 {
			return false
		}
	}
	return true
}

// CredentialsSyncRequest is the body of POST /api/credentials/sync.
type CredentialsSyncRequest struct {
	// Target is the ssh destination, "user@host[:port]".
	Target string `json:"target"`
	// DryRun builds and returns the command shape (token masked) without
	// touching ssh or the network.
	DryRun bool `json:"dry_run,omitempty"`
	// PluginID optionally narrows the push to one plugin; empty means all
	// plugins that have local credentials.
	PluginID string `json:"plugin_id,omitempty"`
}

// CredentialsSyncResult is the response of POST /api/credentials/sync. For a
// dry run, Command carries the exact ssh invocation with the token masked; the
// real run reports the target's own (already redacted) answer in Sync.
type CredentialsSyncResult struct {
	Target  string `json:"target"`
	DryRun  bool   `json:"dry_run"`
	Count   int    `json:"count"`
	Command string `json:"command,omitempty"`
	// Sync is the remote endpoint's redacted answer (present on a real push).
	Sync *SyncResult `json:"sync,omitempty"`
	// Output is the raw remote stdout on failure, truncated; empty on success.
	Output string `json:"output,omitempty"`
}

// handleCredentialsSync serves POST /api/credentials/sync. It is a normal
// admin endpoint: the guard's session cookie and same-origin checks apply.
func (s *server) handleCredentialsSync(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	if s.opts.CredentialsSync == nil {
		s.unavailable(w)
		return
	}
	var body CredentialsSyncRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if !ValidSyncTarget(body.Target) {
		writeError(w, http.StatusBadRequest, "目标必须是 user@host 形式的 ssh 地址", "invalid_request")
		return
	}
	body.PluginID = strings.TrimSpace(body.PluginID)
	result, err := s.opts.CredentialsSync(body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "凭据同步推送失败："+err.Error(), "sync_failed")
		return
	}
	if result == nil {
		writeError(w, http.StatusInternalServerError, "凭据同步推送失败：空结果", "sync_failed")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// guardExemptInternalCredentials reports whether path bypasses the session
// guard. Only the machine-to-machine receive route qualifies.
func guardExemptInternalCredentials(path string) bool {
	return path == internalCredentialsEndpoint
}
