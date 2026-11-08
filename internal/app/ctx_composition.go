// 六维上下文采样组装（task-62）：gateway sink → store 独立表落库；端点回调
// 组装 {req_id, sampled, sections, items, …} 信封。落库失败只记日志，绝不
// 影响请求路径（emit 收束处的 sink 是旁路）。
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"aiworkgate/internal/config"
	"aiworkgate/internal/gateway"
	"aiworkgate/internal/model"
	"aiworkgate/internal/store"
)

// installCtxSink wires the gateway's per-request composition into the store.
// Called once from New; sampling itself is gated inside the gateway.
func (a *App) installCtxSink() {
	gateway.InstallCtxSink(func(e gateway.CtxEmit) {
		if a.store == nil || e.ReqID == "" {
			return
		}
		sections, err := json.Marshal(e.Comp.Sections)
		if err != nil {
			a.logger.Warn("app", "ctx composition marshal failed", map[string]any{
				"req_id": e.ReqID, "error": err.Error(),
			})
			return
		}
		var items json.RawMessage
		if len(e.Comp.Items) > 0 {
			if encoded, err := json.Marshal(e.Comp.Items); err == nil {
				items = encoded
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := a.store.SaveCtxComposition(ctx, store.CtxCompositionRecord{
			ReqID:          e.ReqID,
			TS:             e.TS,
			TotalEst:       e.Comp.TotalEst,
			Sections:       sections,
			Items:          items,
			UpstreamPrompt: e.UpstreamPrompt,
			UsageKnown:     e.UsageKnown,
			SampledReason:  e.Reason,
		}); err != nil {
			a.logger.Warn("app", "ctx composition save failed", map[string]any{
				"req_id": e.ReqID, "error": err.Error(),
			})
		}
	})
}

// ctxCompositionPayload implements GET /api/gateway/ctx?id=<req_id>：
// sampled=false（nil payload）表示该请求未被采样（门控关闭 / 解析失败 /
// 早于本功能的行）；sampled=true 时双口径（est 本地估算 + allocated 分摊）
// 与条级明细原样透出。
func (a *App) ctxCompositionPayload(reqID string) (any, error) {
	reqID = strings.TrimSpace(reqID)
	if a.store == nil || reqID == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec, ok, err := a.store.GetCtxComposition(ctx, reqID)
	if err != nil || !ok {
		return nil, nil
	}
	return map[string]any{
		"req_id":                 rec.ReqID,
		"sampled":                true,
		"ts":                     rec.TS,
		"total_est":              rec.TotalEst,
		"sections":               rec.Sections,
		"items":                  rec.Items,
		"upstream_prompt_tokens": rec.UpstreamPrompt,
		"usage_known":            rec.UsageKnown,
		"sampled_reason":         rec.SampledReason,
	}, nil
}

// applyCtxSamplingConfig pushes the persisted config switches into the
// gateway's process-level gate (task-67: sampling + phase-2 heuristics survive
// a restart). Called from New and after every settings save.
func (a *App) applyCtxSamplingConfig(cfg config.Config) {
	gateway.SetCtxSamplingEnabled(cfg.Gateway.CtxSampling)
	gateway.SetCtxHeuristicsEnabled(cfg.Gateway.CtxHeuristics)
}

// setCtxSamplingPersisted writes the sticky switches into config.json and
// applies them live. A nil heuristics keeps the stored value (the admin
// endpoint's POST {enabled} form). Failure returns an error to the endpoint
// without flipping the in-memory gate.
func (a *App) setCtxSamplingPersisted(enabled bool, heuristics *bool) error {
	next := a.configSnapshot()
	next.Gateway.CtxSampling = enabled
	if heuristics != nil {
		next.Gateway.CtxHeuristics = *heuristics
	}
	if err := config.Save(a.dataDir, next); err != nil {
		return err
	}
	a.cfgMu.Lock()
	a.cfg = next
	a.cfgMu.Unlock()
	gateway.SetCtxSamplingEnabled(next.Gateway.CtxSampling)
	gateway.SetCtxHeuristicsEnabled(next.Gateway.CtxHeuristics)
	a.writeAudit(model.AuditConfig, "ctx_sampling", "config.json",
		fmt.Sprintf("sampling=%v heuristics=%v", next.Gateway.CtxSampling, next.Gateway.CtxHeuristics))
	return nil
}

// ctxSamplingStatus reads the persisted/live switch pair for the admin endpoint.
func (a *App) ctxSamplingStatus() (bool, bool) {
	cfg := a.configSnapshot()
	return cfg.Gateway.CtxSampling, cfg.Gateway.CtxHeuristics
}
