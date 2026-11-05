package gateway

// redact_wiring 骨架的单元验证（不依赖既有文件改动，解锁前即可跑）：
// 入站替换+不过度脱、off 零改动、出站还原、审计无原文。

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWiringMaskJSONBodyOn(t *testing.T) {
	w := NewWiring(RedactOptions{On: true, RouteAware: true})
	body := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"` +
		`mail zoe@quill.dev key sk-ant-` + strings.Repeat("x1", 22) +
		` dial 13800138000 path /Users/jdoe/workspace/magpie color #1a2b3c "}]}`)
	out := w.MaskJSONBody(body, "trae-cn")
	if w.Masked() < 3 {
		t.Fatalf("masked=%d，期望 >=3（邮箱+key+手机）", w.Masked())
	}
	for _, gone := range []string{"zoe@quill.dev", "sk-ant-" + strings.Repeat("x1", 22), "13800138000"} {
		if strings.Contains(string(out), gone) {
			t.Errorf("明文泄漏 %q：%s", gone, out)
		}
	}
	for _, gone := range []string{"{{EMAIL_", "{{API_KEY_", "{{PHONE_"} {
		if !strings.Contains(string(out), gone) {
			t.Errorf("缺占位符 %q：%s", gone, out)
		}
	}
	// 不过度脱：路径/色值原样（example.com 假域名的「不该脱」由 redact 包内
	// 71 条 eval Keep 反例保障，这里不再重复）
	for _, kept := range []string{"/Users/jdoe/workspace/magpie", "#1a2b3c"} {
		if !strings.Contains(string(out), kept) {
			t.Errorf("误伤 %q：%s", kept, out)
		}
	}
	a := w.Audit()
	if a == nil || a.Total() < 3 {
		t.Fatalf("审计缺失：%v", a)
	}
}

func TestWiringMaskJSONBodyOff(t *testing.T) {
	w := NewWiring(RedactOptions{})
	body := []byte(`{"messages":[{"role":"user","content":"mail user@example.com"}]}`)
	out := w.MaskJSONBody(body, "trae-cn")
	if string(out) != string(body) {
		t.Fatalf("off 必须原样：%s", out)
	}
	if w.Masked() != 0 || w.Audit() != nil {
		t.Fatalf("off 不应产生统计：masked=%d", w.Masked())
	}
}

func TestWiringRespWriterRestores(t *testing.T) {
	w := NewWiring(RedactOptions{On: true})
	const email = "user@example.com"
	body := []byte(`{"content":"write to ` + email + ` and key ` + strings.Repeat("k", 30) + `"}`)
	w.MaskJSONBody(body, "trae-cn")

	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "application/json")
	out, finish := w.RespWriter(rec)
	_, _ = out.Write(w.MaskJSONBody(body, "trae-cn"))
	finish()

	got := rec.Body.String()
	if strings.Contains(got, "{{") {
		t.Fatalf("客户端看到占位符：%s", got)
	}
	if !strings.Contains(got, email) {
		t.Fatalf("回程未还原：%s", got)
	}
	if !strings.Contains(got, `"write to `) {
		t.Fatalf("结构损坏：%s", got)
	}
}

func TestWiringAuditLineNoOriginal(t *testing.T) {
	w := NewWiring(RedactOptions{On: true})
	const email = "zoe@quill.dev"
	key := "sk-ant-" + strings.Repeat("x1", 22)
	w.MaskJSONBody([]byte(`{"content":"`+email+` `+key+`"}`), "trae-cn")
	line := w.AuditLine()
	if line == "" {
		t.Fatal("审计行为空")
	}
	for _, forbidden := range []string{email, key, "quill", "sk-ant"} {
		if strings.Contains(line, forbidden) {
			t.Errorf("审计行泄漏 %q：%s", forbidden, line)
		}
	}
	if !strings.Contains(line, "redact masked:") || !strings.Contains(line, "elapsed=") {
		t.Errorf("审计行缺要素：%s", line)
	}
}