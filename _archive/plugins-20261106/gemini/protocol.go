// Package main implements protocol conversion between the OpenAI wire format the
// host speaks and the Gemini Cloud Code (Cloud Code Assist / antigravity) wire
// format that daily-cloudcode-pa.googleapis.com serves.
//
// Everything here is ported from the reference implementation's
// vendor/channel-pack/src/gemini.ts (translateGeminiRequest,
// buildGenerationConfig, buildToolConfig, sanitizeGeminiSchema) and its pack.js
// bundle. The upstream endpoint is
// /v1internal:streamGenerateContent?alt=sse, which speaks Gemini's
// generateContent but wraps each request in an extra envelope:
//
//	{"model": "<upstream model id>", "project": "<project>",
//	 "request": {contents, systemInstruction, generationConfig, tools, ...},
//	 "requestId": "agent/<ms>/<hex>", "userAgent": "antigravity"}
//
// The envelope keys are written in alphabetical order by the reference
// (marshalAlphabetical); JSON object order is not semantic, but the port keeps
// the same key set exactly.
package main

import (
	"encoding/json"
	"strings"
)

// oaTool is one OpenAI tool declaration.
type oaTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// oaToolCall is one OpenAI tool call emitted by the model.
type oaToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// oaMessage is one OpenAI message. Content is either a JSON string or the
// multimodal parts array, so it is kept raw and decoded on demand.
type oaMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []oaToolCall    `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
	Name       string          `json:"name"`
}

// Text returns the message's plain text, or "" when content is a parts array.
func (m oaMessage) Text() string {
	raw := strings.TrimSpace(string(m.Content))
	if raw == "" || raw == "null" {
		return ""
	}
	if !strings.HasPrefix(raw, "[") {
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			return s
		}
		return ""
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Content, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// geminiPart is one Gemini content part. Exactly one of the fields is set.
type geminiPart struct {
	Text             string            `json:"text,omitempty"`
	InlineData       *geminiInlineData `json:"inlineData,omitempty"`
	FunctionCall     *geminiCall       `json:"functionCall,omitempty"`
	FunctionResponse *geminiResponse   `json:"functionResponse,omitempty"`
	ThoughtSignature string            `json:"thoughtSignature,omitempty"`
}

type geminiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type geminiCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type geminiResponse struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

// generationConfig mirrors buildGenerationConfig: maxOutputTokens always, the
// thinking config always, temperature only when the caller gave one.
type generationConfig struct {
	MaxOutputTokens int `json:"maxOutputTokens"`
	ThinkingConfig  struct {
		IncludeThoughts bool `json:"includeThoughts"`
		// ThinkingBudget is omitted when negative: tiered sends -1 and the
		// reference skips the field in that case.
		ThinkingBudget int `json:"thinkingBudget,omitempty"`
	} `json:"thinkingConfig"`
	Temperature *float64 `json:"temperature,omitempty"`
}

// envelope is the outer Cloud Code request object.
type envelope struct {
	Model     string `json:"model"`
	Project   string `json:"project"`
	Request   any    `json:"request"`
	RequestID string `json:"requestId"`
	UserAgent string `json:"userAgent"`
}

// innerRequest is the Gemini generateContent body inside the envelope.
type innerRequest struct {
	Contents          []geminiContent   `json:"contents"`
	GenerationConfig  generationConfig  `json:"generationConfig"`
	SystemInstruction *geminiContent    `json:"systemInstruction,omitempty"`
	Tools             []geminiToolGroup `json:"tools,omitempty"`
	ToolConfig        *geminiToolConfig `json:"toolConfig,omitempty"`
	SessionID         string            `json:"sessionId,omitempty"`
}

type geminiToolGroup struct {
	FunctionDeclarations []geminiFunctionDecl `json:"functionDeclarations"`
}

type geminiFunctionDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type geminiToolConfig struct {
	FunctionCallingConfig struct {
		Mode                 string   `json:"mode"`
		AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
	} `json:"functionCallingConfig"`
}

// modelSpec is what geminiModelSpec returns: the upstream model id (which carries
// the effort tier as a suffix) plus the thinking budget it implies.
type modelSpec struct {
	Upstream        string
	Tier            string
	ThinkingBudget  int
	IncludeThoughts bool
}

// requestOptions is everything the translation needs from the caller.
type requestOptions struct {
	ModelID     string
	Spec        modelSpec
	System      string
	Temperature *float64
	MaxTokens   int
	ToolChoice  json.RawMessage
	// Images maps an OpenAI image URL to already-decoded inline data.
	Images map[string]inlineImage
	// Project is the resolved cloudaicompanionProject.
	Project string
	// Identity carries the per-account device/session tags. Nil keeps the
	// reference's generation-less session id, which the unit tests rely on.
	Identity *identityState
	// LookupSignature returns the cached thoughtSignature for a replayed tool
	// call, or "" when none is known.
	LookupSignature func(name, argsJSON string) string
	// DropSignatures forces a signature-free request; the retry path sets it
	// after the upstream rejected a signature.
	DropSignatures bool
}

// inlineImage is one decoded data-URL image.
type inlineImage struct {
	MimeType string
	Data     string
}
