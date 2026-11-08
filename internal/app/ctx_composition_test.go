package app

import (
	"context"
	"encoding/json"
	"testing"

	"aiworkgate/internal/store"
)

func TestCtxCompositionPayloadSampled(t *testing.T) {
	app, st := newSchedulerApp(t)
	// 直接落一行采样（gateway 侧的 emit→sink 链有独立测试），这里验端点组装。
	if err := st.SaveCtxComposition(context.Background(), store.CtxCompositionRecord{
		ReqID:          "req-ctx-1",
		TS:             1790000000,
		TotalEst:       70,
		Sections:       json.RawMessage(`[{"key":"conversation","est":70,"share":1,"allocated":700}]`),
		UpstreamPrompt: 700,
		UsageKnown:     true,
		SampledReason:  "sampling_enabled",
	}); err != nil {
		t.Fatalf("SaveCtxComposition: %v", err)
	}
	payload, err := app.ctxCompositionPayload("req-ctx-1")
	if err != nil {
		t.Fatalf("ctxCompositionPayload: %v", err)
	}
	result := payload.(map[string]any)
	if result["sampled"] != true || result["req_id"] != "req-ctx-1" {
		t.Fatalf("payload = %+v", result)
	}
	if result["upstream_prompt_tokens"] != int64(700) {
		t.Fatalf("upstream prompt = %v, want 700", result["upstream_prompt_tokens"])
	}
	encoded, err := json.Marshal(result["sections"])
	if err != nil || !json.Valid(encoded) {
		t.Fatalf("sections not marshalled: %s", encoded)
	}
}

func TestCtxCompositionPayloadNotSampled(t *testing.T) {
	app, _ := newSchedulerApp(t)
	payload, err := app.ctxCompositionPayload("never-sampled")
	if err != nil {
		t.Fatalf("ctxCompositionPayload: %v", err)
	}
	if payload != nil {
		t.Fatalf("unsampled request returned %+v, want nil", payload)
	}
	// 空白 id 同样走 not_sampled（端点层已先挡无参请求）。
	payload, err = app.ctxCompositionPayload("  ")
	if err != nil || payload != nil {
		t.Fatalf("blank id = (%+v, %v), want (nil, nil)", payload, err)
	}
}
