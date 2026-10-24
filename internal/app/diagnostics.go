package app

// Connectivity probes (Settings → 连通性自检). One user-triggered probe checks
// A's gateway, a plugin model or a stored custom model, persists the outcome
// (success or failure) and returns it to the admin UI.
//
// Safety rules: every upstream call is bounded (8s for A and plugins, the
// existing 5s for a custom model, redirects disabled), details are redacted of
// credentials and URL query strings before they are logged, persisted or
// returned, and a failed probe never becomes an HTTP error: the API answers
// 200 with ok=false plus an error_code (unreachable, timeout, model_missing,
// not_configured, upstream_error).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"aiworkgate/internal/gateway"
	"aiworkgate/internal/hosts"
	"aiworkgate/internal/model"
	"aiworkgate/internal/plugins"
	"aiworkgate/internal/store"
	"aiworkgate/internal/webui"
)

const (
	// connectivityProbeTimeout bounds one health/models operation.
	connectivityProbeTimeout = 8 * time.Second
	// connectivityProbeDetailRunes caps a stored detail so one chatty upstream
	// cannot flood the history table.
	connectivityProbeDetailRunes = 300
)

// connectivityOutcome is the host-side result before it is persisted and
// shaped into webui.ProbeResult.
type connectivityOutcome struct {
	target    string
	targetID  string
	ok        bool
	status    int
	latencyMS int64
	detail    string
	errorCode string
}

// pluginProbeSource is the subset of *plugins.Manager the probe needs. The
// interface keeps app tests free of spawned plugin processes; production uses
// the manager behind connectivityPluginSource.
type pluginProbeSource interface {
	Get(id string) (model.PluginStatus, bool)
	EnsureRunning(ctx context.Context, id string) error
	ModelsFresh(ctx context.Context, id string) ([]model.ModelInfo, error)
}

// connectivityPluginSource resolves the probe's view of the plugin runtime.
// Tests replace it; production reads a.pluginMgr.
var connectivityPluginSource = func(a *App) pluginProbeSource {
	if a == nil || a.pluginMgr == nil {
		return nil
	}
	return a.pluginMgr
}

// probeConnectivity runs one probe, persists the row and returns the admin
// response. Persistence failures are logged and leave probe_id at 0: the probe
// itself already happened and its result is still worth returning.
func (a *App) probeConnectivity(req webui.ProbeRequest) (webui.ProbeResult, error) {
	var outcome connectivityOutcome
	switch req.Kind {
	case webui.ProbeKindA:
		outcome = a.probeAConnectivity()
	case webui.ProbeKindPlugin:
		outcome = a.probePluginConnectivity(req.PluginID, req.Model)
	case webui.ProbeKindCustomModel:
		outcome = a.probeCustomModelConnectivity(req.CustomModelID)
	default:
		return webui.ProbeResult{}, fmt.Errorf("app: unknown probe kind %q", req.Kind)
	}

	at := time.Now().Unix()
	result := webui.ProbeResult{
		OK:        outcome.ok,
		Kind:      req.Kind,
		Target:    outcome.target,
		TargetID:  outcome.targetID,
		Status:    outcome.status,
		LatencyMS: outcome.latencyMS,
		Detail:    outcome.detail,
		ErrorCode: outcome.errorCode,
		At:        at,
	}
	result.ProbeID = a.recordConnectivityProbe(req.Kind, outcome, at)
	return result, nil
}

// recordConnectivityProbe stores one row and returns its id (0 when storage is
// unavailable or the insert failed).
func (a *App) recordConnectivityProbe(kind string, out connectivityOutcome, at int64) int64 {
	if a.store == nil {
		return 0
	}
	id, err := a.store.InsertConnectivityProbe(store.ConnectivityProbe{
		Kind:      kind,
		Target:    out.target,
		TargetID:  out.targetID,
		OK:        out.ok,
		Status:    out.status,
		LatencyMS: out.latencyMS,
		Detail:    out.detail,
		ErrorCode: out.errorCode,
		CreatedAt: at,
	})
	if err != nil {
		a.logger.Warn("app", "connectivity probe could not be stored", map[string]any{
			"kind": kind, "error": err.Error(),
		})
		return 0
	}
	return id
}

// listConnectivityProbes backs GET /api/diagnostics/probes. A host without a
// store returns an empty list rather than failing the page.
func (a *App) listConnectivityProbes(limit int, kind, targetID string) ([]store.ConnectivityProbe, error) {
	if a.store == nil {
		return []store.ConnectivityProbe{}, nil
	}
	return a.store.ListConnectivityProbes(limit, kind, targetID)
}

// ---------------------------------------------------------------- A

// probeAConnectivity follows A's live base URL through aEnsure (the same
// resolver as GET /api/status), then fetches /v1/models with A's key. ok
// requires health 200; latency covers both calls.
func (a *App) probeAConnectivity() connectivityOutcome {
	start := time.Now()
	out := connectivityOutcome{target: "a"}

	state, healthDetail, baseURL := a.aEnsure(true)
	out.targetID = hostPortOnly(baseURL)
	if strings.TrimSpace(baseURL) == "" {
		out.latencyMS = time.Since(start).Milliseconds()
		out.errorCode = "not_configured"
		out.detail = redactedProbeDetail(healthDetail)
		return out
	}
	if state != hosts.AStateOK {
		out.latencyMS = time.Since(start).Milliseconds()
		out.status = statusFromText(healthDetail)
		switch {
		case isTimeoutText(healthDetail):
			out.errorCode = "timeout"
		case state == hosts.AStateUnauthorized:
			out.errorCode = "upstream_error"
			if out.status == 0 {
				out.status = http.StatusUnauthorized
			}
		default:
			out.errorCode = "unreachable"
		}
		// Show an actionable Chinese hint, not the transport error; the raw
		// detail stays in the host log path that produced it.
		target := strings.TrimSpace(out.targetID)
		if target == "" {
			target = "A"
		}
		switch out.errorCode {
		case "timeout":
			out.detail = "A 响应超时（" + target + "）"
		case "upstream_error":
			out.detail = "A 拒绝访问，请检查 API Key（" + target + "）"
		default:
			out.detail = "A 未运行或端口不可达（" + target + "）"
		}
		return out
	}

	key := ""
	if a.secrets != nil {
		key, _ = a.secrets.GetAAPIKey()
	}
	client := hosts.NewClient(baseURL, key)
	client.HTTP.Timeout = connectivityProbeTimeout
	ctx, cancel := context.WithTimeout(context.Background(), connectivityProbeTimeout)
	defer cancel()
	models, err := client.Models(ctx)
	out.latencyMS = time.Since(start).Milliseconds()
	if err != nil {
		out.errorCode, out.status = probeErrorCode(err, "upstream_error")
		out.detail = redactedProbeDetail(err.Error())
		return out
	}
	out.ok = true
	out.status = http.StatusOK
	out.detail = fmt.Sprintf("A 运行中 · %d 个模型", len(models))
	return out
}

// hostPortOnly reduces a base URL to its host:port, dropping the scheme,
// userinfo, path and query. It tolerates a bare "host:port".
func hostPortOnly(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		parsed, err = url.Parse("http://" + raw)
		if err != nil {
			return ""
		}
	}
	return parsed.Host
}

// ---------------------------------------------------------------- plugin

// probePluginConnectivity wakes the plugin, lists its models and verifies the
// requested model. The bare model id and "<plugin>/<id>" are both accepted;
// latency covers the models fetch only.
func (a *App) probePluginConnectivity(pluginID, model string) connectivityOutcome {
	plugin, wanted := splitPluginModel(pluginID, model)
	out := connectivityOutcome{target: plugin, targetID: plugin}
	if wanted != "" {
		out.targetID = plugin + "/" + wanted
	}
	if plugin == "" || wanted == "" {
		out.errorCode = "not_configured"
		out.detail = "插件 id 和模型名不能为空"
		return out
	}

	source := connectivityPluginSource(a)
	if source == nil {
		out.errorCode = "not_configured"
		out.detail = "插件运行时不可用"
		return out
	}
	status, found := source.Get(plugin)
	if !found {
		out.errorCode = "not_configured"
		out.detail = fmt.Sprintf("插件 %s 未安装", plugin)
		return out
	}
	if !status.Enabled {
		out.errorCode = "not_configured"
		out.detail = fmt.Sprintf("插件 %s 已停用", plugin)
		return out
	}

	ensureCtx, ensureCancel := context.WithTimeout(context.Background(), connectivityProbeTimeout)
	err := source.EnsureRunning(ensureCtx, plugin)
	ensureCancel()
	if err != nil {
		switch {
		case errors.Is(err, plugins.ErrDisabled), errors.Is(err, plugins.ErrNotInstalled):
			out.errorCode = "not_configured"
		case errors.Is(err, context.DeadlineExceeded), isTimeoutText(err.Error()):
			out.errorCode = "timeout"
		default:
			out.errorCode = "unreachable"
		}
		out.detail = redactedProbeDetail("插件未就绪: " + err.Error())
		return out
	}

	start := time.Now()
	modelsCtx, modelsCancel := context.WithTimeout(context.Background(), connectivityProbeTimeout)
	models, err := source.ModelsFresh(modelsCtx, plugin)
	modelsCancel()
	out.latencyMS = time.Since(start).Milliseconds()
	if err != nil {
		out.errorCode, out.status = probeErrorCode(err, "upstream_error")
		out.detail = redactedProbeDetail("获取模型列表失败: " + err.Error())
		return out
	}
	out.status = http.StatusOK
	for _, entry := range models {
		if entry.ID == wanted {
			out.ok = true
			out.detail = fmt.Sprintf("模型存在 · %d 个模型", len(models))
			return out
		}
	}
	out.errorCode = "model_missing"
	out.detail = fmt.Sprintf("模型不存在 · %d 个模型", len(models))
	return out
}

// splitPluginModel trims both inputs and strips a "<plugin>/" prefix from the
// model id when it names the same plugin.
func splitPluginModel(pluginID, model string) (string, string) {
	pluginID = strings.TrimSpace(pluginID)
	model = strings.TrimSpace(model)
	if prefix, rest, ok := strings.Cut(model, "/"); ok {
		if strings.EqualFold(strings.TrimSpace(prefix), pluginID) {
			return pluginID, strings.TrimSpace(rest)
		}
	}
	return pluginID, model
}

// ---------------------------------------------------------------- custom model

// customModelProbeResult is the shared outcome of the explicit custom-model
// test. The legacy admin handler only reads latencyMS and err; the
// connectivity probe additionally uses status and errorCode.
type customModelProbeResult struct {
	row       store.CustomModel
	latencyMS int64
	status    int
	errorCode string
	err       error
}

// runCustomModelTest is the extracted body of the POST
// /api/custom-models/{id}/test probe. Messages and error values are unchanged
// so testCustomModel keeps its behaviour; redirects stay disabled and the
// response body is drained and discarded (it may repeat the upstream key).
func (a *App) runCustomModelTest(id string) customModelProbeResult {
	fail := func(code string, status int, latencyMS int64, err error) customModelProbeResult {
		return customModelProbeResult{latencyMS: latencyMS, status: status, errorCode: code, err: err}
	}
	if a.store == nil || a.secrets == nil {
		return fail("not_configured", 0, 0, errors.New("app: custom model services are not available"))
	}
	row, found, err := a.store.GetCustomModel(id)
	if err != nil {
		return fail("not_configured", 0, 0, err)
	}
	if !found {
		return fail("not_configured", 0, 0, store.ErrNotFound)
	}
	withRow := func(res customModelProbeResult) customModelProbeResult {
		res.row = row
		return res
	}

	keyID := row.ConnectionID
	if keyID == "" {
		keyID = row.ID
	}
	key, _ := a.customModelKey(keyID)
	if strings.TrimSpace(key) == "" {
		return withRow(fail("not_configured", 0, 0, errors.New("该自定义模型还没有配置 API Key")))
	}

	payload, err := customProbePayload(row.Protocol, row.UpstreamModel)
	if err != nil {
		return withRow(fail("upstream_error", 0, 0, err))
	}
	endpoint, err := gateway.CustomProtocolChatURL(row.Protocol, row.BaseURL, row.UpstreamModel, false)
	if err != nil {
		return withRow(fail("upstream_error", 0, 0, errors.New("自定义模型的 API 地址无效")))
	}
	ctx, cancel := context.WithTimeout(context.Background(), customModelTestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return withRow(fail("upstream_error", 0, 0, errors.New("自定义模型的 API 地址无效")))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for name, value := range gateway.CustomProtocolHeaders(row.Protocol, key) {
		req.Header.Set(name, value)
	}
	// The upstream is user-configured, so env proxies are deliberately ignored
	// (same policy as the gateway transport). Redirects are not followed: a 3xx
	// would replay a request that carries the stored credential to another host.
	client := newCustomAdminClient(customModelTestTimeout)
	defer client.CloseIdleConnections()

	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return withRow(fail("timeout", 0, latency,
				fmt.Errorf("自定义上游 %d 秒内未响应", int(customModelTestTimeout/time.Second))))
		}
		return withRow(fail("unreachable", 0, latency,
			errors.New("无法连接自定义上游（请检查 API 地址与网络）")))
	}
	defer resp.Body.Close()
	// Drain at most 1 KiB and discard it: the body may contain the upstream
	// credential. The request URL (never echoed either) may carry a query.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, customModelTestBodyBytes))
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return withRow(fail("upstream_error", resp.StatusCode, latency,
			fmt.Errorf("上游返回重定向状态 %d（已拒绝跟随）", resp.StatusCode)))
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return withRow(fail("upstream_error", resp.StatusCode, latency,
			fmt.Errorf("上游返回状态 %d，请检查 API 地址、模型名和 API Key", resp.StatusCode)))
	}
	return withRow(fail("", http.StatusOK, latency, nil))
}

// probeCustomModelConnectivity reuses the custom-model test path and maps its
// outcome onto the connectivity contract.
func (a *App) probeCustomModelConnectivity(id string) connectivityOutcome {
	id = strings.TrimSpace(id)
	out := connectivityOutcome{targetID: id}
	res := a.runCustomModelTest(id)
	out.latencyMS = res.latencyMS
	out.status = res.status
	if res.row.ID != "" {
		out.target = res.row.Name
		out.targetID = res.row.ID
	}
	if res.err != nil {
		out.errorCode = res.errorCode
		out.detail = redactedProbeDetail(res.err.Error())
		if errors.Is(res.err, store.ErrNotFound) {
			out.detail = "自定义模型不存在"
		}
		return out
	}
	out.ok = true
	out.detail = "模型连通正常"
	return out
}

// ---------------------------------------------------------------- helpers

// probeErrorCode classifies an upstream error into the contract's error_code
// and, when the upstream answered, its HTTP status.
func probeErrorCode(err error, fallback string) (string, int) {
	if err == nil {
		return "", 0
	}
	var pluginErr *plugins.HTTPError
	if errors.As(err, &pluginErr) {
		return fallback, pluginErr.Status
	}
	var hostErr *hosts.HTTPError
	if errors.As(err, &hostErr) {
		return fallback, hostErr.StatusCode
	}
	if errors.Is(err, context.DeadlineExceeded) || isTimeoutText(err.Error()) {
		return "timeout", 0
	}
	return fallback, 0
}

var (
	probeHTTPStatus = regexp.MustCompile(`\bHTTP (\d{3})\b`)
	probeQuery      = regexp.MustCompile(`\?[^\s"'<>]*`)
	probeUserinfo   = regexp.MustCompile(`://[^/@\s]+@`)
	probeSecret     = regexp.MustCompile(`(?i)(sk-[A-Za-z0-9_\-]{4,}|bearer\s+[^\s"',;]+)`)
)

// statusFromText extracts an HTTP status from an error string in the
// "hosts: GET /health: HTTP 401: ..." shape.
func statusFromText(text string) int {
	match := probeHTTPStatus.FindStringSubmatch(text)
	if len(match) != 2 {
		return 0
	}
	status, err := strconv.Atoi(match[1])
	if err != nil {
		return 0
	}
	return status
}

// isTimeoutText recognises transport timeouts that arrive as plain strings.
func isTimeoutText(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "deadline exceeded") ||
		strings.Contains(lower, "timeout") ||
		strings.Contains(lower, "超时")
}

// redactedProbeDetail removes URL query strings, userinfo and credential-like
// tokens from a detail, collapses whitespace and caps the length. It is the
// only path into a stored or returned detail.
func redactedProbeDetail(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	raw = probeQuery.ReplaceAllString(raw, "?<redacted>")
	raw = probeUserinfo.ReplaceAllString(raw, "://<redacted>@")
	raw = probeSecret.ReplaceAllStringFunc(raw, func(match string) string {
		if strings.HasPrefix(strings.ToLower(match), "bearer") {
			return "Bearer <redacted>"
		}
		return "sk-<redacted>"
	})
	raw = strings.Join(strings.Fields(raw), " ")
	runes := []rune(raw)
	if len(runes) > connectivityProbeDetailRunes {
		raw = string(runes[:connectivityProbeDetailRunes]) + "…"
	}
	return raw
}
