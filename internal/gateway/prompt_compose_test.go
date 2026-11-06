package gateway

import (
	"math"
	"testing"
)

func sectionTokens(t *testing.T, composition PromptComposition, key string) int {
	t.Helper()
	for _, section := range composition.Sections {
		if section.Key == key {
			return section.Tokens
		}
	}
	t.Fatalf("section %q missing from %+v", key, composition.Sections)
	return 0
}

func TestAnalyzePromptOpenAISections(t *testing.T) {
	body := `{
		"model": "gpt-4o",
		"messages": [
			{"role": "system", "content": "sys"},
			{"role": "user", "content": "hello"},
			{"role": "assistant", "content": "hi"},
			{"role": "user", "content": "last"}
		],
		"tools": [{"type": "function", "function": {"name": "look", "description": "d"}}],
		"memory": {"note": "m"},
		"temperature": 0.2
	}`
	composition, err := AnalyzePrompt([]byte(body))
	if err != nil {
		t.Fatalf("AnalyzePrompt: %v", err)
	}
	if !composition.Estimated {
		t.Fatal("Estimated = false, want true")
	}
	if got := sectionTokens(t, composition, "system"); got != 5 {
		t.Fatalf("system = %d, want 5", got)
	}
	if got := sectionTokens(t, composition, "user_input"); got != 6 {
		t.Fatalf("user_input = %d, want 6", got)
	}
	if got := sectionTokens(t, composition, "history"); got != 11 {
		t.Fatalf("history = %d, want 11", got)
	}
	if sectionTokens(t, composition, "tools") == 0 {
		t.Fatal("tools = 0, want the tool schema to be counted")
	}
	if sectionTokens(t, composition, "memory") == 0 {
		t.Fatal("memory = 0, want the memory field to be counted")
	}
	if sectionTokens(t, composition, "other") == 0 {
		t.Fatal("other = 0, want model/temperature counted")
	}
	assertSectionsCover(t, composition)
}

func TestAnalyzePromptAnthropicSystemAndBlocks(t *testing.T) {
	body := `{
		"system": [{"type": "text", "text": "rules"}],
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "hi"}, {"type": "image", "source": {"type": "base64", "data": "x"}}]},
			{"role": "assistant", "content": "done"},
			{"role": "user", "content": "go"}
		]
	}`
	composition, err := AnalyzePrompt([]byte(body))
	if err != nil {
		t.Fatalf("AnalyzePrompt: %v", err)
	}
	if got := sectionTokens(t, composition, "system"); got != 2 {
		t.Fatalf("system = %d, want 2", got)
	}
	if got := sectionTokens(t, composition, "user_input"); got != 5 {
		t.Fatalf("user_input = %d, want 5", got)
	}
	if got := sectionTokens(t, composition, "history"); got < 1030 {
		t.Fatalf("history = %d, want the image block counted once", got)
	}
	assertSectionsCover(t, composition)
}

func TestAnalyzePromptGeminiContents(t *testing.T) {
	body := `{
		"system_instruction": {"parts": [{"text": "sys"}]},
		"contents": [
			{"role": "user", "parts": [{"text": "a"}]},
			{"role": "model", "parts": [{"text": "b"}]},
			{"role": "user", "parts": [{"text": "c"}]}
		]
	}`
	composition, err := AnalyzePrompt([]byte(body))
	if err != nil {
		t.Fatalf("AnalyzePrompt: %v", err)
	}
	if got := sectionTokens(t, composition, "system"); got != 1 {
		t.Fatalf("system = %d, want 1", got)
	}
	if got := sectionTokens(t, composition, "user_input"); got != 5 {
		t.Fatalf("user_input = %d, want 5", got)
	}
	if got := sectionTokens(t, composition, "history"); got != 9 {
		t.Fatalf("history = %d, want 9", got)
	}
	if got := sectionTokens(t, composition, "other"); got != 0 {
		t.Fatalf("other = %d, want 0", got)
	}
	assertSectionsCover(t, composition)
}

func TestAnalyzePromptStringInputIsUserInput(t *testing.T) {
	composition, err := AnalyzePrompt([]byte(`{"model":"m","input":"hello world"}`))
	if err != nil {
		t.Fatalf("AnalyzePrompt: %v", err)
	}
	if got := sectionTokens(t, composition, "user_input"); got != 4 {
		t.Fatalf("user_input = %d, want 4 (11 ASCII chars * 0.3)", got)
	}
	assertSectionsCover(t, composition)
}

func TestAnalyzePromptRejectsNonObject(t *testing.T) {
	for _, body := range []string{`[]`, `"x"`, `{`} {
		if _, err := AnalyzePrompt([]byte(body)); err == nil {
			t.Fatalf("AnalyzePrompt(%q) = nil error, want parse error", body)
		}
	}
}

func TestAnalyzePromptEmptyBodyHasZeroTotal(t *testing.T) {
	composition, err := AnalyzePrompt([]byte(`{}`))
	if err != nil {
		t.Fatalf("AnalyzePrompt: %v", err)
	}
	if composition.TotalTokens != 0 {
		t.Fatalf("total = %d, want 0", composition.TotalTokens)
	}
	for _, section := range composition.Sections {
		if section.Tokens != 0 || section.Share != 0 {
			t.Fatalf("section = %+v, want zero", section)
		}
	}
}

func assertSectionsCover(t *testing.T, composition PromptComposition) {
	t.Helper()
	if len(composition.Sections) != 6 {
		t.Fatalf("sections = %d, want 6", len(composition.Sections))
	}
	wantKeys := []string{"system", "tools", "memory", "history", "user_input", "other"}
	sum := 0
	share := 0.0
	for i, section := range composition.Sections {
		if section.Key != wantKeys[i] {
			t.Fatalf("section %d = %q, want %q", i, section.Key, wantKeys[i])
		}
		sum += section.Tokens
		share += section.Share
	}
	if sum != composition.TotalTokens {
		t.Fatalf("sections sum = %d, total = %d", sum, composition.TotalTokens)
	}
	if math.Abs(share-1) > 0.001 {
		t.Fatalf("shares sum = %v, want ~1", share)
	}
}

func TestAnalyzePromptCountsToolCallsAsHistory(t *testing.T) {
	body := `{"messages":[
		{"role":"user","content":"run it"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"do","arguments":"{\"x\":1}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"ok"},
		{"role":"user","content":"thanks"}
	]}`
	composition, err := AnalyzePrompt([]byte(body))
	if err != nil {
		t.Fatalf("AnalyzePrompt: %v", err)
	}
	history := sectionTokens(t, composition, "history")
	if history < 12 {
		t.Fatalf("history = %d, want tool call and result counted", history)
	}
	assertSectionsCover(t, composition)
}
