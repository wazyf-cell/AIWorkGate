package gateway

import (
	"encoding/json"
	"testing"
)

func TestStripDisabledThinking(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool // thinking field kept?
	}{
		{"disabled removed", `{"model":"m","thinking":{"type":"disabled"},"messages":[]}`, false},
		{"disabled case-insensitive", `{"model":"m","thinking":{"type":" DISABLED "}}`, false},
		{"enabled kept", `{"model":"m","thinking":{"type":"enabled","budget_tokens":1024}}`, true},
		{"absent", `{"model":"m","messages":[]}`, false},
		{"malformed thinking kept", `{"model":"m","thinking":"disabled"}`, true},
		{"invalid json unchanged", `not json`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := stripDisabledThinking([]byte(tc.body))
			if tc.body == "not json" {
				if string(out) != tc.body {
					t.Fatalf("invalid body changed: %q", out)
				}
				return
			}
			var top map[string]json.RawMessage
			if err := json.Unmarshal(out, &top); err != nil {
				t.Fatalf("output is not JSON: %v (%q)", err, out)
			}
			_, ok := top["thinking"]
			if ok != tc.want {
				t.Fatalf("thinking present = %v, want %v (body %s)", ok, tc.want, out)
			}
			if _, ok := top["model"]; !ok {
				t.Fatalf("model field lost: %s", out)
			}
		})
	}
}
