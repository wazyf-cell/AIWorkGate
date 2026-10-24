package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// translateRequest rewrites an OpenAI chat/completions body into the Cloud Code
// envelope.
//
// The mapping that matters for correctness:
//   - system/developer messages are hoisted into systemInstruction.parts;
//   - assistant becomes role "model", everything else becomes "user";
//   - tool_calls become functionCall parts, tool results become
//     functionResponse parts carrying {name, response:{content}};
//   - parameters are sanitised against GEMINI_SCHEMA_KEYS: any other key makes
//     the upstream answer 400;
//   - thinkingConfig always carries includeThoughts, and thinkingBudget is
//     omitted for the tiered (-1) case.
func translateRequest(raw []byte, opts requestOptions) ([]byte, error) {
	var req struct {
		Model       string          `json:"model"`
		Temperature *float64        `json:"temperature"`
		MaxTokens   int             `json:"max_tokens"`
		MaxCompTok  int             `json:"max_completion_tokens"`
		Messages    []oaMessage     `json:"messages"`
		Tools       []oaTool        `json:"tools"`
		ToolChoice  json.RawMessage `json:"tool_choice"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}

	// toolNames lets a tool result name its function without the caller having to
	// repeat it, exactly as the reference's toolNames map does.
	toolNames := map[string]string{}
	for _, m := range req.Messages {
		for _, tc := range m.ToolCalls {
			toolNames[tc.ID] = tc.Function.Name
		}
	}

	contents := make([]geminiContent, 0, len(req.Messages))
	var systemParts []string
	for _, m := range req.Messages {
		if m.Role == "system" || m.Role == "developer" {
			if text := m.Text(); text != "" {
				systemParts = append(systemParts, text)
			}
			continue
		}
		parts := messageParts(m, toolNames, opts.Images, opts.signatureLookup())
		if len(parts) == 0 {
			continue
		}
		role := "user"
		if m.Role == "assistant" {
			role = "model"
		}
		contents = append(contents, geminiContent{Role: role, Parts: parts})
	}
	if len(contents) == 0 {
		return nil, fmt.Errorf("gemini: messages has no usable content")
	}

	maxTok := opts.MaxTokens
	if maxTok <= 0 {
		maxTok = req.MaxTokens
	}
	if maxTok <= 0 {
		maxTok = req.MaxCompTok
	}
	if maxTok <= 0 {
		// The definition's maxOutputTokens, which buildGenerationConfig
		// also defaults to.
		maxTok = defaultMaxOutputTokens
	}
	temp := opts.Temperature
	if temp == nil {
		temp = req.Temperature
	}

	inner := innerRequest{
		Contents:         contents,
		GenerationConfig: buildGenerationConfig(opts.Spec, maxTok, temp),
		// The reference always sets sessionId on the inner request; it is a
		// stable hash of (project, lane, first user text[, generation]), not a
		// random id. The generation comes from the account identity, so an
		// identity reset makes the upstream see a new session.
		SessionID: deriveSessionIDGeneration(projectOr(opts.Project), firstUserText(contents),
			sessionLaneInfer, sessionGeneration(opts.Identity)),
	}
	if len(systemParts) > 0 {
		inner.SystemInstruction = &geminiContent{
			Role:  "system",
			Parts: []geminiPart{{Text: strings.Join(dedupeStrings(systemParts), "\n\n")}},
		}
	}
	if len(req.Tools) > 0 {
		inner.Tools = []geminiToolGroup{{FunctionDeclarations: functionDeclarations(req.Tools)}}
	}
	toolChoice := req.ToolChoice
	if len(opts.ToolChoice) > 0 && opts.ToolChoice != nil {
		toolChoice = opts.ToolChoice
	}
	if cfg := buildToolConfig(toolChoice, req.Tools); cfg != nil {
		inner.ToolConfig = cfg
	}

	project := projectOr(opts.Project)
	env := envelope{
		Model:     opts.Spec.Upstream,
		Project:   project,
		Request:   inner,
		RequestID: newRequestID(),
		UserAgent: "antigravity",
	}
	return json.Marshal(env)
}

// projectOr applies the documented default project when none was resolved.
func projectOr(project string) string {
	project = strings.TrimSpace(project)
	if project == "" {
		return defaultProject
	}
	return project
}

// firstUserText is geminiFirstUserText: the first text part of the first user
// turn, used only as a session-id ingredient.
func firstUserText(contents []geminiContent) string {
	for _, c := range contents {
		if c.Role != "user" {
			continue
		}
		for _, p := range c.Parts {
			if p.Text != "" {
				return p.Text
			}
		}
	}
	return ""
}

// signatureLookup returns the injected lookup, or nil when signatures are off
// for this attempt.
func (o requestOptions) signatureLookup() func(name, argsJSON string) string {
	if o.DropSignatures {
		return nil
	}
	return o.LookupSignature
}

// deriveSessionID is deriveGeminiSessionId: a 64-bit FNV-1a hash over
// "<project>\0<lane>\0<firstUserText>" rendered as a signed decimal string.
//
// The stable id matters: the upstream groups a conversation by it, so a random
// id per request would look like a new session every turn.
func deriveSessionID(project, firstUserText, lane string) string {
	return deriveSessionIDGeneration(project, firstUserText, lane, 0)
}

// deriveSessionIDGeneration adds the reference's optional generation suffix,
// used when a session has to be bumped.
func deriveSessionIDGeneration(project, firstUserText, lane string, generation int) string {
	const (
		offset64 = 0xcbf29ce484222325
		prime64  = 0x100000001b3
	)
	hash := uint64(offset64)
	base := project + "\x00" + lane + "\x00" + firstUserText
	if generation > 0 {
		base += "\x00" + strconv.Itoa(generation)
	}
	for i := 0; i < len(base); i++ {
		hash ^= uint64(base[i])
		hash *= prime64
	}
	return strconv.FormatInt(int64(hash), 10)
}

// parseToolArguments decodes a tool call's JSON arguments into an object. A
// malformed or non-object value degrades to {} — never invent arguments.
func parseToolArguments(raw string) map[string]any {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]any{}
	}
	var parsed map[string]any
	if json.Unmarshal([]byte(raw), &parsed) != nil || parsed == nil {
		return map[string]any{}
	}
	return parsed
}

// dedupeStrings keeps the first occurrence of each non-empty value.
func dedupeStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// openAIErrorBody renders an OpenAI-shaped error for a non-2xx upstream result.
func openAIErrorBody(status int, message string) []byte {
	if strings.TrimSpace(message) == "" {
		message = fmt.Sprintf("upstream returned HTTP %d", status)
	}
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "upstream_error",
			"code":    status,
		},
	})
	return b
}
