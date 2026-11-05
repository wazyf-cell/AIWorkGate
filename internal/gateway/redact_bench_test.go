package gateway

// P10-2 验收：耗时实测（1MiB 请求体，脱敏增幅验收 <5ms）。
// 用 wiring 骨架的 MaskJSONBody（On）对 1MiB 混合体（邮箱+密钥+本机路径+代码上下文）
// 与「原样转发」基线对比；另直接量 redact.MaskJSON 以便对照。

import (
	"net/http"
	"strings"
	"testing"

	"aiworkgate/internal/redact"
)

// benchBody1MiB 构造约 1MiB 的 chat 请求体，含 2000 个邮箱/密钥/路径与大量
// 「不该脱」的代码上下文（行号/版本/色值），贴近真实转发负载。
func benchBody1MiB() []byte {
	const line = "model.go:212:9 color #1a2b3c v0.1.1513 1624x1487\n" +
		"mail user@example.com or dev@corp.internal; key sk-proj-H2u9JkLmN4PqR7sTvWxYzA1B3C5D6E7F8G9H0J1K2\n" +
		"db postgres://app:geodatafusion@127.0.0.1:5432/app path /Users/jdoe/workspace/magpie\n"
	var b strings.Builder
	// 3660 行 ≈ 1 MiB（3660 × 300 字节）
	for i := 0; i < 3660; i++ {
		b.WriteString(line)
	}
	return []byte(`{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":` +
		string(quoteString(b.String())) + `}]}`)
}

// quoteString 是最小 JSON 字符串转义（body 只含可打印 ASCII，仅转义引号/反斜杠）。
func quoteString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

func BenchmarkRedactMaskJSON1MiB(b *testing.B) {
	body := benchBody1MiB()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, n := redact.MaskJSON(body, redact.Options{Secrets: true, Personal: true})
		if n < 2000 {
			b.Fatalf("masked %d, want >=2000", n)
		}
		_ = out
	}
}

// BenchmarkWiringMask1MiBOn 走网关接线骨架（Route 感知，route=provider id）。
func BenchmarkWiringMask1MiBOn(b *testing.B) {
	body := benchBody1MiB()
	w := NewWiring(RedactOptions{On: true, RouteAware: true})
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := w.MaskJSONBody(body, "trae-cn")
		if w.Masked() < 2000 {
			b.Fatalf("masked %d, want >=2000", w.Masked())
		}
		_ = out
	}
}

// BenchmarkWiringMask256KBOn 是验收口径 b 的主基准：真实请求量级（≤256KB）<5ms。
func BenchmarkWiringMask128KBOn(b *testing.B) {
	full := benchBody1MiB()
	body := full[:128*1024]
	w := NewWiring(RedactOptions{On: true, RouteAware: true})
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := w.MaskJSONBody(body, "trae-cn")
		if w.Masked() < 200 {
			b.Fatalf("masked %d, want >=200", w.Masked())
		}
		_ = out
	}
}

func BenchmarkWiringMask256KBOn(b *testing.B) {
	full := benchBody1MiB()
	body := full[:256*1024]
	w := NewWiring(RedactOptions{On: true, RouteAware: true})
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := w.MaskJSONBody(body, "trae-cn")
		if w.Masked() < 400 {
			b.Fatalf("masked %d, want >=400", w.Masked())
		}
		_ = out
	}
}

// BenchmarkWiringMask1MiBOff 是关闭时的零开销基线（应接近纯拷贝）。
func BenchmarkWiringMask1MiBOff(b *testing.B) {
	body := benchBody1MiB()
	w := NewWiring(RedactOptions{On: false})
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := w.MaskJSONBody(body, "trae-cn")
		if len(out) != len(body) {
			b.Fatal("off 模式必须原样")
		}
	}
}

// BenchmarkWiringResp1MiB 是出站还原侧：1MiB SSE 帧流经 NewWriter（回程还原），
// 端到端耗时的一部分（入站 + 出站）。
func BenchmarkWiringResp1MiB(b *testing.B) {
	// 先做一次入站脱敏，得到占位符体
	body := benchBody1MiB()
	w := NewWiring(RedactOptions{On: true})
	masked := w.MaskJSONBody(body, "trae-cn")
	_ = masked
	b.SetBytes(int64(len(masked)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// 整包非 SSE 路径：NewWriter whole 模式 + Finish 还原
		rw := &nopResponseWriter{}
		out, finish := w.RespWriter(rw)
		_, _ = out.Write(masked)
		finish()
	}
}

// nopResponseWriter 只收字节，便于 bench（Header/WriteHeader 为空）。
type nopResponseWriter struct{ n int }

func (n *nopResponseWriter) Header() http.Header { return http.Header{} }
func (n *nopResponseWriter) Write(p []byte) (int, error) {
	n.n += len(p)
	return len(p), nil
}
func (n *nopResponseWriter) WriteHeader(int) {}