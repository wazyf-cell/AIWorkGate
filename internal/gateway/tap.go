package gateway

import (
	"bytes"
	"encoding/json"
)

// bodyTap observes response bytes as they are streamed to the client. It never
// buffers the whole stream:
//   - preview keeps the first previewCap bytes for debug logging and error
//     summaries;
//   - for SSE it parses "data:" lines incrementally to find usage;
//   - for plain JSON it keeps at most 1MB so the final usage object can be
//     parsed after the stream ends.
type bodyTap struct {
	preview []byte
	prevCap int

	sse      bool
	line     []byte
	lineCap  int
	lineOver bool

	usage     []byte
	usageCap  int
	usageOver bool

	prompt     int64
	completion int64
	// cached is the cached prompt prefix reported by the upstream. Ported from
	// A (TraeWorkAssistant, MIT, 朱天伟) usage.rs:121-124,227-228 (wide alias
	// extraction, never fabricated when the upstream omits usage).
	cached int64
}

func newBodyTap(previewCap int, sse bool) *bodyTap {
	if previewCap <= 0 {
		previewCap = defaultBodyCap
	}
	return &bodyTap{
		prevCap:  previewCap,
		sse:      sse,
		lineCap:  1 << 20,
		usageCap: 1 << 20,
	}
}

func (t *bodyTap) feed(p []byte) {
	if len(t.preview) < t.prevCap {
		n := t.prevCap - len(t.preview)
		if n > len(p) {
			n = len(p)
		}
		t.preview = append(t.preview, p[:n]...)
	}
	if t.sse {
		t.feedSSE(p)
		return
	}
	// Keep a rolling tail rather than the first usageCap bytes: a large
	// non-stream reply carries the usage object at its end, so a prefix-only
	// buffer records zero tokens for every response above the cap.
	t.usage = append(t.usage, p...)
	if len(t.usage) > t.usageCap {
		t.usage = append(t.usage[:0], t.usage[len(t.usage)-t.usageCap:]...)
		t.usageOver = true
	}
}

func (t *bodyTap) feedSSE(p []byte) {
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			t.appendLine(p)
			return
		}
		t.appendLine(p[:i])
		t.handleLine()
		p = p[i+1:]
	}
}

func (t *bodyTap) appendLine(p []byte) {
	if t.lineOver {
		return
	}
	if len(t.line)+len(p) > t.lineCap {
		t.lineOver = true
		t.line = t.line[:0]
		return
	}
	t.line = append(t.line, p...)
}

func (t *bodyTap) handleLine() {
	line := bytes.TrimRight(t.line, "\r")
	over := t.lineOver
	t.line = t.line[:0]
	t.lineOver = false
	if over {
		return
	}
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	data := bytes.TrimSpace(line[len("data:"):])
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return
	}
	if !bytes.Contains(data, []byte(`"usage"`)) &&
		!bytes.Contains(data, []byte(`"input_tokens"`)) &&
		!bytes.Contains(data, []byte(`"output_tokens"`)) {
		return
	}
	t.parseUsage(data)
}

func (t *bodyTap) finish() {
	if t.sse {
		if len(t.line) > 0 {
			t.handleLine()
		}
		return
	}
	if t.usageOver {
		t.parseUsageTail(t.usage)
		return
	}
	t.parseUsage(t.usage)
}

// parseUsageTail extracts the usage object from the tail window of a reply
// that outgrew the cap. The window no longer starts at a JSON boundary, so it
// brace-matches the object starting at the last "usage" key.
func (t *bodyTap) parseUsageTail(b []byte) {
	key := bytes.LastIndex(b, []byte(`"usage"`))
	if key < 0 {
		return
	}
	rest := b[key+len(`"usage"`):]
	start := bytes.IndexByte(rest, '{')
	if start < 0 {
		return
	}
	depth := 0
	inString, escaped := false, false
	for i := start; i < len(rest); i++ {
		switch c := rest[i]; {
		case inString:
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
		case c == '"':
			inString = true
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				wrapped := make([]byte, 0, len(rest[start:i+1])+10)
				wrapped = append(wrapped, `{"usage":`...)
				wrapped = append(wrapped, rest[start:i+1]...)
				wrapped = append(wrapped, '}')
				t.parseUsage(wrapped)
				return
			}
		}
	}
}

type usageCounts struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	// OpenAI reports the cached prefix under prompt_tokens_details.
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	// Anthropic reports the same idea as cache_read_input_tokens.
	CacheReadInputTokens int64 `json:"cache_read_input_tokens"`
}

func (u usageCounts) prompt() int64 {
	if u.PromptTokens > 0 {
		return u.PromptTokens
	}
	return u.InputTokens
}

func (u usageCounts) completion() int64 {
	if u.CompletionTokens > 0 {
		return u.CompletionTokens
	}
	return u.OutputTokens
}

// cachedPrompt returns the cached prefix in either dialect. Both fields
// describe the same tokens; if a proxy emits both, the larger value wins
// (union semantics) instead of summing, which would double count.
func (u usageCounts) cachedPrompt() int64 {
	var openai int64
	if u.PromptTokensDetails != nil {
		openai = u.PromptTokensDetails.CachedTokens
	}
	if u.CacheReadInputTokens > openai {
		return u.CacheReadInputTokens
	}
	return openai
}

func (t *bodyTap) parseUsage(b []byte) {
	var payload struct {
		Usage   usageCounts `json:"usage"`
		Message *struct {
			Usage usageCounts `json:"usage"`
		} `json:"message"`
		Response *struct {
			Usage usageCounts `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(b, &payload); err != nil {
		return
	}
	if payload.Message != nil {
		t.apply(payload.Message.Usage)
	}
	if payload.Response != nil {
		t.apply(payload.Response.Usage)
	}
	t.apply(payload.Usage)
}

func (t *bodyTap) apply(u usageCounts) {
	if p := u.prompt(); p > 0 {
		t.prompt = p
	}
	if c := u.completion(); c > 0 {
		t.completion = c
	}
	if c := u.cachedPrompt(); c > 0 {
		t.cached = c
	}
}
