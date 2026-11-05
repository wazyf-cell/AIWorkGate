package redact

// P10 验收「SSE NewWriter 不破坏分片、半帧切断正确还原」：
// 占位符被两个流式分片从中间劈开时，Writer 必须持住前半、等后半到齐再整体还原，
// 事件结构不增不减、不留半个占位符。

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWriterHalfPlaceholder：{{EMAIL_xxxxxxxx}} 被从中间切成两个事件，跨 Write 调用。
func TestWriterHalfPlaceholder(t *testing.T) {
	const email = "jeremy.zhou@gmail.com"
	o := Options{Secrets: true, Personal: true}
	masked, n := Mask("write to "+email, o)
	if n != 1 {
		t.Fatalf("masked %d, want 1", n)
	}
	pw := placeholderRe.FindString(masked)
	if pw == "" || pw == email {
		t.Fatalf("占位符异常：%q", masked)
	}
	cut := 8 // 在类别段中间切：{{EMAIL_ | xxxxxxxx}}
	event := func(text string) string {
		b, _ := json.Marshal(map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": text},
		})
		return "event: content_block_delta\ndata: " + string(b) + "\n\n"
	}

	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "text/event-stream")
	w := NewWriter(rec)
	w.WriteHeader(200)
	w.Write([]byte(event("use " + pw[:cut]))) // 半帧：尾巴 {{EMA…
	w.Write([]byte(event(pw[cut:] + " now"))) // 后半 + 正常文本
	w.Finish()

	body := rec.Body.String()
	// 事件结构完整：仍是 2 个 event
	if got := strings.Count(body, "\n\n"); got != 2 {
		t.Fatalf("事件数变了：%d；body=%s", got, body)
	}
	// 没有半个占位符漏网（任何 {{ 都必须闭合）
	if i := strings.Index(body, "{{"); i >= 0 {
		if !strings.Contains(body[i:], "}}") {
			t.Fatalf("残留未闭合占位符：%s", body)
		}
	}
	// 拼回来的文本 = 原文（含真邮箱值），占位符被还原
	var got string
	for _, ev := range strings.Split(body, "\n\n") {
		_, d, ok := strings.Cut(ev, "data: ")
		if !ok || d == "[DONE]" {
			continue
		}
		var e struct {
			Delta struct {
				Text string
			}
		}
		if err := json.Unmarshal([]byte(d), &e); err != nil {
			t.Fatalf("事件 %q: %v", d, err)
		}
		got += e.Delta.Text
	}
	if got != "use "+email+" now" {
		t.Fatalf("拼回文本不对：%q", got)
	}
}

// TestWriterHalfPlaceholderThreePieces：三片切断（5/8/余）依然正确。
func TestWriterHalfPlaceholderThreePieces(t *testing.T) {
	const secret = "ghp_" + "0123456789abcdefghijABCDEFGHIJ0123"
	const text = "token is " + secret + "; keep going"
	masked, _ := Mask(text, Options{Secrets: true})
	pw := placeholderRe.FindString(masked)
	if pw == "" {
		t.Fatalf("无占位符：%q", masked)
	}

	event := func(s string) string {
		b, _ := json.Marshal(map[string]any{"t": s})
		return "data: " + string(b) + "\n\n"
	}
	parts := []string{pw[:5], pw[5:8], pw[8:] + ";"}
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "text/event-stream")
	w := NewWriter(rec)
	w.WriteHeader(200)
	// 用细碎字节写（跨 Write 边界任意切）
	all := ""
	for _, p := range parts {
		all += event(p)
	}
	for i := 0; i < len(all); i += 3 {
		_, _ = w.Write([]byte(all[i : i+3]))
	}
	w.Finish()

	body := rec.Body.String()
	if strings.Count(body, "\n\n") != 3 {
		t.Fatalf("事件数变了：%s", body)
	}
	var got string
	for _, ev := range strings.Split(body, "\n\n") {
		_, d, ok := strings.Cut(ev, "data: ")
		if !ok || d == "[DONE]" {
			continue
		}
		var e struct {
			T string
		}
		if err := json.Unmarshal([]byte(d), &e); err != nil {
			t.Fatalf("事件 %q: %v", d, err)
		}
		got += e.T
	}
	if got != secret+";" {
		t.Fatalf("拼回文本不对：%q", got)
	}
}
