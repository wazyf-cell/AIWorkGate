package gateway

import (
	"strings"
	"testing"
)

// ctxBucketEst 返回某桶的 est（缺桶报错）。
func ctxBucketEst(t *testing.T, comp CtxComposition, key string) int {
	t.Helper()
	for _, section := range comp.Sections {
		if section.Key == key {
			return section.Est
		}
	}
	t.Fatalf("bucket %q missing from %+v", key, comp.Sections)
	return 0
}

const ctxHeuristicBody = `{
	"model": "claude",
	"messages": [
		{"role": "user", "content": "please fix F:\\Code\\app\\src\\server.go and also review /etc/nginx/nginx.conf"},
		{"role": "user", "content": "<memory>用户偏好简洁回复</memory> 记住这个"},
		{"role": "user", "content": "see https://example.com/docs/api.go for reference"}
	]
}`

func TestCtxHeuristicsOffMatchesPhase1(t *testing.T) {
	// 默认关：二期入口关掉启发式 = 一期逐桶一致（含桶清单与值）。
	phase1, err := AnalyzeCtxComposition([]byte(ctxHeuristicBody))
	if err != nil {
		t.Fatalf("phase-1: %v", err)
	}
	off, err := analyzeCtxComposition([]byte(ctxHeuristicBody), false)
	if err != nil {
		t.Fatalf("heuristics off: %v", err)
	}
	if len(phase1.Sections) != len(off.Sections) {
		t.Fatalf("section count %d != %d", len(phase1.Sections), len(off.Sections))
	}
	for i := range phase1.Sections {
		if phase1.Sections[i].Key != off.Sections[i].Key || phase1.Sections[i].Est != off.Sections[i].Est {
			t.Fatalf("section %d = %+v vs %+v", i, phase1.Sections[i], off.Sections[i])
		}
	}
	// 「文件」桶不存在于默认输出。
	for _, section := range phase1.Sections {
		if section.Key == "files" {
			t.Fatal("files bucket present with heuristics off")
		}
	}
}

func TestCtxHeuristicsOnSplitsFilesAndMemory(t *testing.T) {
	comp, err := AnalyzeCtxCompositionHeuristics([]byte(ctxHeuristicBody))
	if err != nil {
		t.Fatalf("heuristics on: %v", err)
	}
	if ctxBucketEst(t, comp, "files") <= 0 {
		t.Fatal("files = 0, want the two paths detected")
	}
	if ctxBucketEst(t, comp, "memory") <= 0 {
		t.Fatal("memory = 0, want the <memory> block detected")
	}
	// 七桶固定序（files 在 tool_results 与 conversation 之间）。
	order := ""
	for _, section := range comp.Sections {
		order += section.Key + ","
	}
	if !strings.Contains(order, "tool_results,files,conversation") {
		t.Fatalf("bucket order = %s, want files between tool_results and conversation", order)
	}
	// token 守恒：启发式把 conversation 的一部分切进 files/memory。每桶独立
	// Ceil 会带来 ±桶数 的取整损耗（一期分桶口径同款），不算凭空增减。
	phase1, _ := AnalyzeCtxComposition([]byte(ctxHeuristicBody))
	if delta := phase1.TotalEst - comp.TotalEst; delta < -2 || delta > 2 {
		t.Fatalf("total_est drifted with heuristics on: %d vs %d", phase1.TotalEst, comp.TotalEst)
	}
	// 条级行：files 桶的 label 是命中的路径串（最长者）。
	best := ""
	for _, item := range comp.Items {
		if item.Bucket != "files" {
			continue
		}
		if !strings.Contains(item.Label, "server.go") && !strings.Contains(item.Label, "nginx.conf") {
			t.Fatalf("files item label unexpected: %s", item.Label)
		}
		if len(item.Label) > len(best) {
			best = item.Label
		}
	}
	if best == "" {
		t.Fatal("no files drill-down row")
	}
	// URL 内部路径不得计入 files（example.com/docs/api.go 是链接的一部分）。
	for _, item := range comp.Items {
		if item.Bucket == "files" && strings.Contains(item.Label, "example.com") {
			t.Fatalf("URL internals leaked into files: %s", item.Label)
		}
	}
}

func TestCtxHeuristicsClaudeMdMarkerBlock(t *testing.T) {
	body := `{
		"model": "m",
		"messages": [
			{"role": "user", "content": "CLAUDE.md\n- 每次改动后跑 go test\n- 保持注释精炼"},
			{"role": "user", "content": "plain chat without markers"}
		]
	}`
	comp, err := AnalyzeCtxCompositionHeuristics([]byte(body))
	if err != nil {
		t.Fatalf("heuristics: %v", err)
	}
	if ctxBucketEst(t, comp, "memory") <= 0 {
		t.Fatal("memory = 0, want the CLAUDE.md marker block detected")
	}
	// 无标记的普通文本留在 conversation（漏归无伤语义）。
	if ctxBucketEst(t, comp, "conversation") <= 0 {
		t.Fatal("conversation = 0, want unmarked text to stay")
	}
}

func TestCtxSamplingHeuristicsGateFeedsComposition(t *testing.T) {
	g := &Gateway{}
	rec := &reqRecord{reqID: "r-heur"}
	SetCtxSamplingEnabled(true)
	SetCtxHeuristicsEnabled(true)
	defer func() {
		SetCtxSamplingEnabled(false)
		SetCtxHeuristicsEnabled(false)
	}()
	g.sampleCtxComposition(rec, []byte(ctxHeuristicBody))
	if rec.ctxComp == nil {
		t.Fatal("no composition sampled")
	}
	found := map[string]bool{}
	for _, section := range rec.ctxComp.Sections {
		found[section.Key] = true
	}
	if !found["files"] || !found["memory"] {
		t.Fatalf("sampled composition missing heuristic buckets: %+v", rec.ctxComp.Sections)
	}
	// 关启发式重采：七桶退回六桶（一期口径）。
	SetCtxHeuristicsEnabled(false)
	rec2 := &reqRecord{reqID: "r-heur-off"}
	g.sampleCtxComposition(rec2, []byte(ctxHeuristicBody))
	for _, section := range rec2.ctxComp.Sections {
		if section.Key == "files" {
			t.Fatal("files bucket sampled with heuristics off")
		}
	}
}

// task-74：代码围栏判据——围栏与前 1~2 行内的路径串强关联时整块归 files。
func TestCtxHeuristicsAssociatedFence(t *testing.T) {
	// raw string 装不下 ```：围栏段用拼接（\\n 是 JSON 的换行转义）。
	fence := "```go\\npackage main\\n\\nfunc main() {}\\n" + "```"
	body := `{"model":"m","messages":[{"role":"user","content":"please fix F:\\Code\\app\\src\\server.go:\n` +
		fence + `\ndone"}]}`
	comp, err := AnalyzeCtxCompositionHeuristics([]byte(body))
	if err != nil {
		t.Fatalf("heuristics: %v", err)
	}
	if ctxBucketEst(t, comp, "files") <= 0 {
		t.Fatal("files = 0, want the associated fence in the files bucket")
	}
	found := false
	for _, item := range comp.Items {
		if item.Bucket == "files" && strings.Contains(item.Label, "server.go") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no fence item labelled with the path: %+v", comp.Items)
	}
	// 闭栏后首行的路径也算强关联。
	fenceJSON := "```json\\n{\\\"a\\\":1}\\n" + "```"
	bodyAfter := `{"model":"m","messages":[{"role":"user","content":"` + fenceJSON + `\nsaved to /etc/app/config.json"}]}`
	comp2, err := AnalyzeCtxCompositionHeuristics([]byte(bodyAfter))
	if err != nil {
		t.Fatalf("heuristics after: %v", err)
	}
	if ctxBucketEst(t, comp2, "files") <= 0 {
		t.Fatal("files = 0, want the trailing-path association")
	}
}

// task-74：无关联的围栏不误归（宽泛猜测被强关联要求挡住）。
func TestCtxHeuristicsUnassociatedFenceStays(t *testing.T) {
	fence := "```python\\nprint(1)\\n" + "```"
	body := `{"model":"m","messages":[{"role":"user","content":"random chat:\n` + fence + `\nthat is all"}]}`
	comp, err := AnalyzeCtxCompositionHeuristics([]byte(body))
	if err != nil {
		t.Fatalf("heuristics: %v", err)
	}
	if ctxBucketEst(t, comp, "files") != 0 {
		t.Fatal("files > 0, want an unassociated fence to stay in conversation")
	}
}

// task-74：<file path="..."> 约定标签整块归 files，path 记入 item 名称。
func TestCtxHeuristicsFileTag(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"<file path=\"src/main.go\">package main</file> review it"}]}`
	comp, err := AnalyzeCtxCompositionHeuristics([]byte(body))
	if err != nil {
		t.Fatalf("heuristics: %v", err)
	}
	if ctxBucketEst(t, comp, "files") <= 0 {
		t.Fatal("files = 0, want the file tag block in the files bucket")
	}
	found := false
	for _, item := range comp.Items {
		if item.Bucket == "files" && strings.Contains(item.Label, "src/main.go") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no item labelled with the path attribute: %+v", comp.Items)
	}
}
