package gateway

// P10-2 端到端冒烟（task-21 验收④）：起真实网关链路（httptest 上游 + fakeRegistry），
// 验证「入站邮箱/密钥被替换 → 上游收到 {{KIND_8位}} → 响应回程还原 → 客户端无脱敏指纹」。
//
// 主断言对象选用 API key（"sk-ant-"+hex）：还原到明文无歧义；邮箱参与入站替换断言
// （上游必须收到 {{EMAIL_…}}），保证「邮箱被替换」成立；邮箱回程还原语义由
// internal/redact 的 writer 单测（整包/半帧）覆盖。
//
// SSE 半帧：把上游回显的占位符从中间切断成两个事件，验证 writer 的 pending
// 持留跨事件完整还原，事件结构不增不减。

import (
	"bytes"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"aiworkgate/internal/model"
)

// key/email 是原值（明文形状）：sk-ant- 密钥与 alice@quill.dev 邮箱都会被规则脱成
// {{KIND_8位}}。注意不能用 {{KIND_××}} 占位符形做原值——规则视既有占位符为「已脱」而跳过。
const (
	key   = "sk-ant-" + "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678901"
	email = "alice@quill.dev"
)

// firstPlaceholderBytes 取上游 body 里第一个 {{KIND_指纹}}（模拟上游把已脱敏内容写回）。
func firstPlaceholderBytes(b []byte) []byte {
	i := bytes.Index(b, []byte("{{"))
	if i < 0 {
		return nil
	}
	j := bytes.Index(b[i+2:], []byte("}}"))
	if j < 0 {
		return nil
	}
	return b[i : i+j+4]
}

// firstEmailPlaceholderBytes 取上游 body 里第一个 {{EMAIL_指纹}}（邮箱半帧切断用）。
func firstEmailPlaceholderBytes(b []byte) []byte {
	i := bytes.Index(b, []byte("{{EMAIL_"))
	if i < 0 {
		return nil
	}
	j := bytes.Index(b[i+2:], []byte("}}"))
	if j < 0 {
		return nil
	}
	return b[i : i+j+4]
}

// firstKeyPlaceholderBytes 取上游 body 里第一个 {{API_KEY_指纹}}（密钥往返断言用）。
func firstKeyPlaceholderBytes(b []byte) []byte {
	i := bytes.Index(b, []byte("{{API_KEY_"))
	if i < 0 {
		return nil
	}
	j := bytes.Index(b[i+2:], []byte("}}"))
	if j < 0 {
		return nil
	}
	return b[i : i+j+4]
}

// placeholderShape 匹配脱敏写的占位符 {{KIND_8位}}。
var placeholderShape = regexp.MustCompile(`\{\{[A-Z_]+_[a-z2-7]{8}\}\}`)

func countPlaceholders(s string) int {
	return len(placeholderShape.FindAllString(s, -1))
}

// TestRedactEndToEndNonStream 非流式全链路。
func TestRedactEndToEndNonStream(t *testing.T) {
	body := &upBody{}
	reqCount := 0
	srv, _ := newUpstreamServer(t, func(w http.ResponseWriter, _ *http.Request, b []byte) {
		reqCount++
		appendBody(body)(b)
		// 上游把邮箱与密钥的占位符都写回（task-21 只回显第一个占位符，
		// 密钥从未进入响应，密钥往返断言因此形同虚设——task-24 修正）
		ph := firstPlaceholderBytes(b)
		kph := firstKeyPlaceholderBytes(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"got `+string(ph)+` and got `+string(kph)+`"}}]}`)
	})
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	g, _ := retryGateway(t, reg, singlePoolSnapshot("p1", 1, "acc1"), nil)
	g.redact = RedactOptions{On: true, RouteAware: true}

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/m1","messages":[{"role":"user","content":"write to `+email+` using `+key+` now"}]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	// ① 上游收到脱敏体：邮箱与密钥都换成 {{KIND_8位}}，原值一个都不在
	for _, gone := range []string{email, key} {
		if bytes.Contains(body.b, []byte(gone)) {
			t.Errorf("上游收到原文 %q：%s", gone, body.b)
		}
	}
	if !bytes.Contains(body.b, []byte("{{EMAIL_")) || !bytes.Contains(body.b, []byte("{{API_KEY_")) {
		t.Errorf("上游应收到邮箱与密钥的占位符：%s", body.b)
	}
	// ② 客户端：邮箱与密钥占位符都还原回原值（P10 验收原文的主对象）；无脱敏指纹残留
	got := rr.Body.String()
	t.Logf("upstream requests: %d", reqCount)
	t.Logf("upstream got: %s", body.b)
	t.Logf("client got: %s", got)
	if !strings.Contains(got, "got "+email) {
		t.Errorf("客户端未收到还原的邮箱：%s", got)
	}
	if !strings.Contains(got, "got "+key) {
		t.Errorf("客户端未收到还原的密钥（task-24 修复后必须为明文）：%s", got)
	}
	if countPlaceholders(got) != 0 {
		t.Errorf("客户端看到脱敏指纹：%s", got)
	}
}

// TestRedactEndToEndSSE 流式全链路 + 半帧切断。
func TestRedactEndToEndSSE(t *testing.T) {
	body := &upBody{}
	srv, _ := newUpstreamServer(t, func(w http.ResponseWriter, _ *http.Request, b []byte) {
		appendBody(body)(b)
		w.Header().Set("Content-Type", "text/event-stream")
		ph := firstEmailPlaceholderBytes(b) // 以邮箱占位符做半帧切断
		if len(ph) == 0 {
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		cut := len(ph) / 2 // 从占位符中间切断
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"mail `+string(ph[:cut])+`"}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"`+string(ph[cut:])+`"}}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	g, _ := retryGateway(t, reg, singlePoolSnapshot("p1", 1, "acc1"), nil)
	g.redact = RedactOptions{On: true, RouteAware: true}

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/m1","stream":true,"messages":[{"role":"user","content":"key `+key+` mail `+email+`"}]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	// ① 上游收到脱敏体
	if !bytes.Contains(body.b, []byte("{{API_KEY_")) || bytes.Contains(body.b, []byte(key)) {
		t.Errorf("上游应收到密钥的占位符：%s", body.b)
	}
	// ② 客户端：半帧切断的邮箱占位符被还原回原值（帧边界内）；事件结构不坏
	got := rr.Body.String()
	if !strings.Contains(got, "mail ") || !strings.Contains(got, email) {
		t.Fatalf("客户端未收到还原的邮箱（半帧未拼好？）：%s", got)
	}
	// 还原后不得残留密钥指纹（原值邮箱恰形似 EMAIL 占位符，故不按 EMAIL 计数判断）
	if strings.Contains(got, "{{API_KEY_") {
		t.Fatalf("客户端看到密钥脱敏指纹：%s", got)
	}
	if ev := strings.Count(got, "data: "); ev < 3 {
		t.Fatalf("事件结构被破坏（应有 2 内容帧 + [DONE]）：%s", got)
	}
}

// TestRedactEndToEndOff off 模式必须全链路原样（零行为变化）。
func TestRedactEndToEndOff(t *testing.T) {
	body := &upBody{}
	srv, _ := newUpstreamServer(t, func(w http.ResponseWriter, _ *http.Request, b []byte) {
		appendBody(body)(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	})
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	g, _ := retryGateway(t, reg, singlePoolSnapshot("p1", 1, "acc1"), nil)
	// g.redact 保持零值 = off

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/m1","messages":[{"role":"user","content":"mail `+email+` key `+key+`"}]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	if !bytes.Contains(body.b, []byte(key)) {
		t.Errorf("off 模式上游应收到原文：%s", body.b)
	}
	if !strings.Contains(rr.Body.String(), "ok") {
		t.Errorf("off 模式客户端输出不应变化：%s", rr.Body.String())
	}
}

// TestRedactEndToEndAuditLine 审计行进入请求日志（APIRequestLog.Redact），不含原文。
func TestRedactEndToEndAuditLine(t *testing.T) {
	var rows []model.APIRequestLog
	sink := func(row model.APIRequestLog) { rows = append(rows, row) }
	srv, _ := newUpstreamServer(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[]}`)
	})
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	g, _ := retryGateway(t, reg, singlePoolSnapshot("p1", 1, "acc1"), nil)
	g.redact = RedactOptions{On: true}
	g.logs = sinkFunc(sink)

	serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/m1","messages":[{"role":"user","content":"mail `+email+` key `+key+`"}]}`))
	if len(rows) == 0 {
		t.Fatal("没有产生日志行")
	}
	line := rows[0].Redact
	if !strings.Contains(line, "redact masked: API_KEY=") {
		t.Errorf("审计行缺类别/条数：%q", line)
	}
	if strings.Contains(line, key) || strings.Contains(line, email) || strings.Contains(line, "zoe") {
		t.Errorf("审计行含原文：%q", line)
	}
}

// sinkFunc 适配 APILogSink。
type sinkFunc func(model.APIRequestLog)

func (f sinkFunc) Write(row model.APIRequestLog) { f(row) }

// ---- 辅助 -----------------------------------------------------------------

// upBody 捕获上游实际收到的请求体。
type upBody struct{ b []byte }

func appendBody(dst *upBody) func([]byte) {
	return func(b []byte) { dst.b = b }
}