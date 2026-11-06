package app

// Model test (P12-1, decisions D-27/D-39): send one minimal chat request to a
// plugin model through the existing scheduling path and report a magpie-shaped
// result. The transport endpoint and the tier rules live in webui (see
// internal/webui/model_test.go); this file owns the dispatch, the tier choice,
// the otherAPI classification and the probe recording.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"aiworkgate/internal/model"
	"aiworkgate/internal/scheduler"
	"aiworkgate/internal/webui"
)

// D-39 tiers: light 20s, standard 60s, reasoning 2min with a streamed reply.
// max_tokens stays inside the decided 16..1024 band.
const (
	modelTestLightTimeout     = 20 * time.Second
	modelTestStandardTimeout  = 60 * time.Second
	modelTestReasoningTimeout = 2 * time.Minute

	modelTestLightMaxTokens     = 16
	modelTestStandardMaxTokens  = 256
	modelTestReasoningMaxTokens = 1024

	// modelTestTotalBudget bounds one whole test (plugin wake-up + model list +
	// the minimal request) so a wedged plugin cannot pin the admin request
	// forever; the per-request timeout is the tier's.
	modelTestTotalBudget = modelTestReasoningTimeout + 30*time.Second
	// modelTestBodyReadBytes caps how much of a reply is drained. The content is
	// irrelevant: the test only needs "did the model answer".
	modelTestBodyReadBytes = 64 << 10
	// modelTestErrorBodyBytes caps the error body kept for classification.
	modelTestErrorBodyBytes = 4 << 10
	modelTestBatchMaxItems  = 50
)

// modelTestSource is the plugin-manager slice the endpoint needs. Tests replace
// it so no plugin process has to run.
type modelTestSource interface {
	Get(id string) (model.PluginStatus, bool)
	EnsureRunning(ctx context.Context, id string) error
	ModelsFresh(ctx context.Context, id string) ([]model.ModelInfo, error)
	Endpoint(id string) (baseURL, token string, ok bool)
}

// Seams: production reads the app's plugin manager, scheduler and snapshot;
// tests inject fakes.
var (
	modelTestSourceFor = func(a *App) modelTestSource {
		if a == nil || a.pluginMgr == nil {
			return nil
		}
		return a.pluginMgr
	}
	modelTestSnapshotFor = func(a *App, ctx context.Context) (*scheduler.Snapshot, error) {
		return a.schedulerSnapshot(ctx)
	}
	modelTestHTTPClient = func() *http.Client {
		// Loopback plugin traffic: never route through an environment proxy,
		// exactly like the plugin client transport.
		return &http.Client{Transport: &http.Transport{Proxy: nil, IdleConnTimeout: 30 * time.Second}}
	}
	// modelTestRun is the per-model entry the batch endpoint fans out through.
	// Tests replace it to exercise provider isolation without a network.
	modelTestRun = func(a *App, provider, modelID string) webui.ModelTestResult {
		res, _ := a.modelTest(provider, modelID)
		return res
	}
)

// modelTestTier is the D-39 tier chosen for one model.
type modelTestTier struct {
	Timeout   time.Duration
	MaxTokens int
	Stream    bool
}

// selectModelTestTier picks the tier for one catalog entry. Reasoning models
// (declared thinking tiers or a telltale name) get the 2min streamed tier;
// small/free models get the 20s light tier; everything else is standard.
func selectModelTestTier(info model.ModelInfo) modelTestTier {
	if isReasoningModel(info) {
		return modelTestTier{Timeout: modelTestReasoningTimeout, MaxTokens: modelTestReasoningMaxTokens, Stream: true}
	}
	if isLightModel(info) {
		return modelTestTier{Timeout: modelTestLightTimeout, MaxTokens: modelTestLightMaxTokens}
	}
	return modelTestTier{Timeout: modelTestStandardTimeout, MaxTokens: modelTestStandardMaxTokens}
}

func isReasoningModel(info model.ModelInfo) bool {
	if len(info.Thinking) > 0 {
		return true
	}
	id := strings.ToLower(info.ID + " " + info.Name)
	for _, marker := range []string{"thinking", "think", "reason", "o1-", "-o1", "o3-", "-o3", "r1", "qwq", "deepseek-r"} {
		if strings.Contains(id, marker) {
			return true
		}
	}
	return false
}

func isLightModel(info model.ModelInfo) bool {
	if info.Free {
		return true
	}
	// A small context window is the catalog's own signal for a light model.
	if info.ContextLength > 0 && info.ContextLength <= 8192 {
		return true
	}
	id := strings.ToLower(info.ID + " " + info.Name)
	for _, marker := range []string{"mini", "flash", "nano", "lite", "air", "haiku", "small", "tiny"} {
		if strings.Contains(id, marker) {
			return true
		}
	}
	return false
}

// classifyOtherAPI maps an upstream failure onto "the vendor says this model
// does not live on this API" (D-27). Such a failure is not a dead model: the UI
// should suggest another endpoint/provider instead.
func classifyOtherAPI(status int, body string) bool {
	switch status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusNotImplemented:
	default:
		return false
	}
	lower := strings.ToLower(body)
	for _, marker := range []string{
		"model not found", "unknown model", "invalid model", "no such model", "unsupported model",
		"model does not exist", "does not exist in this api", "not available for this api",
		"not supported for this api", "not supported by this api", "not in this api",
		"模型不存在", "不在此 api", "不在该 api", "不支持该模型",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	// A 404 that names a model is the common "wrong endpoint for this model"
	// answer, even when the wording differs.
	return status == http.StatusNotFound && strings.Contains(lower, "model")
}

// modelTestErrorMessage extracts the readable reason from a plugin error body:
// the OpenAI-style error.message first, the raw text second.
func modelTestErrorMessage(status int, body []byte) string {
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err == nil {
		if msg := strings.TrimSpace(payload.Error.Message); msg != "" {
			return msg
		}
	}
	text := strings.TrimSpace(string(body))
	if runes := []rune(text); len(runes) > 200 {
		text = string(runes[:200]) + "…"
	}
	if text == "" {
		return http.StatusText(status)
	}
	return text
}

// modelTest runs one minimal chat request for provider/model and returns the
// magpie-shaped result. Business failures (model missing, plugin not logged in,
// upstream errors) are reported as ok=false with a redacted reason and a nil
// error; a non-nil error means the test itself could not be attempted.
func (a *App) modelTest(provider, modelID string) (webui.ModelTestResult, error) {
	provider = strings.TrimSpace(provider)
	modelID = strings.TrimSpace(modelID)
	pluginID, wanted := splitPluginModel(provider, modelID)
	result := webui.ModelTestResult{Model: pluginID + "/" + wanted}
	if pluginID == "" || wanted == "" {
		result.Error = "provider 和 model 不能为空"
		return result, nil
	}
	start := time.Now()
	fail := func(status int, text, errorCode string) (webui.ModelTestResult, error) {
		result.OK = false
		result.Status = status
		result.MS = time.Since(start).Milliseconds()
		result.Error = redactedProbeDetail(text)
		a.recordModelTestProbe(pluginID, wanted, result, errorCode)
		return result, nil
	}

	source := modelTestSourceFor(a)
	if source == nil {
		return fail(0, "插件运行时不可用", "not_configured")
	}
	status, found := source.Get(pluginID)
	if !found {
		return fail(0, "插件未安装: "+pluginID, "not_configured")
	}
	if !status.Enabled {
		return fail(0, "插件已停用: "+pluginID, "not_configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), modelTestTotalBudget)
	defer cancel()
	if err := source.EnsureRunning(ctx, pluginID); err != nil {
		code := "unreachable"
		if errors.Is(err, context.DeadlineExceeded) || isTimeoutText(err.Error()) {
			code = "timeout"
		}
		return fail(0, "插件未就绪: "+err.Error(), code)
	}
	models, err := source.ModelsFresh(ctx, pluginID)
	if err != nil {
		code, status := probeErrorCode(err, "upstream_error")
		return fail(status, "获取模型列表失败: "+err.Error(), code)
	}
	info, foundModel := findModel(models, wanted)
	if !foundModel {
		return fail(http.StatusNotFound, "模型不存在: "+pluginID+"/"+wanted, "model_missing")
	}
	tier := selectModelTestTier(info)

	// Existing scheduling path: when the plugin is a schedulable pool with
	// accounts, ask the scheduler which account should serve this model. A pool
	// without accounts (api_level 1 / upstream-only) is dialled directly, which
	// is exactly the gateway's pluginWithoutAccounts rule.
	bodyModel := wanted
	pinned := ""
	if snap, snapErr := modelTestSnapshotFor(a, ctx); snapErr == nil && snap != nil && a.dispatch != nil {
		if pool, ok := snap.Pools[pluginID]; ok && pool != nil && pool.UpstreamEnabled && len(pool.Accounts) > 0 {
			decision, pickErr := a.dispatch.Pick(ctx, snap, scheduler.Request{Model: pluginID + "/" + wanted})
			if pickErr != nil {
				return fail(0, "调度失败: "+pickErr.Error(), "unreachable")
			}
			pinned = strings.TrimSpace(decision.AccountID)
			if m := strings.TrimSpace(decision.Model); m != "" {
				bodyModel = m
			}
		}
	}

	base, token, ok := source.Endpoint(pluginID)
	if !ok || strings.TrimSpace(base) == "" {
		return fail(0, "插件未运行或端口不可用", "unreachable")
	}

	payload := map[string]any{
		"model":      bodyModel,
		"messages":   []map[string]any{{"role": "user", "content": "ping"}},
		"max_tokens": tier.MaxTokens,
	}
	if tier.Stream {
		payload["stream"] = true
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return webui.ModelTestResult{}, fmt.Errorf("app: encode model test body: %w", err)
	}

	reqCtx, reqCancel := context.WithTimeout(ctx, tier.Timeout)
	defer reqCancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		strings.TrimRight(base, "/")+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return webui.ModelTestResult{}, fmt.Errorf("app: build model test request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Aiworkgate-Request-Id", fmt.Sprintf("model-test-%d", time.Now().UnixNano()))
	if tier.Stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	if pinned != "" {
		req.Header.Set("X-Aiworkgate-Account", pinned)
	}

	client := modelTestHTTPClient()
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	result.MS = time.Since(start).Milliseconds()
	if err != nil {
		code := "unreachable"
		text := "连接插件失败: " + err.Error()
		if errors.Is(err, context.DeadlineExceeded) || isTimeoutText(err.Error()) {
			code = "timeout"
			text = fmt.Sprintf("模型测试超时（%s）", tier.Timeout)
		}
		return fail(0, text, code)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, modelTestErrorBodyBytes))
		message := modelTestErrorMessage(resp.StatusCode, body)
		otherAPI := classifyOtherAPI(resp.StatusCode, string(body))
		text := fmt.Sprintf("插件返回 %d: %s", resp.StatusCode, message)
		if otherAPI {
			text = fmt.Sprintf("该模型不在此 API（插件返回 %d: %s），请换端点/供应商再试", resp.StatusCode, message)
		}
		result.OtherAPI = otherAPI
		return fail(resp.StatusCode, text, "upstream_error")
	}

	// The reply content is irrelevant: reading a bounded slice proves the model
	// answered (a streamed reasoning reply has bytes before the first frame).
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, modelTestBodyReadBytes))
	result.OK = true
	result.Status = resp.StatusCode
	a.recordModelTestProbe(pluginID, wanted, result, "")
	return result, nil
}

// findModel matches a bare model id (a "<plugin>/<id>" form has already been
// stripped by splitPluginModel).
func findModel(models []model.ModelInfo, wanted string) (model.ModelInfo, bool) {
	for _, entry := range models {
		if entry.ID == wanted {
			return entry, true
		}
	}
	return model.ModelInfo{}, false
}

// recordModelTestProbe writes the outcome into the existing diagnostics probes
// table under the model_test kind. A missing store simply skips the row.
func (a *App) recordModelTestProbe(pluginID, modelID string, res webui.ModelTestResult, errorCode string) {
	if a == nil || a.store == nil {
		return
	}
	detail := res.Error
	if detail == "" {
		detail = fmt.Sprintf("模型测试通过 · %d ms", res.MS)
	}
	a.recordConnectivityProbe(webui.ProbeKindModelTest, connectivityOutcome{
		target:    pluginID,
		targetID:  pluginID + "/" + modelID,
		ok:        res.OK,
		status:    res.Status,
		latencyMS: res.MS,
		detail:    detail,
		errorCode: errorCode,
	}, time.Now().Unix())
}

// modelTestBatch tests every item with per-provider parallelism: items of the
// same provider run sequentially (one plugin, one account pool at a time), and
// providers run concurrently. Results keep the request order and one provider
// failing (or panicking) never affects the others. It never returns an error:
// per-model failures are part of the results, mirroring the single-model test.
func (a *App) modelTestBatch(items []webui.ModelTestItem) ([]webui.ModelTestResult, error) {
	if len(items) > modelTestBatchMaxItems {
		items = items[:modelTestBatchMaxItems]
	}
	results := make([]webui.ModelTestResult, len(items))

	groups := make(map[string][]int, len(items))
	order := make([]string, 0, len(items))
	for i, item := range items {
		provider := strings.TrimSpace(item.Provider)
		if _, seen := groups[provider]; !seen {
			order = append(order, provider)
		}
		groups[provider] = append(groups[provider], i)
	}

	var wg sync.WaitGroup
	for _, provider := range order {
		indexes := groups[provider]
		wg.Add(1)
		go func(indexes []int) {
			defer wg.Done()
			defer func() {
				// One provider panicking must not take the batch down.
				if r := recover(); r != nil {
					for _, i := range indexes {
						if results[i].Model == "" {
							results[i] = webui.ModelTestResult{
								Model: strings.TrimSpace(items[i].Provider) + "/" + strings.TrimSpace(items[i].Model),
								Error: "内部错误",
							}
						}
					}
				}
			}()
			for _, i := range indexes {
				results[i] = modelTestRun(a, strings.TrimSpace(items[i].Provider), strings.TrimSpace(items[i].Model))
			}
		}(indexes)
	}
	wg.Wait()
	return results, nil
}
