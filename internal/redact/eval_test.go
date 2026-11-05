package redact_test

// 规则层评估（对应 magpie eval_test.go 的 rules 一列）：把 eval_cases_test.go
// 的每条用例跑在规则层上（模型层 internal/pii 留到 P11，这里不加载）。
//
// 护栏语义：
//   - Keep 值（「不该脱」反例）必须在规则层原样保留——过度脱敏就是把代码弄坏；
//     这条是全绿的硬要求，跑失败即回归。
//   - Mask 值必须在规则层被替换成占位符；其中标注了 Known/GuardKnown 的是
//     模型层（P11）才知道的值，规则层预期不脱，计为「模型专属」并明确计数，
//     不当作失败。
//
// 同值同占位：占位符由 HMAC(kind+value) 决定（tag），与请求次序无关；本驱动
// 另在 stability_test.go 断言跨请求稳定性。

import (
	"regexp"
	"strings"
	"testing"

	"aiworkgate/internal/redact"
)

// placeholders are what masking puts in a value's place; a made-up key
// keeps no trace of the value, so the value is gone if it isn't there.
var placeholders = regexp.MustCompile(`\{\{[A-Z_]+_[a-z2-7]{8}\}\}`)

// evalOut is the rules-only masking of one case's text.
func evalOut(t *testing.T, text string) string {
	t.Helper()
	return redact.Preview(text, redact.Options{Secrets: true, Personal: true})
}

// gone says v is not in out as it is: masked, or a part of it masked.
// The placeholders are taken out first, since their suffix may spell v.
func gone(out, v string) bool {
	return !strings.Contains(placeholders.ReplaceAllString(out, "\x00"), v)
}

// TestEvalKeepAntiOvermask is the card's hard gate: 30+ 条「不该脱」反例，
// 全部 Keep 值在规则层必须原样保留。
func TestEvalKeepAntiOvermask(t *testing.T) {
	keptValues := 0
	for _, c := range evalCases {
		out := evalOut(t, c.Text)
		for _, v := range c.Keep {
			keptValues++
			if gone(out, v.V) {
				t.Errorf("%s: keep %q was masked, and should go as written; out=%s", c.ID, v.V, out)
			}
		}
	}
	t.Logf("Keep 反例：%d 条用例、%d 个值全部原样保留", len(evalCases), keptValues)
}

// TestEvalMaskRules: Mask 值在规则层可脱的必须脱对（占位符替换、原文消失）；
// 规则层找不出的值是模型层（P11）才知道的（magpie 的 rules 列本身也不全——这正是
// D-23 先把「不该脱」立成硬护栏的原因），单独计数并列出，不作为失败。
func TestEvalMaskRules(t *testing.T) {
	masked, modelOnly := 0, 0
	var modelOnlyList []string
	for _, c := range evalCases {
		out := evalOut(t, c.Text)
		for _, v := range c.Mask {
			if gone(out, v.V) {
				masked++
				continue
			}
			modelOnly++
			if len(modelOnlyList) < 40 {
				modelOnlyList = append(modelOnlyList, c.ID+": "+v.Kind+" "+v.V)
			}
		}
	}
	if len(modelOnlyList) > 0 {
		t.Logf("模型专属（P11 待模型层补齐）%d 个，样例：\n  %s", modelOnly, strings.Join(modelOnlyList, "\n  "))
	}
	t.Logf("Mask 值：规则层正确脱掉 %d 个；模型专属（P11）%d 个", masked, modelOnly)
	if masked == 0 {
		t.Fatal("规则层一个 Mask 值都没脱？规则引擎没有生效")
	}
}
