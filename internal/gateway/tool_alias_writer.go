// D-32③ 姊妹篇 · 问题6A（task-43）：工具别名指纹清洗的网关接线。
//
// 正向（请求侧）：body 进上游前的唯一改写点与 redact MaskJSONBody 同位
// （forwardScheduled / forwardPlugin 两处），命中的 tools[].function.name /
// tool_choice / 历史 tool_calls 逐字改写为别名，逆映射记在 rec 上。
//
// 反向（响应侧）：forward() 的响应写循环是所有客户端协议面的必经点——
// OpenAI 客户端直收字节；/v1/messages、/v1beta gemini、/v1/responses 客户端
// 的适配器（anthropicWriter / geminiStreamWriter / envelopeWriter）也都从这
// 里消费 OpenAI 字节流再翻译。因此在这里包一个逆向回改 writer，四个协议面
// 一处覆盖：别名 → 原名的字节在适配器解码前已经还原，各面零改动。
// 未命中（rec.toolReverse == nil）时零开销直通。
//
// 热替换：SetToolAlias 用 atomic.Value（仿 SetCorsOrigins），保存即生效。

package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"aiworkgate/internal/gateway/tool_alias"
)

// toolAliasTableOf 原子读出生效表（nil = 停用）。
func (g *Gateway) toolAliasTable() *tool_alias.Table {
	if g == nil {
		return nil
	}
	if v, ok := g.toolAlias.Load().(*tool_alias.Table); ok {
		return v
	}
	return nil
}

// applyToolAliasForward 是请求侧钩子：命中改写并记逆映射，未命中原样返回。
func (g *Gateway) applyToolAliasForward(rec *reqRecord, body []byte) []byte {
	t := g.toolAliasTable()
	if t == nil {
		return body
	}
	out, reverse, changed := t.Forward(body)
	if changed && len(reverse) > 0 {
		rec.toolReverse = reverse
	}
	return out
}

// toolAliasWriter 把响应里的别名回改为客户端原名（字节流层面）。
// 三种模式（对齐 redact Writer 的 decide 口径）：
//
//	whole    — 非流式 JSON：缓冲整包，Finish 时解码回改重发；
//	stream   — SSE：逐 data: 行解码回改（OpenAI chunk 与 Anthropic 事件两种
//	           形态按帧嗅探），行外字节原样；
//	asIs     — 压缩/二进制响应不解码（别名只出现在文本协议里）。
type toolAliasWriter struct {
	w       http.ResponseWriter
	reverse map[string]string
	mode    int // 0 unknown, 1 stream, 2 whole, 3 asIs
	buf     bytes.Buffer
	// stream 模式的行缓冲：跨 Write 的不完整行留到下一片
	mu sync.Mutex
}

const (
	aliasUnknown = iota
	aliasStream
	aliasWhole
	aliasAsIs
)

func newToolAliasWriter(w http.ResponseWriter, reverse map[string]string) *toolAliasWriter {
	return &toolAliasWriter{w: w, reverse: reverse}
}

func (rw *toolAliasWriter) Header() http.Header { return rw.w.Header() }

func (rw *toolAliasWriter) decide() {
	h := rw.w.Header()
	switch {
	case h.Get("Content-Encoding") != "" && !strings.EqualFold(h.Get("Content-Encoding"), "identity"):
		rw.mode = aliasAsIs
	case strings.Contains(strings.ToLower(h.Get("Content-Type")), "event-stream"):
		rw.mode = aliasStream
	case binaryToolAliasType(h.Get("Content-Type")):
		rw.mode = aliasAsIs
	default:
		rw.mode = aliasWhole
	}
	if rw.mode != aliasAsIs {
		// 回改后的长度与原文不同（别名 ≠ 原名字节数）
		h.Del("Content-Length")
	}
}

// binaryToolAliasType 与 redact Writer 的 binaryType 同口径：别名只改文本协议。
func binaryToolAliasType(ct string) bool {
	ct = strings.ToLower(ct)
	for _, t := range []string{"audio/", "image/", "video/", "application/octet-stream", "application/pdf", "application/zip"} {
		if strings.HasPrefix(ct, t) {
			return true
		}
	}
	return false
}

func (rw *toolAliasWriter) WriteHeader(code int) {
	if rw.mode == aliasUnknown {
		rw.decide()
	}
	rw.w.WriteHeader(code)
}

func (rw *toolAliasWriter) Write(p []byte) (int, error) {
	if rw.mode == aliasUnknown {
		rw.decide()
	}
	switch rw.mode {
	case aliasAsIs:
		return rw.w.Write(p)
	case aliasWhole:
		rw.mu.Lock()
		defer rw.mu.Unlock()
		rw.buf.Write(p)
		return len(p), nil
	}
	rw.mu.Lock()
	defer rw.mu.Unlock()
	rw.buf.Write(p)
	// 逐完整行处理（SSE 的 data: JSON 名字必在单行内）；末尾不完整行留到下一片
	for {
		b := rw.buf.Bytes()
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			break
		}
		line := append([]byte(nil), b[:i+1]...)
		rw.buf.Next(i + 1)
		if _, err := rw.w.Write(rw.reverseLine(line)); err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}

// reverseLine 回改一行 SSE：data: {…} 解码后分别尝试 OpenAI chunk 与
// Anthropic 事件两种形态的回改（各自对不上的形状是空操作），命中重序列化；
// 其余行（event:、: comment、data: [DONE]、空行、非 JSON 帧）原样。
func (rw *toolAliasWriter) reverseLine(line []byte) []byte {
	// 拆出行尾换行（保持原有 \n / \r\n 形态）
	var body, ending []byte
	switch {
	case bytes.HasSuffix(line, []byte("\r\n")):
		body, ending = line[:len(line)-2], []byte("\r\n")
	case bytes.HasSuffix(line, []byte("\n")):
		body, ending = line[:len(line)-1], []byte("\n")
	default:
		body = line
	}
	trimmed := bytes.TrimSpace(body)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return line
	}
	payload := bytes.TrimSpace(trimmed[len("data:"):])
	if len(payload) == 0 || payload[0] != '{' {
		return line // [DONE] 等
	}
	var tree map[string]any
	if err := json.Unmarshal(payload, &tree); err != nil {
		return line // 非 JSON 帧（按上游原样）
	}
	changed := tool_alias.ReverseOpenAI(tree, rw.reverse)
	if tool_alias.ReverseAnthropic(tree, rw.reverse) {
		changed = true
	}
	if !changed {
		return line
	}
	out, err := json.Marshal(tree)
	if err != nil {
		return line
	}
	rewritten := append([]byte("data: "), out...)
	return append(rewritten, ending...)
}

// Finish 收尾：whole 模式在此回改整包并写出；stream 模式冲刷残留行。
func (rw *toolAliasWriter) Finish() {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	switch rw.mode {
	case aliasWhole:
		data := rw.buf.Bytes()
		if len(data) == 0 {
			return
		}
		var tree map[string]any
		if err := json.Unmarshal(data, &tree); err != nil {
			_, _ = rw.w.Write(data)
			return
		}
		changed := tool_alias.ReverseOpenAI(tree, rw.reverse)
		changed = tool_alias.ReverseAnthropic(tree, rw.reverse) || changed
		if !changed {
			_, _ = rw.w.Write(data)
			return
		}
		out, err := json.Marshal(tree)
		if err != nil {
			_, _ = rw.w.Write(data)
			return
		}
		_, _ = rw.w.Write(out)
	case aliasStream:
		if rw.buf.Len() > 0 {
			rest := append([]byte(nil), rw.buf.Bytes()...)
			rw.buf.Reset()
			_, _ = rw.w.Write(rw.reverseLine(rest))
		}
	}
}

// Unwrap 供 http.ResponseController 使用。
func (rw *toolAliasWriter) Unwrap() http.ResponseWriter { return rw.w }

func (rw *toolAliasWriter) Flush() {
	if f, ok := rw.w.(http.Flusher); ok && rw.mode != aliasWhole {
		f.Flush()
	}
}
