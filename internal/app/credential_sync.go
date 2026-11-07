package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"aiworkgate/internal/webui"
)

// P4 凭据同步器（本机代码；服务器验收待部署后进行）。
//
// 双机拓扑（plans/2026-11-05 §P4 + docs/p8/00 安全加固 S-5）：
//
//	本机（Windows，DPAPI）--ssh+curl--> 服务器（127.0.0.1:7865 接收端点）
//
// 凭据与令牌都走 secrets.Store 的 extra 机制（notify.url 先例）：
//   - credential.<plugin_id>.<account_id>  一条凭据，值为 JSON secret 对象
//     （对齐桥 /admin/credentials 的 credentials 注入形状）；
//   - credential.<plugin_id>.<account_id>.expires_at  可选的 unix 秒过期；
//   - credential_sync.token  机器对机器同步令牌（X-Sync-Token）。
//
// 推送通道与 token 安全：ssh user@host "<remote>"，远端命令第一行用
// `read -r AIWG_SYNC_TOKEN` 从 ssh 的 stdin 取令牌，正文随后跟在同一 stdin
// 上给 curl 的 --data @-。令牌因此只存在于 ssh 加密通道与远端 shell 变量里
// —— 不进任何 argv、不进环境变量、不进日志。dry-run 只打印命令形状（令牌
// 打码）且完全不发。ssh 目标按 user@host 白名单校验后进 argv。

const (
	// CredentialSyncTokenSecretKey is the DPAPI extra key holding the sync
	// token shared with the receiving host.
	CredentialSyncTokenSecretKey = "credential_sync.token"
	// credentialSecretPrefix prefixes every synced plugin credential.
	credentialSecretPrefix = "credential."
	// credentialExpiresSuffix marks the optional expiry record of a credential.
	credentialExpiresSuffix = ".expires_at"
	// credentialsSyncTimeout bounds one ssh push; the remote curl is bounded by
	// the receiving server, so this covers connect + remote round trip.
	credentialsSyncTimeout = 60 * time.Second
	// remoteSyncURL is the loopback URL the remote curl hits. The server binds
	// its admin surface to 127.0.0.1 by default; the ssh channel is what makes
	// the call "remote" without ever exposing the endpoint publicly.
	remoteSyncURL = "http://127.0.0.1:7865/api/internal/credentials"
	// maskedToken is the placeholder that replaces the token in dry-run output.
	maskedToken = "<redacted>"
)

// credentialExtraKey is the DPAPI extra key of one synced credential.
func credentialExtraKey(pluginID, accountID string) string {
	return credentialSecretPrefix + pluginID + "." + accountID
}

// parseCredentialExtraKey splits an extra key back into (pluginID, accountID,
// ok). Expiry records (the .expires_at suffix) are not credentials and report
// not-ok, so listing never mistakes them for secrets.
func parseCredentialExtraKey(key string) (pluginID, accountID string, ok bool) {
	if !strings.HasPrefix(key, credentialSecretPrefix) || strings.HasSuffix(key, credentialExpiresSuffix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(key, credentialSecretPrefix)
	dot := strings.Index(rest, ".")
	if dot <= 0 || dot == len(rest)-1 {
		return "", "", false
	}
	return rest[:dot], rest[dot+1:], true
}

// setCredentialSyncToken stores (or clears, with "") the sync token. A token
// containing control characters is rejected: the stdin handshake is
// line-framed and such a token could never be delivered.
func (a *App) setCredentialSyncToken(token string) error {
	if a.secrets == nil {
		return fmt.Errorf("app: secret store is not available")
	}
	token = strings.TrimSpace(token)
	if !tokenIsValidForSync(token) {
		return fmt.Errorf("%w: 同步令牌不能包含控制字符", webui.ErrInvalidRequest)
	}
	return a.secrets.SetExtra(CredentialSyncTokenSecretKey, token)
}

// credentialSyncToken reads the sync token; ok=false means unconfigured.
func (a *App) credentialSyncToken() (string, bool) {
	if a.secrets == nil {
		return "", false
	}
	tok, ok := a.secrets.GetExtra(CredentialSyncTokenSecretKey)
	return tok, ok
}

// tokenIsValidForSync rejects control characters that would break the
// line-framed stdin handshake.
func tokenIsValidForSync(token string) bool {
	for _, r := range token {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// syncCredentials implements the receive half: apply a payload into the local
// extra store and answer with the redacted view. Secret values never echo.
func (a *App) syncCredentials(pluginID string, items []webui.SyncCredentialItem) (webui.SyncResult, error) {
	if a.secrets == nil {
		return webui.SyncResult{}, fmt.Errorf("app: secret store is not available")
	}
	pluginID = strings.TrimSpace(pluginID)
	if pluginID == "" {
		return webui.SyncResult{}, fmt.Errorf("app: empty plugin id")
	}
	views := make([]webui.SyncCredentialView, 0, len(items))
	for _, item := range items {
		accountID := strings.TrimSpace(item.AccountID)
		raw, err := json.Marshal(item.Secret)
		if err != nil {
			return webui.SyncResult{}, fmt.Errorf("app: encode credential %s: %w", accountID, err)
		}
		if err := a.secrets.SetExtra(credentialExtraKey(pluginID, accountID), string(raw)); err != nil {
			return webui.SyncResult{}, err
		}
		if item.ExpiresAt > 0 {
			if err := a.secrets.SetExtra(credentialExtraKey(pluginID, accountID)+credentialExpiresSuffix, fmt.Sprint(item.ExpiresAt)); err != nil {
				return webui.SyncResult{}, err
			}
		}
		keys := make([]string, 0, len(item.Secret))
		for k := range item.Secret {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		views = append(views, webui.SyncCredentialView{AccountID: accountID, Keys: keys, Present: true})
	}
	return webui.SyncResult{PluginID: pluginID, Count: len(views), Credentials: views}, nil
}

// localSyncCredentials collects every locally stored plugin credential as a
// push payload. The DPAPI store is the single source; a non-empty pluginID
// narrows the push to that one plugin.
func (a *App) localSyncCredentials(pluginID string) map[string][]webui.SyncCredentialItem {
	out := map[string][]webui.SyncCredentialItem{}
	if a.secrets == nil {
		return out
	}
	pluginID = strings.TrimSpace(pluginID)
	for _, key := range a.secrets.ExtraKeys() {
		pID, accountID, ok := parseCredentialExtraKey(key)
		if !ok || (pluginID != "" && pID != pluginID) {
			continue
		}
		raw, ok := a.secrets.GetExtra(key)
		if !ok {
			continue
		}
		var secret map[string]any
		if err := json.Unmarshal([]byte(raw), &secret); err != nil || secret == nil {
			// A malformed record is skipped, not fatal: one bad entry must not
			// block pushing the rest.
			continue
		}
		item := webui.SyncCredentialItem{AccountID: accountID, Secret: secret}
		if expRaw, ok := a.secrets.GetExtra(key + credentialExpiresSuffix); ok {
			var expires int64
			if _, err := fmt.Sscan(expRaw, &expires); err == nil {
				item.ExpiresAt = expires
			}
		}
		out[pID] = append(out[pID], item)
	}
	return out
}

// maskSyncToken replaces every occurrence of the token in rendered text with
// the fixed placeholder, so dry-run output is safe to show and to log.
func maskSyncToken(text, token string) string {
	if token == "" {
		return text
	}
	return strings.ReplaceAll(text, token, maskedToken)
}

// sshTargetPattern is the authoritative user@host shape (mirrors
// webui.ValidSyncTarget; duplicated so the command builder and the HTTP
// validation cannot drift apart silently).
var sshTargetPattern = regexp.MustCompile(
	`^[A-Za-z0-9._-]{1,64}@([A-Za-z0-9._-]{1,255}|\[[0-9a-fA-F:.]{2,45}\])(:[1-9][0-9]{0,4})?$`)

// validSyncTarget mirrors webui.ValidSyncTarget, including the port range
// check the regexp cannot express.
func validSyncTarget(target string) bool {
	if !sshTargetPattern.MatchString(strings.TrimSpace(target)) {
		return false
	}
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

// remoteSyncCommand is the command executed on the remote host. The first
// stdin line carries the token (read -r), the rest is the JSON body for
// curl's --data @-.
func remoteSyncCommand() string {
	return `read -r AIWG_SYNC_TOKEN && curl -sS -X POST` +
		` -H "Content-Type: application/json"` +
		` -H "X-Sync-Token: $AIWG_SYNC_TOKEN"` +
		` --data @- ` + remoteSyncURL
}

// buildSyncCommandShape renders the ssh invocation for display (dry run).
// The token placeholder shows where the value would ride: through stdin,
// never through argv.
func buildSyncCommandShape(target string) string {
	return `printf '%s\n' "$TOKEN" <body...> | ssh ` + target + ` ` + strconvQuote(remoteSyncCommand())
}

// strconvQuote renders a single-quoted POSIX shell word.
func strconvQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// runCredentialsSync performs (or simulates) one push.
func (a *App) runCredentialsSync(req webui.CredentialsSyncRequest) (*webui.CredentialsSyncResult, error) {
	target := strings.TrimSpace(req.Target)
	if !validSyncTarget(target) {
		return nil, fmt.Errorf("%w: 目标必须是 user@host 形式的 ssh 地址", webui.ErrInvalidRequest)
	}
	token, ok := a.credentialSyncToken()
	if !ok || strings.TrimSpace(token) == "" {
		return nil, webui.ErrSyncTokenMissing
	}
	if !tokenIsValidForSync(token) {
		return nil, fmt.Errorf("%w: 同步令牌包含不可传送的字符，请重新配置", webui.ErrInvalidRequest)
	}

	perPlugin := a.localSyncCredentials(req.PluginID)
	total := 0
	for _, items := range perPlugin {
		total += len(items)
	}
	result := &webui.CredentialsSyncResult{Target: target, DryRun: req.DryRun, Count: total}
	if total == 0 {
		return result, nil
	}
	if req.DryRun {
		// Dry run prints the command shape only, token masked; nothing runs.
		result.Command = maskSyncToken(buildSyncCommandShape(target), token)
		return result, nil
	}

	// One ssh per plugin: the receive endpoint takes one {plugin_id,
	// credentials} object per call, and with the realistic one-or-two plugins
	// per host, sequential correctness beats parallel throughput.
	var lastSync *webui.SyncResult
	for _, pluginID := range sortedPluginIDs(perPlugin) {
		body, err := json.Marshal(map[string]any{
			"plugin_id":   pluginID,
			"credentials": perPlugin[pluginID],
		})
		if err != nil {
			return nil, err
		}
		sync, out, err := a.runOneSync(target, token, body)
		if err != nil {
			if out != "" {
				return nil, fmt.Errorf("ssh 推送失败：%s", firstLine(out))
			}
			return nil, err
		}
		lastSync = &sync
	}
	result.Sync = lastSync
	return result, nil
}

// sortedPluginIDs returns the payload keys in stable order.
func sortedPluginIDs(perPlugin map[string][]webui.SyncCredentialItem) []string {
	ids := make([]string, 0, len(perPlugin))
	for id := range perPlugin {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// runOneSync executes one ssh push. ssh's stdin carries
// "<token>\n{body}": the remote `read -r` consumes the first line into a
// shell variable and curl reads the rest. The token lives only in the ssh
// transport and the remote shell variable — never in a local or remote argv
// row, never in an environment block, never in this process's logs.
func (a *App) runOneSync(target, token string, body []byte) (sync webui.SyncResult, remoteOut string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), credentialsSyncTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", target, remoteSyncCommand())
	cmd.Stdin = io.MultiReader(strings.NewReader(token+"\n"), bytes.NewReader(body))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	out := strings.TrimSpace(stdout.String())
	if runErr != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = runErr.Error()
		}
		return webui.SyncResult{}, "", fmt.Errorf("ssh 推送失败：%s", firstLine(detail))
	}
	if out != "" {
		if jsonErr := json.Unmarshal([]byte(out), &sync); jsonErr != nil {
			// A non-JSON answer means something other than the endpoint
			// answered (proxy page, wrong port). Surface one line; never echo
			// the request we sent.
			return webui.SyncResult{}, out, fmt.Errorf("服务器返回非 JSON 响应：%s", firstLine(out))
		}
	}
	return sync, "", nil
}

// firstLine cuts a multi-line string at the first newline (whole string when
// there is none), bounded to 200 runes for log safety.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	r := []rune(s)
	if len(r) > 200 {
		r = r[:200]
	}
	return string(r)
}
