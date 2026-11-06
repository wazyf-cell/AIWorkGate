package main

// Folding and error-mapping guards for the chat surface.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConsumeStreamFoldsContentAndToolCalls(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"x1","model":"m","created":5,"choices":[{"delta":{"role":"assistant"}}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"think"}}]}`,
		`data: {"choices":[{"delta":{"content":"Hel"}}]}`,
		`data: {"choices":[{"delta":{"content":"lo"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":"{\"a\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`,
		`data: [DONE]`,
		``,
	}, "\n")
	c, err := consumeStream(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	out, err := c.json("m")
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	var got struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			Total int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Object != "chat.completion" {
		t.Fatalf("object = %q", got.Object)
	}
	msg := got.Choices[0].Message
	if msg.Content != "Hello" {
		t.Fatalf("content = %q", msg.Content)
	}
	if msg.ReasoningContent != "think" {
		t.Fatalf("reasoning = %q", msg.ReasoningContent)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Arguments != `{"a":1}` {
		t.Fatalf("tool calls = %+v", msg.ToolCalls)
	}
	if got.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish = %q, want tool_calls", got.Choices[0].FinishReason)
	}
	if got.Usage.Total != 6 {
		t.Fatalf("usage total = %d", got.Usage.Total)
	}
}

func TestClassifyUpstreamError(t *testing.T) {
	// A 402 means credits are gone: waiting does not help, and the advice must say so.
	status, kind, message := classifyUpstreamError(402, `{"error":{"message":"Insufficient credits"}}`)
	if status != 402 || kind != "quota" {
		t.Fatalf("402 -> %d/%q", status, kind)
	}
	if !strings.Contains(message, "app.cline.bot") {
		t.Fatalf("402 advice does not point at the top-up page: %q", message)
	}
	// A 429 is retryable, and free-tier exhaustion is the common cause.
	status, kind, _ = classifyUpstreamError(429, `{"error":{"message":"Too many requests"}}`)
	if status != 429 || kind != "rate_limit" {
		t.Fatalf("429 -> %d/%q", status, kind)
	}
	// Auth failures keep their status so the host can distinguish them.
	status, kind, _ = classifyUpstreamError(401, `unauthorized`)
	if status != 401 || kind != "auth" {
		t.Fatalf("401 -> %d/%q", status, kind)
	}
	status, kind, _ = classifyUpstreamError(500, `boom`)
	if status != 500 || kind != "server" {
		t.Fatalf("500 -> %d/%q", status, kind)
	}
}

func TestIsRegionForbidden(t *testing.T) {
	if !isRegionForbidden(403, `{"error":{"message":"API is not available in your region"}}`) {
		t.Fatal("region lock not detected: refreshing a token cannot fix it")
	}
	if isRegionForbidden(403, `{"error":{"message":"forbidden"}}`) {
		t.Fatal("a plain 403 is not a region lock")
	}
	if isRegionForbidden(401, `not available in your region`) {
		t.Fatal("only 403 can be the region lock")
	}
}
