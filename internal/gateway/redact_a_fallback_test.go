package gateway

// TestRedactABranchSinglePass 锁定 task-30 修掉的唯一真实双重脱敏点：
// forwardScheduled 入口先脱敏一次，errNotScheduled 兜底走 A 时**过去**会把已脱敏
// 的 body 再交给 forwardA 脱第二遍（n=0，白烧 CPU 且把审计行覆盖成
// "redact: no values masked"，丢失真正那次脱敏的类别/条数）。
//
// 现在 forwardScheduled 把未脱敏的 origBody 交给 forwardA，A 自己只脱一遍：
//
//   ① A 收到的是脱敏体（保护不丢）；
//   ② 最终审计行必须是「脱到值的那一遍」（含 "redact masked:" 与计数），
//      而不是被第二遍 n=0 覆盖的 "redact: no values masked"。

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"aiworkgate/internal/model"
	"aiworkgate/internal/scheduler"
)

func TestRedactABranchSinglePass(t *testing.T) {
	up := &upBody{}
	aSrv, _ := newUpstreamServer(t, func(w http.ResponseWriter, _ *http.Request, b []byte) {
		appendBody(up)(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"a":true}`)
	})
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), "http://p1.invalid", "tok")

	// p1 的池**没有模型**：调度器不持有该名 → errNotScheduled → bareModelTarget
	// 无默认插件且 LegacyA 打开 → ok=false, err=nil → 走 forwardScheduled 的 A 兜底。
	snap := &scheduler.Snapshot{
		InterPool: scheduler.InterPoolSmart,
		Bindings:  map[string]scheduler.Binding{},
		Pools: map[string]*scheduler.Pool{
			"p1": {PluginID: "p1", Name: "p1", APIVersion: 2, UpstreamEnabled: true,
				Strategy: scheduler.StrategyFixed, Accounts: fixedPoolAccounts("acc1")},
		},
	}
	logs := &fakeLogSink{}
	g := newTestGateway(t, Options{
		Plugins:  reg,
		A:        &fakeA{base: aSrv.URL, key: "sk-a"},
		Log:      logs,
		Schedule: scheduler.New(scheduler.Options{}),
		Snapshot: staticSnapshot(snap),
		LegacyA:  true,
	})
	g.redact = RedactOptions{On: true}

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/m1","messages":[{"role":"user","content":"mail `+email+` key `+key+`"}]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get(sourceHeader); got != "a" {
		t.Fatalf("source header = %q, want a", got)
	}

	// ① A 收到脱敏体
	if bytes.Contains(up.b, []byte(key)) || bytes.Contains(up.b, []byte(email)) {
		t.Fatalf("A 收到原文：%s", up.b)
	}
	if !bytes.Contains(up.b, []byte("{{API_KEY_")) {
		t.Fatalf("A 未收到密钥脱敏占位符：%s", up.b)
	}

	// ② 审计行是脱到值的那一遍（不是被第二遍 n=0 覆盖）
	rows := logs.wait(t, 1)
	line := rows[0].Redact
	if !strings.Contains(line, "redact masked:") {
		t.Fatalf("审计行不是有效的脱敏结果（双重未去重 → 被 n=0 第二遍覆盖）：%q", line)
	}
	if strings.Contains(line, "no values masked") {
		t.Fatalf("审计行被第二遍覆盖：%q", line)
	}
}
