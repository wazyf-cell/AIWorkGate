package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// geminiTestRequest is the generateContent shape decoded back for assertions.
type geminiTestRequest struct {
	Contents []struct {
		Role  string `json:"role"`
		Parts []struct {
			Text       string `json:"text"`
			Thought    bool   `json:"thought"`
			InlineData *struct {
				MimeType string `json:"mimeType"`
				Data     string `json:"data"`
			} `json:"inlineData"`
			FunctionCall *struct {
				Name string         `json:"name"`
				Args map[string]any `json:"args"`
			} `json:"functionCall"`
			FunctionResponse *struct {
				Name     string `json:"name"`
				Response struct {
					Content string `json:"content"`
				} `json:"response"`
			} `json:"functionResponse"`
		} `json:"parts"`
	} `json:"contents"`
	SystemInstruction *struct {
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	} `json:"systemInstruction"`
	GenerationConfig *struct {
		Temperature     *float64 `json:"temperature"`
		TopP            *float64 `json:"topP"`
		MaxOutputTokens *int     `json:"maxOutputTokens"`
		StopSequences   []string `json:"stopSequences"`
		ThinkingConfig  *struct {
			ThinkingBudget  *int `json:"thinkingBudget"`
			IncludeThoughts bool `json:"includeThoughts"`
		} `json:"thinkingConfig"`
	} `json:"generationConfig"`
	Tools []struct {
		FunctionDeclarations []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			Parameters  map[string]any `json:"parameters"`
		} `json:"functionDeclarations"`
	} `json:"tools"`
	ToolConfig *struct {
		FunctionCallingConfig struct {
			Mode                 string   `json:"mode"`
			AllowedFunctionNames []string `json:"allowedFunctionNames"`
		} `json:"functionCallingConfig"`
	} `json:"toolConfig"`
}

func decodeGeminiRequest(t *testing.T, body []byte) geminiTestRequest {
	t.Helper()
	var req geminiTestRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("converted Gemini body is not valid: %v (%s)", err, body)
	}
	return req
}

func requireGeminiRejected(t *testing.T, chatBody string, want string) {
	t.Helper()
	out, err := chatToGeminiRequest([]byte(chatBody), "gemini-2.5-pro", "")
	if err == nil {
		t.Fatalf("want rejection, got %s", out)
	}
	var envErr *envelopeError
	if !errors.As(err, &envErr) {
		t.Fatalf("want *envelopeError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not mention %q", err, want)
	}
}

// ------------------------------------------------------------------ requests

func TestChatToGeminiRequest(t *testing.T) {
	t.Run("text system generation tools and choice", func(t *testing.T) {
		src := `{"model":"gemini-2.5-pro","temperature":0.3,"top_p":0.9,"max_tokens":128,` +
			`"stop":["END"],"stream":true,"tool_choice":"required",` +
			`"messages":[` +
			`{"role":"system","content":"be brief"},` +
			`{"role":"developer","content":[{"type":"text","text":"no fluff"}]},` +
			`{"role":"user","content":"hi"},` +
			`{"role":"assistant","content":"hello"},` +
			`{"role":"user","content":"weather?"}],` +
			`"tools":[{"type":"function","function":{"name":"get_weather",` +
			`"description":"Look up weather","parameters":{"type":"object",` +
			`"properties":{"city":{"type":"string"}},"required":["city"]}}}]}`
		body, err := chatToGeminiRequest([]byte(src), "gemini-2.5-pro", "")
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		req := decodeGeminiRequest(t, body)
		if req.SystemInstruction == nil || len(req.SystemInstruction.Parts) != 1 {
			t.Fatalf("systemInstruction = %+v", req.SystemInstruction)
		}
		if got := req.SystemInstruction.Parts[0].Text; got != "be brief\n\nno fluff" {
			t.Fatalf("system text = %q, want joined system and developer", got)
		}
		roles := make([]string, 0, len(req.Contents))
		for _, content := range req.Contents {
			roles = append(roles, content.Role)
		}
		if got := strings.Join(roles, ","); got != "user,model,user" {
			t.Fatalf("contents roles = %q, want user,model,user", got)
		}
		if got := req.Contents[1].Parts[0].Text; got != "hello" {
			t.Fatalf("assistant text = %q", got)
		}
		gen := req.GenerationConfig
		if gen == nil {
			t.Fatal("generationConfig missing")
		}
		if gen.Temperature == nil || *gen.Temperature != 0.3 {
			t.Fatalf("temperature = %v", gen.Temperature)
		}
		if gen.TopP == nil || *gen.TopP != 0.9 {
			t.Fatalf("topP = %v", gen.TopP)
		}
		if gen.MaxOutputTokens == nil || *gen.MaxOutputTokens != 128 {
			t.Fatalf("maxOutputTokens = %v", gen.MaxOutputTokens)
		}
		if strings.Join(gen.StopSequences, "|") != "END" {
			t.Fatalf("stopSequences = %v", gen.StopSequences)
		}
		if len(req.Tools) != 1 || len(req.Tools[0].FunctionDeclarations) != 1 {
			t.Fatalf("tools = %+v", req.Tools)
		}
		decl := req.Tools[0].FunctionDeclarations[0]
		if decl.Name != "get_weather" || decl.Description != "Look up weather" ||
			decl.Parameters["type"] != "object" {
			t.Fatalf("declaration = %+v", decl)
		}
		if req.ToolConfig == nil || req.ToolConfig.FunctionCallingConfig.Mode != "ANY" {
			t.Fatalf("toolConfig = %+v", req.ToolConfig)
		}
	})

	t.Run("max_completion_tokens preferred", func(t *testing.T) {
		body, err := chatToGeminiRequest([]byte(
			`{"max_tokens":10,"max_completion_tokens":20,"stop":"STOP",`+
				`"messages":[{"role":"user","content":"hi"}]}`), "m", "")
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		gen := decodeGeminiRequest(t, body).GenerationConfig
		if gen == nil || gen.MaxOutputTokens == nil || *gen.MaxOutputTokens != 20 {
			t.Fatalf("maxOutputTokens = %+v", gen)
		}
		if len(gen.StopSequences) != 1 || gen.StopSequences[0] != "STOP" {
			t.Fatalf("stopSequences = %v", gen.StopSequences)
		}
	})

	t.Run("image data URL", func(t *testing.T) {
		src := `{"messages":[{"role":"user","content":[` +
			`{"type":"text","text":"what is this?"},` +
			`{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}]}`
		body, err := chatToGeminiRequest([]byte(src), "m", "")
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		req := decodeGeminiRequest(t, body)
		parts := req.Contents[0].Parts
		if len(parts) != 2 || parts[0].Text != "what is this?" {
			t.Fatalf("parts = %+v", parts)
		}
		if parts[1].InlineData == nil || parts[1].InlineData.MimeType != "image/png" ||
			parts[1].InlineData.Data != "iVBORw0KGgo=" {
			t.Fatalf("inlineData = %+v", parts[1].InlineData)
		}
	})

	t.Run("http image URL rejected", func(t *testing.T) {
		requireGeminiRejected(t,
			`{"messages":[{"role":"user","content":[{"type":"image_url",`+
				`"image_url":{"url":"https://example.com/cat.png"}}]}]}`,
			"data URL")
	})

	t.Run("tool result round trip", func(t *testing.T) {
		src := `{"messages":[` +
			`{"role":"user","content":"weather?"},` +
			`{"role":"assistant","content":null,"tool_calls":[{"id":"call_abc","type":"function",` +
			`"function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},` +
			`{"role":"tool","tool_call_id":"call_abc","content":"18C sunny"}]}`
		body, err := chatToGeminiRequest([]byte(src), "m", "")
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		req := decodeGeminiRequest(t, body)
		if len(req.Contents) != 3 {
			t.Fatalf("contents = %d, want 3", len(req.Contents))
		}
		model := req.Contents[1]
		if model.Role != "model" || len(model.Parts) != 1 || model.Parts[0].FunctionCall == nil {
			t.Fatalf("assistant turn = %+v", model)
		}
		call := model.Parts[0].FunctionCall
		if call.Name != "get_weather" {
			t.Fatalf("functionCall name = %q", call.Name)
		}
		if city, _ := call.Args["city"].(string); city != "Paris" {
			t.Fatalf("functionCall args = %#v, want an object with city=Paris", call.Args)
		}
		tool := req.Contents[2]
		if tool.Role != "user" || len(tool.Parts) != 1 || tool.Parts[0].FunctionResponse == nil {
			t.Fatalf("tool turn = %+v", tool)
		}
		resp := tool.Parts[0].FunctionResponse
		if resp.Name != "get_weather" || resp.Response.Content != "18C sunny" {
			t.Fatalf("functionResponse = %+v", resp)
		}
	})

	t.Run("explicit function choice", func(t *testing.T) {
		src := `{"tool_choice":{"type":"function","function":{"name":"get_weather"}},` +
			`"messages":[{"role":"user","content":"hi"}]}`
		body, err := chatToGeminiRequest([]byte(src), "m", "")
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		config := decodeGeminiRequest(t, body).ToolConfig
		if config == nil || config.FunctionCallingConfig.Mode != "ANY" {
			t.Fatalf("toolConfig = %+v", config)
		}
		allowed := config.FunctionCallingConfig.AllowedFunctionNames
		if len(allowed) != 1 || allowed[0] != "get_weather" {
			t.Fatalf("allowedFunctionNames = %v", allowed)
		}
	})

	t.Run("none choice", func(t *testing.T) {
		body, err := chatToGeminiRequest([]byte(
			`{"tool_choice":"none","messages":[{"role":"user","content":"hi"}]}`), "m", "")
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		config := decodeGeminiRequest(t, body).ToolConfig
		if config == nil || config.FunctionCallingConfig.Mode != "NONE" {
			t.Fatalf("toolConfig = %+v", config)
		}
	})

	t.Run("reasoning effort fallback", func(t *testing.T) {
		body, err := chatToGeminiRequest([]byte(
			`{"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`), "m", "")
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		gen := decodeGeminiRequest(t, body).GenerationConfig
		if gen == nil || gen.ThinkingConfig == nil || gen.ThinkingConfig.ThinkingBudget == nil ||
			*gen.ThinkingConfig.ThinkingBudget != 8192 {
			t.Fatalf("thinkingConfig = %+v", gen)
		}
	})

	t.Run("reasoning tiers", func(t *testing.T) {
		const src = `{"messages":[{"role":"user","content":"hi"}]}`
		cases := []struct {
			tier    string
			budget  int
			thought bool
		}{
			{"off", 0, false},
			{"auto", -1, true},
			{"high", 8192, true},
		}
		for _, tc := range cases {
			body, err := chatToGeminiRequest([]byte(src), "m", tc.tier)
			if err != nil {
				t.Fatalf("%s: convert: %v", tc.tier, err)
			}
			gen := decodeGeminiRequest(t, body).GenerationConfig
			if gen == nil || gen.ThinkingConfig == nil || gen.ThinkingConfig.ThinkingBudget == nil {
				t.Fatalf("%s: thinkingConfig = %+v", tc.tier, gen)
			}
			if got := *gen.ThinkingConfig.ThinkingBudget; got != tc.budget {
				t.Fatalf("%s: thinkingBudget = %d, want %d", tc.tier, got, tc.budget)
			}
			if gen.ThinkingConfig.IncludeThoughts != tc.thought {
				t.Fatalf("%s: includeThoughts = %v", tc.tier, gen.ThinkingConfig.IncludeThoughts)
			}
		}
	})

	t.Run("empty tier omits thinkingConfig", func(t *testing.T) {
		body, err := chatToGeminiRequest([]byte(
			`{"messages":[{"role":"user","content":"hi"}]}`), "m", "")
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		if gen := decodeGeminiRequest(t, body).GenerationConfig; gen != nil && gen.ThinkingConfig != nil {
			t.Fatalf("thinkingConfig = %+v, want omitted", gen.ThinkingConfig)
		}
	})
}

// ----------------------------------------------------------------- responses

func TestGeminiResponseToChat(t *testing.T) {
	t.Run("text thought call and usage", func(t *testing.T) {
		src := `{"candidates":[{"content":{"parts":[` +
			`{"text":"The answer"},` +
			`{"thought":true,"text":"Let me think"},` +
			`{"functionCall":{"name":"get_weather","args":{"city":"Paris"}}}]},` +
			`"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":11,` +
			`"candidatesTokenCount":22,"cachedContentTokenCount":3,` +
			`"thoughtsTokenCount":4,"totalTokenCount":40}}`
		out, err := geminiResponseToChat([]byte(src), "gemini-2.5-pro")
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		var reply chatReply
		if err := json.Unmarshal(out, &reply); err != nil {
			t.Fatalf("reply is not valid JSON: %v (%s)", err, out)
		}
		if !strings.HasPrefix(reply.ID, "chatcmpl-gemini-") {
			t.Fatalf("id = %q", reply.ID)
		}
		if reply.Model != "gemini-2.5-pro" || reply.Created == 0 {
			t.Fatalf("model/created = %q/%d", reply.Model, reply.Created)
		}
		if len(reply.Choices) != 1 {
			t.Fatalf("choices = %d", len(reply.Choices))
		}
		choice := reply.Choices[0]
		if choice.Message.Role != "assistant" || choice.Message.Content != "The answer" {
			t.Fatalf("message = %+v", choice.Message)
		}
		if choice.Message.ReasoningContent != "Let me think" {
			t.Fatalf("reasoning_content = %q", choice.Message.ReasoningContent)
		}
		if len(choice.Message.ToolCalls) != 1 {
			t.Fatalf("tool_calls = %+v", choice.Message.ToolCalls)
		}
		call := choice.Message.ToolCalls[0]
		if call.ID != "call_0" || call.Type != "function" || call.Function.Name != "get_weather" {
			t.Fatalf("tool call = %+v", call)
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			t.Fatalf("arguments is not JSON: %q", call.Function.Arguments)
		}
		if args["city"] != "Paris" {
			t.Fatalf("arguments = %q", call.Function.Arguments)
		}
		if choice.FinishReason != "stop" {
			t.Fatalf("finish_reason = %v", choice.FinishReason)
		}
		usage := reply.Usage
		if usage == nil || usage.PromptTokens != 11 || usage.CompletionTokens != 22 || usage.TotalTokens != 40 {
			t.Fatalf("usage = %+v", usage)
		}
		if usage.PromptTokensDetails == nil || usage.PromptTokensDetails.CachedTokens != 3 {
			t.Fatalf("cached tokens = %+v", usage.PromptTokensDetails)
		}
		if usage.CompletionTokensDetails == nil || usage.CompletionTokensDetails.ReasoningTokens != 4 {
			t.Fatalf("reasoning tokens = %+v", usage.CompletionTokensDetails)
		}
	})

	t.Run("blocked prompt becomes content_filter", func(t *testing.T) {
		src := `{"promptFeedback":{"blockReason":"SAFETY"},` +
			`"usageMetadata":{"promptTokenCount":5,"totalTokenCount":5}}`
		out, err := geminiResponseToChat([]byte(src), "m")
		if err != nil {
			t.Fatalf("blocked prompt must not error: %v", err)
		}
		var reply chatReply
		if err := json.Unmarshal(out, &reply); err != nil {
			t.Fatalf("reply is not valid JSON: %v", err)
		}
		if len(reply.Choices) != 1 || reply.Choices[0].FinishReason != "content_filter" {
			t.Fatalf("choices = %+v", reply.Choices)
		}
		if reply.Choices[0].Message.Content != "" {
			t.Fatalf("content = %q, want empty", reply.Choices[0].Message.Content)
		}
		if reply.Usage == nil || reply.Usage.PromptTokens != 5 {
			t.Fatalf("usage = %+v", reply.Usage)
		}
	})

	t.Run("safety finish maps to content_filter", func(t *testing.T) {
		out, err := geminiResponseToChat([]byte(
			`{"candidates":[{"content":{"parts":[{"text":"partial"}]},"finishReason":"SAFETY"}]}`), "m")
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		var reply chatReply
		if err := json.Unmarshal(out, &reply); err != nil {
			t.Fatalf("reply is not valid JSON: %v", err)
		}
		if reply.Choices[0].FinishReason != "content_filter" {
			t.Fatalf("finish_reason = %v", reply.Choices[0].FinishReason)
		}
	})
}

// ------------------------------------------------------------------- streams

type geminiTestFrame struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string      `json:"role"`
			Content          string      `json:"content"`
			ReasoningContent string      `json:"reasoning_content"`
			ToolCalls        []toolDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage"`
}

// parseGeminiFrames splits the emitted SSE body into frames and reports whether
// the terminal [DONE] sentinel was present.
func parseGeminiFrames(t *testing.T, out string) ([]geminiTestFrame, bool) {
	t.Helper()
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("stream does not end with data: [DONE]\\n\\n: %q", out)
	}
	var frames []geminiTestFrame
	for _, block := range strings.Split(strings.TrimSuffix(out, "data: [DONE]\n\n"), "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		payload, ok := strings.CutPrefix(block, "data: ")
		if !ok {
			t.Fatalf("frame %q lacks the data: prefix", block)
		}
		var frame geminiTestFrame
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			t.Fatalf("frame is not valid JSON: %v (%s)", err, payload)
		}
		frames = append(frames, frame)
	}
	if len(frames) == 0 {
		t.Fatalf("stream emitted no frames: %q", out)
	}
	return frames, true
}

func TestGeminiStreamToChat(t *testing.T) {
	t.Run("text finish usage and done", func(t *testing.T) {
		src := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hel\"}]}}]}\n\n" +
			"data: not-json\n\n" +
			"data: \n\n" +
			"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"lo\"}," +
			"{\"thought\":true,\"text\":\"hmm\"}]}}]}\n\n" +
			"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\" world\"}]}," +
			"\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":7," +
			"\"candidatesTokenCount\":3,\"totalTokenCount\":10}}\n\n"
		var out bytes.Buffer
		if err := geminiStreamToChat(strings.NewReader(src), &out, "gemini-2.5-pro"); err != nil {
			t.Fatalf("stream: %v", err)
		}
		frames, done := parseGeminiFrames(t, out.String())
		if !done {
			t.Fatal("[DONE] missing")
		}
		first := frames[0]
		if first.Object != "chat.completion.chunk" || first.Model != "gemini-2.5-pro" || first.Created == 0 {
			t.Fatalf("first frame = %+v", first)
		}
		if first.Choices[0].Delta.Role != "assistant" {
			t.Fatalf("first delta role = %q, want assistant", first.Choices[0].Delta.Role)
		}
		var content, reasoning strings.Builder
		finish := ""
		var usage *wireUsage
		for _, frame := range frames {
			if len(frame.Choices) > 0 {
				content.WriteString(frame.Choices[0].Delta.Content)
				reasoning.WriteString(frame.Choices[0].Delta.ReasoningContent)
				if frame.Choices[0].FinishReason != nil {
					finish = *frame.Choices[0].FinishReason
				}
			}
			if frame.Usage != nil {
				usage = frame.Usage
			}
		}
		if content.String() != "Hello world" {
			t.Fatalf("content = %q", content.String())
		}
		if reasoning.String() != "hmm" {
			t.Fatalf("reasoning = %q", reasoning.String())
		}
		if finish != "stop" {
			t.Fatalf("finish_reason = %q", finish)
		}
		if usage == nil || usage.PromptTokens != 7 || usage.CompletionTokens != 3 || usage.TotalTokens != 10 {
			t.Fatalf("usage = %+v", usage)
		}
		last := frames[len(frames)-1]
		if last.Usage == nil || len(last.Choices) != 0 {
			t.Fatalf("final usage chunk = %+v", last)
		}
	})

	t.Run("function call", func(t *testing.T) {
		src := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":" +
			"{\"name\":\"get_weather\",\"args\":{\"city\":\"Paris\"}}}]}," +
			"\"finishReason\":\"STOP\"}]}\n\n"
		var out bytes.Buffer
		if err := geminiStreamToChat(strings.NewReader(src), &out, "m"); err != nil {
			t.Fatalf("stream: %v", err)
		}
		frames, _ := parseGeminiFrames(t, out.String())
		var calls []toolDelta
		finish := ""
		for _, frame := range frames {
			if len(frame.Choices) == 0 {
				continue
			}
			calls = append(calls, frame.Choices[0].Delta.ToolCalls...)
			if frame.Choices[0].FinishReason != nil {
				finish = *frame.Choices[0].FinishReason
			}
		}
		if len(calls) != 1 {
			t.Fatalf("tool_calls = %+v", calls)
		}
		call := calls[0]
		if call.ID != "call_0" || call.Type != "function" || call.Function.Name != "get_weather" {
			t.Fatalf("tool call = %+v", call)
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			t.Fatalf("arguments is not JSON: %q", call.Function.Arguments)
		}
		if args["city"] != "Paris" {
			t.Fatalf("arguments = %q", call.Function.Arguments)
		}
		if finish != "stop" {
			t.Fatalf("finish_reason = %q", finish)
		}
	})
}
