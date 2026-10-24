package gateway

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"aiworkgate/internal/model"
)

// proxyFailureHintWindow is N of the P0 clause: a tunnel failure older than
// this no longer explains the current 5xx. Ten minutes covers a proxy that was
// switched off while a client kept retrying, and stays short enough that a
// long-fixed proxy does not keep blaming new upstream errors.
const proxyFailureHintWindow = 10 * time.Minute

// egressHintMaxRunes caps the appended clause (~160 runes) so an oversized
// tunnel error cannot turn a one-line message into a payload.
const egressHintMaxRunes = 160

// egressHintTimeout bounds the store reads on the failure path. The hint is
// best-effort: past this budget the original message is written unchanged.
var egressHintTimeout = 250 * time.Millisecond

// EgressHintSource is the store read side the clause needs: the account's
// egress assignment and its newest failed tunnel row. *store.Store satisfies
// it; the gateway never imports the store package, so the host hands the same
// object over through an interface.
type EgressHintSource interface {
	ListAccountEgress(pluginID string) ([]model.AccountEgress, error)
	LatestProxyFailure(pluginID, accountID string, since int64) (*model.ProxyLog, error)
}

var (
	egressHintMu       sync.RWMutex
	egressHintOverride EgressHintSource
)

// InstallEgressHintSource wires the egress reader. The host does not have to
// call it: app.go already installs the store as the key-usage reserver, and
// that same *store.Store satisfies EgressHintSource, so it is used by default.
// An explicit source wins. Nil restores the default (tests, standalone hosts).
func InstallEgressHintSource(src EgressHintSource) {
	egressHintMu.Lock()
	defer egressHintMu.Unlock()
	egressHintOverride = src
}

func currentEgressHintSource() EgressHintSource {
	egressHintMu.RLock()
	src := egressHintOverride
	egressHintMu.RUnlock()
	if src != nil {
		return src
	}
	// The startup wiring hands the concrete store in as the quota reserver
	// (app.go: gateway.InstallKeyUsageReserver(a.store)); reuse that object.
	if src, ok := currentKeyUsageReserver().(EgressHintSource); ok {
		return src
	}
	return nil
}

// egressFailureHint builds the one-line clause for a failed request that was
// routed to a specific (plugin, account), or "" when either condition is
// missing. All store work runs off the request goroutine under a short
// deadline: a broken or slow store simply yields "".
func (g *Gateway) egressFailureHint(rec *reqRecord) string {
	if g == nil || rec == nil {
		return ""
	}
	pluginID, accountID := egressHintTarget(rec)
	if pluginID == "" || accountID == "" {
		return ""
	}
	src := currentEgressHintSource()
	if src == nil {
		return ""
	}
	now := time.Now
	if g.now != nil {
		now = g.now
	}
	at := now()

	out := make(chan string, 1)
	go func() {
		out <- buildEgressHintFromSource(src, pluginID, accountID, at)
	}()
	select {
	case clause := <-out:
		return clause
	case <-time.After(egressHintTimeout):
		return ""
	}
}

// egressHintTarget recovers the (plugin, account) pair the request was routed
// to. Both are needed: a hint that cannot name the account would be noise.
func egressHintTarget(rec *reqRecord) (pluginID, accountID string) {
	if rec == nil {
		return "", ""
	}
	source := strings.TrimSpace(rec.source)
	if !strings.HasPrefix(source, "plugin:") {
		return "", ""
	}
	pluginID = strings.TrimSpace(strings.TrimPrefix(source, "plugin:"))
	accountID = strings.TrimSpace(rec.account)
	return pluginID, accountID
}

// buildEgressHintFromSource asks the store whether this account egresses
// through a proxy and, if so, whether that tunnel failed recently.
func buildEgressHintFromSource(src EgressHintSource, pluginID, accountID string, now time.Time) string {
	assignments, err := src.ListAccountEgress(pluginID)
	if err != nil {
		return ""
	}
	proxied := false
	for _, a := range assignments {
		if strings.TrimSpace(a.AccountID) != accountID {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(a.Mode), "proxy") {
			proxied = true
		}
		break
	}
	if !proxied {
		return ""
	}
	row, err := src.LatestProxyFailure(pluginID, accountID, now.Add(-proxyFailureHintWindow).Unix())
	if err != nil || row == nil {
		return ""
	}
	return buildEgressHint(row, now)
}

// buildEgressHint renders the clause:
//
//	；该账号经代理「<label>」出网，最近一次隧道失败：<error>（<m:ss> 前）
//
// label falls back to the proxy id when the log row carries no name. The
// tunnel error is flattened to one line, has any userinfo redacted, and is
// truncated so the clause stays within egressHintMaxRunes.
func buildEgressHint(row *model.ProxyLog, now time.Time) string {
	if row == nil {
		return ""
	}
	label := strings.Join(strings.Fields(strings.TrimSpace(row.ProxyLabel)), " ")
	if label == "" {
		label = strings.TrimSpace(row.ProxyID)
	}
	if label == "" {
		return ""
	}
	label = truncateRunes(redactUserinfo(label), 48)

	age := now.Unix() - row.At
	if row.At <= 0 || age < 0 {
		age = 0
	}
	clock := fmt.Sprintf("%d:%02d", age/60, age%60)

	prefix := "；该账号经代理「" + label + "」出网，最近一次隧道失败："
	suffix := "（" + clock + " 前）"
	budget := egressHintMaxRunes - runeLen(prefix) - runeLen(suffix)
	if budget < 8 {
		budget = 8
	}
	return prefix + truncateRunes(sanitizeTunnelError(row.Error), budget) + suffix
}

// tunnelUserinfoRE matches scheme://user:password@ so a tunnel error that
// echoed a proxy URL cannot leak credentials into a client-facing message.
var tunnelUserinfoRE = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/@\s]+@`)

func sanitizeTunnelError(raw string) string {
	s := strings.Join(strings.Fields(strings.TrimSpace(raw)), " ")
	s = redactUserinfo(s)
	if s == "" {
		return "连接失败"
	}
	return s
}

// redactUserinfo removes scheme://user:password@ from free-form tunnel text.
func redactUserinfo(s string) string {
	return tunnelUserinfoRE.ReplaceAllString(s, "$1***@")
}

// marshalJSON encodes without HTML escaping so URLs in the original message
// keep their literal "&" instead of becoming \u0026.
func marshalJSON(v any) ([]byte, error) {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return []byte(strings.TrimSuffix(buf.String(), "\n")), nil
}

// appendEgressHint appends clause to the human-readable message of a JSON
// error body and leaves every other byte-level field intact. Bodies that are
// not an object with error.message (or message) are returned unchanged, so the
// response shape never changes. The caller passes "" to keep the body as-is.
func appendEgressHint(body []byte, clause string) []byte {
	if clause == "" || len(body) == 0 {
		return body
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || top == nil {
		return body
	}
	if raw, ok := top["error"]; ok {
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(raw, &inner); err == nil && inner != nil {
			if msg, ok := rawJSONString(inner["message"]); ok {
				encoded, err := marshalJSON(withEgressClause(msg, clause))
				if err != nil {
					return body
				}
				inner["message"] = encoded
				encodedInner, err := marshalJSON(inner)
				if err != nil {
					return body
				}
				top["error"] = encodedInner
				if out, err := marshalJSON(top); err == nil {
					return out
				}
				return body
			}
		}
	}
	if msg, ok := rawJSONString(top["message"]); ok {
		if encoded, err := marshalJSON(withEgressClause(msg, clause)); err == nil {
			top["message"] = encoded
			if out, err := marshalJSON(top); err == nil {
				return out
			}
		}
	}
	return body
}

func rawJSONString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || strings.TrimSpace(s) == "" {
		return "", false
	}
	return s, true
}

func withEgressClause(message, clause string) string {
	if strings.Contains(message, "该账号经代理") {
		return message
	}
	return message + clause
}

func runeLen(s string) int { return len([]rune(s)) }

// truncateRunes keeps at most max runes, marking a cut with an ellipsis.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	return string(r[:max-1]) + "…"
}
