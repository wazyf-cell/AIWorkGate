package rules

import (
	"strings"

	"aiworkgate/internal/model"
)

// BuiltinWBPack is the name of the pack seeded on first start (M6).
const BuiltinWBPack = "wb-moderation"

// BuiltinPack returns a fresh copy of the named builtin pack. The bool reports
// whether name is a builtin at all. Every call rebuilds the tables, so a caller
// may edit the returned pack (and save it) without affecting later resets.
func BuiltinPack(name string) (model.RulePack, bool) {
	switch strings.TrimSpace(name) {
	case BuiltinWBPack:
		return builtinWBPack(), true
	default:
		return model.RulePack{}, false
	}
}

// builtinWBPack returns a fresh copy of A's WorkBuddy pack: its literal
// identity-sentence table plus the cc_/x-anthropic token-pair strip.
func builtinWBPack() model.RulePack {
	return model.RulePack{
		Name:        BuiltinWBPack,
		Description: "WorkBuddy 上游内容审查规避（字面替换 + cc_/x-anthropic token 对剥离）",
		Rules:       builtinWBRules(),
		Strip:       builtinWBStrip(),
	}
}

// builtinWBRules mirrors default_template_map() from A's
// api_server/wb_payload.rs (ordered literal replacements). F80 counted 13
// pairs when it was measured; A's current table ships 18, all ported verbatim.
func builtinWBRules() []model.Rule {
	return []model.Rule{
		{
			From: `You are Claude Code, Anthropic's official CLI for Claude.`,
			To:   `You are Claude Code, Anthropic's official CLI tool for Claude.`,
		},
		{
			From: `Main branch (you will usually use this with PRs)`,
			To:   `Default branch (you will usually use this with PRs)`,
		},
		{
			From: `Main branch (you will usually use this for PRs)`,
			To:   `Default branch (you will usually use this for PRs)`,
		},
		{
			From: `You are Codex, based on GPT-5`,
			To:   `You are Codex, built on GPT-5`,
		},
		{
			From: `You are Codex, an agent based on GPT-5`,
			To:   `You are Codex, an agent built on GPT-5`,
		},
		{
			From: `You are Codex, a coding agent based on GPT-5`,
			To:   `You are Codex, a coding agent built on GPT-5`,
		},
		{
			From: `You are a coding agent running in the Codex CLI`,
			To:   `You are a coding agent working in the Codex CLI`,
		},
		{
			From: `You are an interactive agent in TraeCode that helps the USER with software engineering tasks.`,
			To:   `You are an interactive agent running in TraeCode that helps the USER with software engineering tasks.`,
		},
		{
			From: `You are Cline, a highly skilled software engineer with extensive knowledge`,
			To:   `You are Cline, a highly capable software engineering assistant with extensive knowledge`,
		},
		{
			From: `You are Roo, a highly skilled software engineer with extensive knowledge`,
			To:   `You are Roo, a highly capable software engineering assistant with extensive knowledge`,
		},
		{
			From: `You are an interactive CLI agent specializing in software engineering tasks.`,
			To:   `You are an interactive CLI assistant specializing in software engineering tasks.`,
		},
		{
			From: `You are opencode, an interactive CLI tool that helps users with software engineering tasks.`,
			To:   `You are opencode, an interactive CLI assistant that helps users with software engineering tasks.`,
		},
		{
			From: `You are OpenCode, You and the user share the same workspace.`,
			To:   `You are OpenCode, You and the user work in the same workspace.`,
		},
		{
			From: `You are OpenCode, the best coding agent on the planet.`,
			To:   `You are OpenCode, a top coding agent for software engineering.`,
		},
		{
			From: `You are opencode, an interactive CLI agent specializing in software engineering tasks.`,
			To:   `You are opencode, an interactive CLI assistant specializing in software engineering tasks.`,
		},
		{
			From: `You are operating as and within the OpenCode CLI, a terminal-based agentic coding assistant built by OpenAI.`,
			To:   `You are operating as and within the OpenCode CLI, a terminal-based agentic coding tool built by OpenAI.`,
		},
		{
			From: `You are an AI agent powered by DeepSeek Harness.`,
			To:   `You are an AI agent built on DeepSeek Harness.`,
		},
		{
			From: `You are a coding agent powered by the glm-`,
			To:   `You are a coding agent built on the glm-`,
		},
	}
}

// builtinWBStrip removes `cc_*` / `x-anthropic-*` key-value pairs (A's
// strip_cc_fingerprints, wb_payload.rs:211-262). A scans byte-wise: token-run
// key at a token boundary, then `=`/`:`, then a value up to `;`, `,` or
// whitespace, then trailing `;`/`,`/blanks. One regexp is the closest single
// form; `\b` keeps it conservative, so text without those prefixes is untouched.
func builtinWBStrip() []model.StripPattern {
	return []model.StripPattern{
		{Pattern: `\b(?:cc_|x-anthropic-)[A-Za-z0-9_-]+\s*[=:]\s*[^;,\s]+[;,]?[ \t]*`},
	}
}
