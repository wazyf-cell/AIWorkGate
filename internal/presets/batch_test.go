package presets

// D-30④ 批量覆盖语法解析器单测：基础值 · 模型=值, 模型=值。

import (
	"strings"
	"testing"
)

func TestParseBatchOverrides(t *testing.T) {
	cases := []struct {
		name string
		spec string
		want BatchOverride
		err  string
	}{
		{
			name: "base plus two models",
			spec: "128k · gpt-4o=200k, gemini-2.5-pro=1m",
			want: BatchOverride{Base: "128k", Models: map[string]string{
				"gpt-4o":         "200k",
				"gemini-2.5-pro": "1m",
			}},
		},
		{
			name: "no base, single model",
			spec: "claude=1m",
			want: BatchOverride{Models: map[string]string{"claude": "1m"}},
		},
		{
			name: "base only, no models",
			spec: "200k · ",
			want: BatchOverride{Base: "200k"},
		},
		{
			name: "empty spec",
			spec: "",
			want: BatchOverride{},
		},
		{
			name: "whitespace tolerated",
			spec: "  64k  ·  m1 = 128k ,  m2 = 256k ",
			want: BatchOverride{Base: "64k", Models: map[string]string{"m1": "128k", "m2": "256k"}},
		},
		{name: "no equals", spec: "128k · gpt-4o", err: "段必须含"},
		{name: "empty model", spec: "128k · =200k", err: "模型名不能为空"},
		{name: "empty value", spec: "128k · gpt-4o=", err: "值不能为空"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseBatchOverrides(tc.spec)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if got.Base != tc.want.Base {
				t.Errorf("base = %q, want %q", got.Base, tc.want.Base)
			}
			if len(got.Models) != len(tc.want.Models) {
				t.Fatalf("models = %v, want %v", got.Models, tc.want.Models)
			}
			for k, wantV := range tc.want.Models {
				if got.Models[k] != wantV {
					t.Errorf("models[%q] = %q, want %q", k, got.Models[k], wantV)
				}
			}
		})
	}
}

func TestApplyBatchOverride(t *testing.T) {
	o, err := ParseBatchOverrides("128k · gpt-4o=200k")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("parsed base=%q models=%v", o.Base, o.Models)
	if got := ApplyBatchOverride(o, "gpt-4o", "128k"); got != "200k" {
		t.Errorf("gpt-4o override = %q, want 200k", got)
	}
	if got := ApplyBatchOverride(o, "claude", "128k"); got != "128k" {
		t.Errorf("claude falls back to base = %q, want 128k", got)
	}
}
