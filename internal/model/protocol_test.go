package model

import "testing"

func TestNormalizeProtocol(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", ProtocolOpenAI, true},
		{"openai", ProtocolOpenAI, true},
		{" OpenAI ", ProtocolOpenAI, true},
		{"openai-compatible", ProtocolOpenAI, true},
		{"openai_compatible", ProtocolOpenAI, true},
		{"anthropic", ProtocolAnthropic, true},
		{"Claude", ProtocolAnthropic, true},
		{"gemini", ProtocolGemini, true},
		{"google", ProtocolGemini, true},
		{"bogus", "", false},
	}
	for _, tc := range cases {
		got, ok := NormalizeProtocol(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("NormalizeProtocol(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
