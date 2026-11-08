package store

import (
	"context"
	"encoding/json"
	"testing"
)

func TestCtxCompositionRoundtrip(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	sections := json.RawMessage(`[{"key":"system","est":30,"share":0.3,"allocated":300},{"key":"other","est":70,"share":0.7,"allocated":700}]`)
	items := json.RawMessage(`[{"bucket":"conversation","label":"user#0: hello","tokens":70}]`)
	if err := st.SaveCtxComposition(ctx, CtxCompositionRecord{
		ReqID: "req-1", TS: 1790000000, TotalEst: 100,
		Sections: sections, Items: items,
		UpstreamPrompt: 1000, UsageKnown: true, SampledReason: "sampling_enabled",
	}); err != nil {
		t.Fatalf("SaveCtxComposition: %v", err)
	}
	got, ok, err := st.GetCtxComposition(ctx, "req-1")
	if err != nil || !ok {
		t.Fatalf("GetCtxComposition = (%v, %v), want the saved row", got, ok)
	}
	if got.TotalEst != 100 || got.UpstreamPrompt != 1000 || !got.UsageKnown || got.SampledReason != "sampling_enabled" {
		t.Fatalf("record = %+v", got)
	}
	if len(got.Sections) == 0 || len(got.Items) == 0 {
		t.Fatalf("json blobs lost: sections=%s items=%s", got.Sections, got.Items)
	}
	// Idempotent replace on the same req_id.
	if err := st.SaveCtxComposition(ctx, CtxCompositionRecord{
		ReqID: "req-1", TS: 1790000001, TotalEst: 50, Sections: sections,
	}); err != nil {
		t.Fatalf("SaveCtxComposition replace: %v", err)
	}
	got, _, _ = st.GetCtxComposition(ctx, "req-1")
	if got.TotalEst != 50 || got.TS != 1790000001 {
		t.Fatalf("replace did not take: %+v", got)
	}
}

func TestCtxCompositionNotFound(t *testing.T) {
	st := openTestStore(t)
	rec, ok, err := st.GetCtxComposition(context.Background(), "never-sampled")
	if err != nil || ok {
		t.Fatalf("GetCtxComposition = (%+v, %v), want not-found without error", rec, ok)
	}
	if _, _, err := st.GetCtxComposition(context.Background(), ""); err == nil {
		t.Fatal("empty req_id accepted")
	}
	if err := st.SaveCtxComposition(context.Background(), CtxCompositionRecord{TS: 1}); err == nil {
		t.Fatal("empty req_id save accepted")
	}
}
