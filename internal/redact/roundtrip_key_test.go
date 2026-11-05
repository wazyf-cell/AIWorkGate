package redact

// P10-3（task-24）缺陷锁定：真实形态密钥 Mask→Restore 必须还原回原值
// （P10 验收硬线「客户端看不到任何占位符」）。
//
// 真实缺陷场景（task-21 e2e 发现）：第一轮明文 → 占位符；第二轮（agent 历史
// 重放 / 上游回显）文本已含 {{KIND_指纹}}，再 Mask 时占位符本身不得被当作待脱值
// 二次登记，Restore 必须仍还原回明文。先红后绿。

import (
	"strings"
	"testing"
)

const probeKey = "sk-ant-" + "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678901"

func TestRestoreKeyRoundtripFirstPass(t *testing.T) {
	// 第一轮：明文 → 占位符
	text := "login with " + probeKey + " now"
	out, n := Mask(text, Options{Secrets: true, Personal: true})
	if n < 1 {
		t.Fatalf("第一轮未脱敏（n=%d）", n)
	}
	if back := Restore(out, false); back != text {
		t.Errorf("第一轮往返失败：\n  in : %s\n  out: %s\n  back: %s", text, out, back)
	}
}

func TestRestoreKeyRoundtripSecondPass(t *testing.T) {
	// 第一轮：明文 → 占位符文本
	text := "login with " + probeKey + " now"
	first, _ := Mask(text, Options{Secrets: true, Personal: true})

	// 第二轮：含占位符的文本（agent 历史重放）再次 Mask ——
	// 占位符必须不被二次登记，输出应仍保持同一层占位符
	second, n2 := Mask(first, Options{Secrets: true, Personal: true})
	if n2 != 0 {
		t.Errorf("第二轮不应再脱敏（n=%d）：\n  first : %s\n  second: %s", n2, first, second)
	}
	if back := Restore(second, false); back != text {
		t.Errorf("第二轮往返失败（占位符被二次登记）：\n  text   : %s\n  second : %s\n  back   : %s", text, second, back)
	}
}

func TestRestoreKeyRoundtripJSONSecondPass(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"token ` + probeKey + `"}]}`)
	first, n := MaskJSON(body, Options{Secrets: true, Personal: true})
	if n < 1 {
		t.Fatalf("第一轮未脱敏（n=%d）", n)
	}
	if back := RestoreJSON(first); string(back) != string(body) {
		t.Fatalf("第一轮往返失败：\n  in : %s\n  out: %s\n  back: %s", body, first, back)
	}
	// 第二轮：历史重放含占位符
	second, n2 := MaskJSON(first, Options{Secrets: true, Personal: true})
	if n2 != 0 {
		t.Errorf("第二轮不应再脱敏（n=%d）：\n  first : %s\n  second: %s", n2, first, second)
	}
	if back := RestoreJSON(second); string(back) != string(body) {
		t.Fatalf("第二轮往返失败：\n  in : %s\n  out: %s\n  back: %s", body, second, back)
	}
}

func TestRestoreKeyRoundtripKeyShapes(t *testing.T) {
	// sk-ant- / sk- / ghp_ / gho_ 等真实形态
	for _, secret := range []string{
		"sk-ant-" + strings.Repeat("a1", 22),
		"sk-" + strings.Repeat("x", 28),
		"ghp_" + strings.Repeat("c3", 20),
		"gho_" + strings.Repeat("d4", 18),
	} {
		text := "key " + secret
		out, n := Mask(text, Options{Secrets: true, Personal: true})
		if n < 1 {
			t.Fatalf("%s: 未脱敏（n=%d）", secret, n)
		}
		if back := Restore(out, false); back != text {
			t.Errorf("密钥往返失败：\n  in : %s\n  out: %s\n  back: %s", text, out, back)
		}
	}
}