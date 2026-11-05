package redact

// P10 必做 ③④ 的验收：占位符 {{KIND_指纹}} 同值同占位、跨请求稳定；Route 感知。

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestPlaceholderStableAcrossRequests 验证指纹由 HMAC(kind+value) 决定：
// 同一值在多次请求（含 Forget 之后）占位符不变；不同值绝不同占位；可逆对账。
func TestPlaceholderStableAcrossRequests(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "placeholder.key")
	SetKeyPath(keyFile)
	t.Cleanup(func() { SetKeyPath("") })

	const secret = "sk-proj-" + "Qm7vT2xLk9RbW4nZpE6sJc8YhD3fGa5uKiN1oVr0"
	a, n1 := Mask("key is "+secret, Options{Secrets: true})
	b, n2 := Mask("again "+secret, Options{Secrets: true})
	if n1 != 1 || n2 != 1 {
		t.Fatalf("masked %d/%d", n1, n2)
	}
	pa, pb := placeholderRe.FindString(a), placeholderRe.FindString(b)
	if pa == "" || pa != pb {
		t.Fatalf("同值不同占位：%q %q", a, b)
	}

	// 跨请求稳定：清空记忆（Forget）后同一文本重新脱，占位不变（key 文件持久）
	Forget()
	c, _ := Mask("key is "+secret, Options{Secrets: true})
	if pc := placeholderRe.FindString(c); pc != pa {
		t.Fatalf("跨请求占位漂移：%q vs %q", pc, pa)
	}

	// 同值同占位、不同值不同占位（一值绝不两占、两值绝不一占）
	other := "sk-proj-" + strings.Repeat("x", 32)
	d, _ := Mask("other "+other, Options{Secrets: true})
	pd := placeholderRe.FindString(d)
	if pd == "" || pd == pa {
		t.Fatalf("不同值占位不应相同：%q %q", pd, pa)
	}

	// 可逆对账：Mask → Restore 还原原文
	e, _ := Mask("mix "+secret+" and "+other, Options{Secrets: true})
	if back := Restore(e, false); back != "mix "+secret+" and "+other {
		t.Fatalf("不可逆：%q -> %q", e, back)
	}
}

// TestMaskJSONStableAcrossRequests 同样的稳定性对 JSON 体成立。
func TestMaskJSONStableAcrossRequests(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "placeholder.key")
	SetKeyPath(keyFile)
	t.Cleanup(func() { SetKeyPath("") })
	const secret = "ghp_" + "0123456789abcdefghijABCDEFGHIJ0123"
	body := `{"messages":[{"role":"user","content":"token ` + secret + `"}]}`
	out1, _ := MaskJSON([]byte(body), Options{Secrets: true})
	out2, _ := MaskJSON([]byte(body), Options{Secrets: true})
	if string(out1) != string(out2) {
		t.Fatalf("JSON 脱敏不稳定：\n%s\n%s", out1, out2)
	}
	p1, p2 := placeholderRe.FindString(string(out1)), placeholderRe.FindString(string(out2))
	if p1 == "" || p1 != p2 {
		t.Fatalf("JSON 占位漂移：%q %q", p1, p2)
	}
}

// TestRouteAwareness 验证 Route 感知的 API 语义：
//
//	同一文本在同一 Route 上保持已发形态（两次 Mask 输出逐字节相同）；
//	换 Route 重新脱敏（sent 集合按 Route 分开），但占位符仍由值本身决定。
func TestRouteAwareness(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "placeholder.key")
	SetKeyPath(keyFile)
	t.Cleanup(func() { SetKeyPath("") })

	text := "mail me at zoe@example.dev and dial {{PHONE_ou2wvn36}}"
	oOpenAI := Options{Secrets: true, Personal: true, Route: "openai"}
	oAnthropic := Options{Secrets: true, Personal: true, Route: "anthropic"}

	a1, _ := Mask(text, oOpenAI)
	a2, _ := Mask(text, oOpenAI)
	if a1 != a2 {
		t.Fatalf("同一 Route 同一文本应保持已发形态：\n%s\n%s", a1, a2)
	}

	b, _ := Mask(text, oAnthropic)
	pa, pb := placeholderRe.FindString(a1), placeholderRe.FindString(b)
	if pa == "" || pa != pb {
		t.Fatalf("换 Route 后占位符不应变（占位由值决定）：%q vs %q", pa, pb)
	}
	if a1 == b {
		t.Logf("注意：两条 Route 输出恰好相同（同一批值、规则亦同）——只证明占位稳定；")
		t.Logf("Route 的 sent 集合差异见 known_test.go / sent_known_test.go 的既有用例。")
	}

	// 同 Route 连续两轮：第二轮复用第一轮的占位（回执给用户的验收语义）
	first, _ := Mask(text, oOpenAI)
	second, _ := Mask("after that, "+text, oOpenAI)
	if !strings.Contains(second, placeholderRe.FindString(first)) {
		t.Fatalf("第二轮没有复用第一轮的占位符：\n%s\n%s", first, second)
	}
}
