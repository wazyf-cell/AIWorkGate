package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"aiworkgate/internal/scheduler"
)

// task-72：intent 配置的 descriptions 接入 criteria——payload 断言描述生效、
// 缺省回落 label 名。
func TestIntentCriteriaUsesDescriptions(t *testing.T) {
	var jevCalls int64
	var payloadProbe map[string]any
	jevSrv, _ := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		atomic.AddInt64(&jevCalls, 1)
		_ = json.Unmarshal(body, &payloadProbe)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"answers":{"route":{"type":"choice","choice":"hard"}}}`)
	})
	snap := intentSnapshot(&scheduler.IntentBinding{
		Labels: []string{"simple", "hard"},
		Descriptions: map[string]string{
			"hard": "复杂技术设计、编程与推理任务",
		},
		Route: map[string]string{"simple": "grp", "hard": "strong"},
	}, "grp")
	g, _, _ := intentGateway(t, jevSrv.URL, snap)

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, intentBody("design a compiler")))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	questions, _ := payloadProbe["questions"].(map[string]any)
	route, _ := questions["route"].(map[string]any)
	criteria, _ := route["criteria"].(map[string]any)
	// 有描述的 label 用描述。
	if got := criteria["hard"]; got != "复杂技术设计、编程与推理任务" {
		t.Fatalf("criteria[hard] = %v, want the configured description", got)
	}
	// 无描述的 label 回落 label 名。
	if got := criteria["simple"]; got != "simple" {
		t.Fatalf("criteria[simple] = %v, want the label name fallback", got)
	}
}
