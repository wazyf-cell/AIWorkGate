// Package main implements the protocol layer for the OpenCode free tier
// (https://opencode.ai/inference/v1), ported from the reference implementation's
// vendor/channel-pack/src/opencode-messages.ts and opencode-capability.ts.
//
// Two facts drive everything below, both measured against the live endpoint on
// 2026-10-21 (and both documented in the reference):
//
//  1. The free tier only answers when the request *looks like* the official CLI:
//     `stream` must be true, and the payload must declare the official `read`
//     and `shell` tool definitions verbatim. Anything else gets
//     HTTP 403 {"type":"error","error":{"type":"FreeTierError",...}}.
//  2. Because of (1) the upstream always replies as SSE, so a caller that asked
//     for a non-streaming completion still needs the stream folded back into one
//     chat.completion object here.
package main

import (
	"bytes"
	"encoding/json"
)

// gateTools are the two placeholders the free tier insists on. The names and
// the exact descriptions/parameter schemas are copied verbatim from the
// reference (opencode-messages.ts GATE_TOOL_DEFINITIONS): the upstream compares
// them structurally, so a reworded description is treated as a missing gate.
var gateTools = []string{"read", "shell"}

var gateToolDefinitions = map[string]map[string]any{
	"read": {
		"description": "Read the contents of a file or directory. Supports text files, images, and PDFs. Images and PDFs are presented directly to the model. Each text line is prefixed by its 1-based line number as <line>: <content>. The prefix is for reference and is not part of the file content. Directory entries are returned one per line. Use offset and limit to read large files or directories in sections. Prefer one larger read over many small slices, and use grep to find specific content in large files.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":   map[string]any{"type": "string", "description": "File or directory to read"},
				"offset": map[string]any{"type": "integer", "minimum": 0, "description": "The line or directory entry to start reading from (1-based)"},
				"limit":  map[string]any{"type": "integer", "minimum": 0, "description": "The maximum number of lines or directory entries to read (defaults to and capped at 2000)"},
			},
			"required":             []any{"path"},
			"additionalProperties": false,
		},
	},
	"shell": {
		"description": "Execute a shell command and return its output. Commands run on Linux using bash. Quote file paths containing spaces or special characters. Prefer dedicated tools over shell commands when possible. When output is large, the full result is saved to a file and a truncated preview is returned. Rely on automatic truncation unless filtering the output is more useful. Commands accept an optional timeout, background commands have no timeout by default. Background commands return immediately, and you will be notified when they complete.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command":    map[string]any{"type": "string", "description": "Shell command string to execute"},
				"workdir":    map[string]any{"type": "string", "description": "Working directory to execute the command in. Defaults to the current working directory. When possible, avoid changing directories in the command and set the working directory here instead."},
				"timeout":    map[string]any{"type": "integer", "minimum": 0, "description": "Timeout in milliseconds. Set to 0 to disable the timeout. Defaults to 120000 for foreground commands. Background commands have no timeout by default."},
				"background": map[string]any{"type": "boolean", "description": "Run the command in the background and return immediately (useful for dev servers and long-running builds). You do not need to use '&' at the end of the command when using this parameter. You will be notified when it completes. DO NOT poll for completion."},
			},
			"required":             []any{"command"},
			"additionalProperties": false,
		},
	},
}

type gateShape struct {
	tools    []any
	injected []string
}

// ensureGateShape dedupes read/shell and injects whichever official definition is
// absent, exactly as the reference's ensureFreeLaneShape does.
func ensureGateShape(tools []any, aliases map[string]string) gateShape {
	seen := map[string]bool{}
	present := map[string]bool{}
	next := make([]any, 0, len(tools)+len(gateTools))
	for _, raw := range tools {
		row, _ := raw.(map[string]any)
		fn, _ := row["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if !isGateName(name) {
			next = append(next, raw)
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		present[name] = true
		next = append(next, raw)
	}
	var injected []string
	for _, name := range gateTools {
		if present[name] {
			continue
		}
		next = append(next, officialGateTool(name))
		injected = append(injected, name)
	}
	// A caller tool renamed to read_client/shell_client must not shadow the gate
	// names; aliases were already assigned in buildTools, so nothing to do here
	// beyond keeping the map consistent.
	for alias, original := range aliases {
		if seen[alias] && isGateName(original) {
			delete(aliases, alias)
		}
	}
	return gateShape{tools: next, injected: injected}
}

func officialGateTool(name string) map[string]any {
	def := gateToolDefinitions[name]
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": def["description"],
			"parameters":  def["parameters"],
		},
	}
}

// isGateTool reports whether the caller supplied a tool occupying a gate name.
func isGateTool(name string) bool { return name == "read" || name == "shell" }

func isGateName(name string) bool { return isGateTool(name) }

// isOfficialGateTool reports whether the tool already carries the exact official
// description and schema. Only such a tool may keep the bare gate name.
func isOfficialGateTool(t oaTool, name string) bool {
	def, ok := gateToolDefinitions[name]
	if !ok {
		return false
	}
	if t.Function.Description != def["description"] {
		return false
	}
	want, err := json.Marshal(def["parameters"])
	if err != nil {
		return false
	}
	got := bytes.TrimSpace(t.Function.Parameters)
	if len(got) == 0 {
		return false
	}
	var a, b any
	if json.Unmarshal(got, &a) != nil || json.Unmarshal(want, &b) != nil {
		return false
	}
	na, _ := json.Marshal(a)
	nb, _ := json.Marshal(b)
	return bytes.Equal(na, nb)
}
