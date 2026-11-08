package gateway

import (
	"encoding/json"
	"testing"
)

func ctxSectionOf(t *testing.T, comp CtxComposition, key string) CtxSection {
	t.Helper()
	for _, section := range comp.Sections {
		if section.Key == key {
			return section
		}
	}
	t.Fatalf("bucket %q missing from %+v", key, comp.Sections)
	return CtxSection{}
}

func TestAnalyzeCtxOpenAISixBuckets(t *testing.T) {
	body := `{
		"model": "gpt-4o",
		"messages": [
			{"role": "system", "content": "You are a helpful assistant"},
			{"role": "user", "content": "hello"},
			{"role": "assistant", "content": "hi", "tool_calls": [{"type": "function", "function": {"name": "look", "arguments": "{\"q\":1}"}}]},
			{"role": "tool", "content": "search result payload"}
		],
		"tools": [{"type": "function", "function": {"name": "look", "description": "d"}}],
		"memory": {"note": "m"},
		"temperature": 0.2
	}`
	comp, err := AnalyzeCtxComposition([]byte(body))
	if err != nil {
		t.Fatalf("AnalyzeCtxComposition: %v", err)
	}
	if ctxSectionOf(t, comp, "system").Est == 0 {
		t.Fatal("system = 0")
	}
	if ctxSectionOf(t, comp, "tools").Est == 0 {
		t.Fatal("tools = 0")
	}
	if ctxSectionOf(t, comp, "tool_results").Est == 0 {
		t.Fatal("tool_results = 0, want role=tool messages in their own bucket")
	}
	if ctxSectionOf(t, comp, "conversation").Est == 0 {
		t.Fatal("conversation = 0")
	}
	if ctxSectionOf(t, comp, "memory").Est == 0 {
		t.Fatal("memory = 0")
	}
	if ctxSectionOf(t, comp, "other").Est == 0 {
		t.Fatal("other = 0, want model/temperature")
	}
	// 条级行：tools 一条 + system + user + assistant(+calls) + tool_result。
	byBucket := map[string]int{}
	for _, item := range comp.Items {
		byBucket[item.Bucket]++
	}
	if byBucket["tools"] != 1 || byBucket["tool_results"] != 1 {
		t.Fatalf("items = %+v, want 1 tool row and 1 tool_result row", comp.Items)
	}
	// 求和守恒：六桶 est 合计 == TotalEst。
	sum := 0
	for _, section := range comp.Sections {
		sum += section.Est
	}
	if sum != comp.TotalEst {
		t.Fatalf("sections sum = %d, total_est = %d", sum, comp.TotalEst)
	}
}

func TestAnalyzeCtxAnthropicToolResultBlocks(t *testing.T) {
	body := `{
		"model": "claude",
		"system": "be brief",
		"messages": [
			{"role": "user", "content": "run it"},
			{"role": "assistant", "content": [{"type": "tool_use", "name": "search", "input": {"q": "x"}}]},
			{"role": "user", "content": [
				{"type": "tool_result", "content": "the search found 3 items"},
				{"type": "text", "text": "now summarize"}
			]}
		],
		"tools": [{"name": "search", "input_schema": {"type": "object"}}]
	}`
	comp, err := AnalyzeCtxComposition([]byte(body))
	if err != nil {
		t.Fatalf("AnalyzeCtxComposition: %v", err)
	}
	// Anthropic 的 tool_result 块进独立桶，同一消息的 text 块留 conversation。
	if ctxSectionOf(t, comp, "tool_results").Est == 0 {
		t.Fatal("tool_results = 0, want the tool_result block in its own bucket")
	}
	// system 顶层字段归 system；第二条 user 的 text 块与 tool_use 轮归 conversation。
	found := map[string]bool{}
	for _, item := range comp.Items {
		found[item.Bucket] = true
	}
	if !found["conversation"] {
		t.Fatalf("items = %+v, want conversation rows", comp.Items)
	}
}

func TestAnalyzeCtxResponsesFunctionCallOutput(t *testing.T) {
	body := `{
		"model": "gpt-5",
		"instructions": "be terse",
		"input": [
			{"type": "message", "role": "user", "content": "what is 1+1"},
			{"type": "function_call", "name": "calc", "arguments": "{}"},
			{"type": "function_call_output", "call_id": "c1", "output": "2"}
		],
		"tools": [{"type": "function", "name": "calc"}]
	}`
	comp, err := AnalyzeCtxComposition([]byte(body))
	if err != nil {
		t.Fatalf("AnalyzeCtxComposition: %v", err)
	}
	if ctxSectionOf(t, comp, "system").Est == 0 {
		t.Fatal("system = 0, want instructions")
	}
	if ctxSectionOf(t, comp, "tool_results").Est == 0 {
		t.Fatal("tool_results = 0, want function_call_output items")
	}
	if ctxSectionOf(t, comp, "conversation").Est == 0 {
		t.Fatal("conversation = 0")
	}
}

func TestAnalyzeCtxEmptyBodyZeroTotal(t *testing.T) {
	comp, err := AnalyzeCtxComposition([]byte(`{}`))
	if err != nil {
		t.Fatalf("AnalyzeCtxComposition: %v", err)
	}
	if comp.TotalEst != 0 || len(comp.Items) != 0 {
		t.Fatalf("empty body composition = %+v", comp)
	}
}

func TestAnalyzeCtxRejectsNonObject(t *testing.T) {
	if _, err := AnalyzeCtxComposition([]byte(`[1,2]`)); err == nil {
		t.Fatal("array body accepted, want an error")
	}
}

func TestAllocatePromptTokensProportionalWithRemainder(t *testing.T) {
	comp := CtxComposition{
		TotalEst: 100,
		Sections: []CtxSection{
			{Key: "system", Est: 30},
			{Key: "tools", Est: 60},
			{Key: "conversation", Est: 10},
		},
	}
	sections := AllocatePromptTokens(comp, 1000)
	got := map[string]int{}
	total := 0
	for _, section := range sections {
		if section.Allocated == nil {
			t.Fatalf("bucket %s not allocated", section.Key)
		}
		got[section.Key] = *section.Allocated
		total += *section.Allocated
	}
	// 300 / 600 / 100 整除；总和精确还原上游口径。
	if got["system"] != 300 || got["tools"] != 600 || got["conversation"] != 100 || total != 1000 {
		t.Fatalf("allocations = %+v, want 300/600/100 summing to 1000", got)
	}
	// 非整除：余数落最大桶。
	sections = AllocatePromptTokens(comp, 1001)
	total = 0
	for _, section := range sections {
		total += *section.Allocated
	}
	if total != 1001 {
		t.Fatalf("remainder allocation sum = %d, want 1001", total)
	}
	if *sections[1].Allocated != 601 {
		t.Fatalf("largest bucket (tools) = %d, want 601 (remainder lands there)", *sections[1].Allocated)
	}
	// 无上游口径时 Allocated 缺省。
	sections = AllocatePromptTokens(comp, 0)
	for _, section := range sections {
		if section.Allocated != nil {
			t.Fatalf("allocation without upstream usage = %d, want nil", *section.Allocated)
		}
	}
}

func TestCtxSamplingGateOffSkipsAnalysis(t *testing.T) {
	g := &Gateway{}
	rec := &reqRecord{reqID: "r1"}
	SetCtxSamplingEnabled(false)
	defer SetCtxSamplingEnabled(false)
	out := g.sampleCtxComposition(rec, []byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	if rec.ctxComp != nil {
		t.Fatal("gate closed but the body was analyzed")
	}
	if string(out) != `{"messages":[{"role":"user","content":"hi"}]}` {
		t.Fatalf("body changed with the gate closed: %s", out)
	}
	// 开启后同一请求被采样。
	SetCtxSamplingEnabled(true)
	g.sampleCtxComposition(rec, []byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	if rec.ctxComp == nil || rec.ctxComp.TotalEst == 0 {
		t.Fatalf("gate open but no composition: %+v", rec.ctxComp)
	}
}

func TestCtxEmitCarriesAllocatedSections(t *testing.T) {
	rec := &reqRecord{reqID: "r2", prompt: 1000, usageKnown: true}
	comp, err := AnalyzeCtxComposition([]byte(`{"messages":[{"role":"user","content":"hello world this is a prompt"}]}`))
	if err != nil {
		t.Fatalf("AnalyzeCtxComposition: %v", err)
	}
	rec.ctxComp = &comp
	var got CtxEmit
	InstallCtxSink(func(e CtxEmit) { got = e })
	defer InstallCtxSink(nil)
	emitCtxComposition(rec, 12345)
	if got.ReqID != "r2" || got.TS != 12345 || got.UpstreamPrompt != 1000 {
		t.Fatalf("emit = %+v", got)
	}
	if got.Comp.Sections[0].Allocated == nil {
		t.Fatal("sections not allocated at emit time")
	}
	total := 0
	for _, section := range got.Comp.Sections {
		total += *section.Allocated
	}
	if total != 1000 {
		t.Fatalf("allocated sum = %d, want 1000", total)
	}
	encoded, err := json.Marshal(got.Comp.Sections)
	if err != nil || !json.Valid(encoded) {
		t.Fatalf("sections not JSON-encodable: %s", encoded)
	}
}
