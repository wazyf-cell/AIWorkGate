package gateway

import (
	"strings"
	"testing"
)

func TestBodyTapParsesNonStreamUsage(t *testing.T) {
	cases := []struct {
		name  string
		parts []string
		wantP int64
		wantC int64
	}{
		{
			name:  "openai",
			parts: []string{`{"id":"x","usage":{"prompt_tokens":3,"completion_tokens":4}}`},
			wantP: 3, wantC: 4,
		},
		{
			name:  "openai_split",
			parts: []string{`{"id":"x","usa`, `ge":{"prompt_tokens":5,`, `"completion_tokens":6}}`},
			wantP: 5, wantC: 6,
		},
		{
			name:  "anthropic",
			parts: []string{`{"usage":{"input_tokens":7,"output_tokens":8}}`},
			wantP: 7, wantC: 8,
		},
		{
			name:  "nested_message",
			parts: []string{`{"message":{"usage":{"input_tokens":9,"output_tokens":10}}}`},
			wantP: 9, wantC: 10,
		},
		{
			name:  "no_usage",
			parts: []string{`{"id":"x","choices":[]}`},
			wantP: 0, wantC: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tap := newBodyTap(1024, false)
			for _, p := range tc.parts {
				tap.feed([]byte(p))
			}
			tap.finish()
			if tap.prompt != tc.wantP || tap.completion != tc.wantC {
				t.Fatalf("usage = %d/%d, want %d/%d", tap.prompt, tap.completion, tc.wantP, tc.wantC)
			}
		})
	}
}

func TestBodyTapParsesCachedTokensNonStream(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int64
	}{
		{
			// OpenAI口径: prompt_tokens_details.cached_tokens.
			name: "openai",
			body: `{"usage":{"prompt_tokens":20,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":12}}}`,
			want: 12,
		},
		{
			// Anthropic口径: cache_read_input_tokens（移植自 A usage.rs:121-124）。
			name: "anthropic",
			body: `{"usage":{"input_tokens":20,"output_tokens":5,"cache_read_input_tokens":9}}`,
			want: 9,
		},
		{
			// 两种口径同时出现时取较大值（并集近似，避免重复求和）。
			name: "both",
			body: `{"usage":{"prompt_tokens":20,"prompt_tokens_details":{"cached_tokens":12},"cache_read_input_tokens":7}}`,
			want: 12,
		},
		{
			name: "absent",
			body: `{"usage":{"prompt_tokens":20,"completion_tokens":5}}`,
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tap := newBodyTap(1024, false)
			tap.feed([]byte(tc.body))
			tap.finish()
			if tap.cached != tc.want {
				t.Fatalf("cached = %d, want %d", tap.cached, tc.want)
			}
		})
	}
}

func TestBodyTapParsesCachedTokensSSE(t *testing.T) {
	// Anthropic streaming puts usage in message_delta; the cache counter uses
	// the Anthropic spelling and must survive line splitting.
	tap := newBodyTap(4096, true)
	for _, p := range []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":0}}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":20,",
		"\"output_tokens\":5,\"cache_read_input_tokens\":14}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	} {
		tap.feed([]byte(p))
	}
	tap.finish()
	if tap.prompt != 20 || tap.completion != 5 || tap.cached != 14 {
		t.Fatalf("usage = %d/%d cached %d, want 20/5 cached 14", tap.prompt, tap.completion, tap.cached)
	}
}

func TestBodyTapParsesSSEUsageAcrossChunks(t *testing.T) {
	tap := newBodyTap(4096, true)
	parts := []string{
		"data: {\"choices\":[],\"usa",
		"ge\":{\"prompt_tokens\":2,\"completion_tokens\":3}}\r\n\r\n",
		"data: {\"choices\":[{\"delta\":{}}]}\n\n",
		"data: [DONE]\n\n",
	}
	for _, p := range parts {
		tap.feed([]byte(p))
	}
	tap.finish()
	if tap.prompt != 2 || tap.completion != 3 {
		t.Fatalf("usage = %d/%d, want 2/3", tap.prompt, tap.completion)
	}
	if got := string(tap.preview); got != strings.Join(parts, "") {
		t.Fatalf("preview = %q, want the full stream", got)
	}
}

func TestBodyTapSSEUsageWithoutTrailingNewline(t *testing.T) {
	tap := newBodyTap(4096, true)
	tap.feed([]byte(`data: {"usage":{"prompt_tokens":4,"completion_tokens":5}}`))
	if tap.prompt != 0 || tap.completion != 0 {
		t.Fatal("usage must not be parsed before finish")
	}
	tap.finish()
	if tap.prompt != 4 || tap.completion != 5 {
		t.Fatalf("usage = %d/%d, want 4/5", tap.prompt, tap.completion)
	}
}

func TestBodyTapPreviewCap(t *testing.T) {
	tap := newBodyTap(8, false)
	tap.feed([]byte("0123456789"))
	tap.feed([]byte("abcdef"))
	tap.finish()
	if got := string(tap.preview); got != "01234567" {
		t.Fatalf("preview = %q, want 01234567", got)
	}
}

func TestBodyTapSSEPreviewCap(t *testing.T) {
	tap := newBodyTap(5, true)
	tap.feed([]byte("data: [DONE]\n\n"))
	tap.finish()
	if got := string(tap.preview); got != "data:" {
		t.Fatalf("preview = %q, want data:", got)
	}
	if tap.prompt != 0 || tap.completion != 0 {
		t.Fatalf("unexpected usage: %d/%d", tap.prompt, tap.completion)
	}
}

// A non-stream reply larger than the usage cap still carries its usage object
// at the end; the tap must keep the tail instead of the first megabyte.
func TestBodyTapKeepsUsagePastTheCap(t *testing.T) {
	tap := newBodyTap(8, false)
	tap.feed([]byte(strings.Repeat("a", 1<<20+64)))
	tap.feed([]byte(`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,` +
		`"prompt_tokens_details":{"cached_tokens":2}}}`))
	tap.finish()
	if tap.prompt != 7 || tap.completion != 3 || tap.cached != 2 {
		t.Fatalf("usage = %d/%d cached %d, want 7/3 cached 2", tap.prompt, tap.completion, tap.cached)
	}
}
