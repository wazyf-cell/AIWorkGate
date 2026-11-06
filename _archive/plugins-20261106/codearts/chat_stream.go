package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// ---- upstream call and SSE parsing ----

type streamOutcome struct {
	kind     string // done, queue, quota, benefit, timeout, error
	message  string
	finish   string
	hasTools bool
}

type sseToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type sseDelta struct {
	Role             string        `json:"role"`
	Content          string        `json:"content"`
	ReasoningContent string        `json:"reasoning_content"`
	ToolCalls        []sseToolCall `json:"tool_calls"`
}

type sseChoice struct {
	Index        int      `json:"index"`
	Delta        sseDelta `json:"delta"`
	FinishReason *string  `json:"finish_reason"`
}

type sseEvent struct {
	ID        string          `json:"id"`
	Choices   []sseChoice     `json:"choices"`
	Usage     json.RawMessage `json:"usage"`
	ErrorCode string          `json:"error_code"`
	ErrorMsg  string          `json:"error_msg"`
	Error     *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (p *Plugin) postChat(ctx context.Context, acct *Account, proxyURL string, body []byte, benefit bool) (*http.Response, error) {
	var extraSigned map[string]string
	if benefit {
		extraSigned = map[string]string{"maas_type": "benefit"}
	}
	headers, err := signRequestHuawei(http.MethodPost, p.endpoints.Chat, body, acct.Credential.AccessKeyID, acct.Credential.SecretAccessKey, acct.Credential.SecurityToken, extraSigned, p.now())
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoints.Chat, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		switch strings.ToLower(k) {
		case "host":
		case "authorization":
			req.Header.Set("Authorization", v)
		default:
			req.Header.Set(k, v)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Chat-Id", acct.Identity.ChatID)
	req.Header.Set("Session-Id", acct.Identity.SessionID)
	req.Header.Set("lang", "en")
	return p.httpClientFor(acct.ID, proxyURL).Do(req)
}

type lineResult struct {
	line string
	err  error
}

// consumeChatStream reads the upstream SSE stream with the reference timeouts:
// 300s until the first token and 600s idle after that. It converts events to
// a sink so the same parser serves streaming and aggregate responses.
func (p *Plugin) consumeChatStream(ctx context.Context, body io.Reader, sink chatSink) streamOutcome {
	reader := bufio.NewReaderSize(body, 64*1024)
	lines := make(chan lineResult, 32)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			line, err := reader.ReadString('\n')
			if line != "" {
				select {
				case lines <- lineResult{line: line}:
				case <-done:
					return
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				select {
				case lines <- lineResult{err: err}:
				case <-done:
				case <-ctx.Done():
				}
				return
			}
		}
	}()

	timer := time.NewTimer(p.opts.FirstTokenTimeout)
	defer timer.Stop()
	contentExtractor := &dsmlExtractor{}
	reasoningExtractor := &dsmlExtractor{}
	dsmlIndex := 0
	nativeIndex := map[int]int{}
	outcome := streamOutcome{kind: "done"}
	toolSeen := false

	flushExtractors := func() {
		if text, reasoning := contentExtractor.flush(); text != "" || reasoning != "" {
			sink.Text(text)
			sink.Reasoning(reasoning)
		}
		if text, reasoning := reasoningExtractor.flush(); text != "" || reasoning != "" {
			sink.Reasoning(text + reasoning)
		}
	}

	for {
		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				return streamOutcome{kind: "timeout", message: "codearts: 上游响应超时"}
			}
			return streamOutcome{kind: "error", message: "client disconnected"}
		case <-timer.C:
			return streamOutcome{kind: "timeout", message: "codearts: 等待上游首个 token 超时"}
		case res := <-lines:
			if res.err != nil {
				if res.err != io.EOF {
					if ctx.Err() != nil {
						return streamOutcome{kind: "timeout", message: "codearts: 上游响应超时"}
					}
					return streamOutcome{kind: "error", message: "codearts: sse transport error: " + res.err.Error()}
				}
				flushExtractors()
				outcome.hasTools = toolSeen
				return outcome
			}
			line := strings.TrimSpace(res.line)
			if line == "" || !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "[DONE]" {
				flushExtractors()
				outcome.hasTools = toolSeen
				return outcome
			}
			var ev sseEvent
			if err := json.Unmarshal([]byte(payload), &ev); err != nil {
				continue
			}
			timer.Reset(p.opts.IdleTimeout)
			if len(ev.Usage) > 0 {
				sink.Usage(ev.Usage)
			}
			code := ev.ErrorCode
			msg := ev.ErrorMsg
			if code == "" && ev.Error != nil {
				code, msg = ev.Error.Code, ev.Error.Message
			}
			if code != "" {
				if msg == "" {
					msg = code
				}
				switch {
				case isQuotaCode(code, msg):
					return streamOutcome{kind: "quota", message: "codearts: " + msg}
				case isQueueCode(code):
					return streamOutcome{kind: "queue", message: "codearts: " + msg}
				case code == "InferHub.4004.200":
					return streamOutcome{kind: "benefit", message: "codearts: " + msg}
				default:
					return streamOutcome{kind: "error", message: "codearts: " + msg}
				}
			}
			for _, choice := range ev.Choices {
				if choice.FinishReason != nil && *choice.FinishReason != "" {
					sink.Finish(*choice.FinishReason)
					outcome.finish = *choice.FinishReason
				}
				delta := choice.Delta
				if delta.Content != "" {
					text, reasoning, calls := contentExtractor.feed(delta.Content)
					if text != "" {
						sink.Text(text)
					}
					if reasoning != "" {
						sink.Reasoning(reasoning)
					}
					for _, call := range calls {
						if strings.TrimSpace(call.Name) == "" {
							continue
						}
						id := "dsml-" + randomHex(16)
						sink.ToolDelta(dsmlIndex, id, call.Name, call.Arguments, true)
						dsmlIndex++
						toolSeen = true
					}
				}
				if delta.ReasoningContent != "" {
					text, reasoning, calls := reasoningExtractor.feed(delta.ReasoningContent)
					if text+reasoning != "" {
						sink.Reasoning(text + reasoning)
					}
					for _, call := range calls {
						if strings.TrimSpace(call.Name) == "" {
							continue
						}
						id := "dsml-" + randomHex(16)
						sink.ToolDelta(dsmlIndex, id, call.Name, call.Arguments, true)
						dsmlIndex++
						toolSeen = true
					}
				}
				for _, tc := range delta.ToolCalls {
					upstreamIdx := tc.Index
					idx, ok := nativeIndex[upstreamIdx]
					if !ok {
						idx = upstreamIdx
						nativeIndex[upstreamIdx] = idx
					}
					announce := tc.Function.Name != ""
					sink.ToolDelta(idx, tc.ID, tc.Function.Name, tc.Function.Arguments, announce)
					if announce {
						toolSeen = true
					}
				}
			}
		}
	}
}
