package redact

// P10 必做 ⑤ 的验收：审计只记 类别/条数/行号/耗时，绝不记原文；
// 同时是「邮箱被替换、色值/行号/版本号未伤」的证据测试。

import (
	"fmt"
	"strings"
	"testing"
)

// TestAuditRecordsNeverTheValue 是审计的字段纪律：AuditRecord 里任何可序列化的
// 内容都不得包含被脱敏的值、占位符或值的前缀。
func TestAuditRecordsNeverTheValue(t *testing.T) {
	var secret = "sk-ant-" + strings.Repeat("a1", 24) //nolint:gocritic // 运行期拼 48 字符 key
	const email = "zoe@example.dev"                   // 占位符形状的假邮箱值
	text := "hello\nlogin with " + secret + " or write to " + email + "\nbye"
	out, a := MaskAudit(text, Options{Secrets: true, Personal: true})
	if strings.Contains(out, secret) {
		t.Fatalf("secret 没被脱：%s", out)
	}
	if strings.Contains(out, email) {
		t.Fatalf("邮箱没被脱：%s", out)
	}
	recs := a.Records()
	if len(recs) < 2 {
		t.Fatalf("应有 >=2 类（SECRET/EMAIL），实际 %d 类 %+v", len(recs), recs)
	}
	for _, r := range recs {
		blob := fmt.Sprintf("%+v", r)
		for _, forbidden := range []string{secret, "sk-ant", email, "zoe", "44hrdvs2"} {
			if strings.Contains(blob, forbidden) {
				t.Errorf("审计行泄漏了值/占位/前缀：%q in %s", forbidden, blob)
			}
		}
		if r.Kind == "" || r.Count <= 0 {
			t.Errorf("类别/条数缺失：%+v", r)
		}
		if r.Line < 1 {
			t.Errorf("行号异常：%+v", r)
		}
		if r.Elapsed < 0 {
			t.Errorf("耗时异常：%+v", r)
		}
	}
	if a.Total() < 2 {
		t.Errorf("Total=%d，应 >=2", a.Total())
	}
	// 行号对照：secret（sk-ant-…，规则判为 API_KEY）与邮箱都在原文第 2 行
	var lineOf map[string]int
	lineOf = map[string]int{}
	var lineCats []string
	for _, r := range recs {
		lineOf[r.Kind] = r.Line
		lineCats = append(lineCats, r.Kind)
	}
	for kind, line := range lineOf {
		if line != 2 {
			t.Errorf("类别 %s 行号推算错误：%d（应有 2）；类别=%v", kind, line, lineCats)
		}
	}
}

// TestMaskDoesNotOverMask 是「不过度脱敏」的直接证据：色值/行号/版本号/尺寸/端口
// 在本机路径与代码语境里必须原样保留（与 eval_cases 的 Keep 同源）。
func TestMaskDoesNotOverMask(t *testing.T) {
	text := strings.Join([]string{
		"model.go:212:9: this is a compile error at 1624x1487 px",
		"color #1a2b3c is fine, so is v0.1.1513, and 127.0.0.1:443",
		"rg -n 'password|passwd|secret_key|api_token' --glob '*.go'",
		"cd /Users/jdoe/workspace/projects/magpie && git clone https://git@gitlab.acme-corp.cn/team/repo.git",
		"Use mcp__claude-in-chrome__navigate to open the page.",
		"Author: dependabot[bot] <49699333+dependabot[bot]@users.noreply.github.com>",
	}, "\n")
	out, _ := Mask(text, Options{Secrets: true, Personal: true})
	for _, keep := range []string{
		"model.go:212:9", "1624x1487", "#1a2b3c", "v0.1.1513", "127.0.0.1:443",
		"password|passwd|secret_key|api_token", "/Users/jdoe/workspace/projects/magpie",
		"git@gitlab.acme-corp.cn", "mcp__claude-in-chrome__navigate",
		"49699333+dependabot[bot]@users.noreply.github.com",
	} {
		if !strings.Contains(out, keep) {
			t.Errorf("误伤：%q 被改了；out=%s", keep, out)
		}
	}
}

// TestMaskJSONEvidence 是 MaskJSON 的证据：密钥字段被替换、JSON 结构完好、可逆。
func TestMaskJSONEvidence(t *testing.T) {
	var apiKey = "sk-proj-" + strings.Repeat("9", 32)
	body := `{"model":"gpt-5","authorization":"Bearer ` + apiKey + `","messages":[{"role":"user","content":"mail : ` + apiKey + `"}]}`
	out, n := MaskJSON([]byte(body), Options{Secrets: true})
	if n == 0 || strings.Contains(string(out), apiKey) {
		t.Fatalf("密钥未脱：%d %s", n, out)
	}
	if back := RestoreJSON(out); string(back) != body {
		t.Fatalf("不可逆对账失败：\nin :%s\nout:%s\nback:%s", body, out, back)
	}
}
