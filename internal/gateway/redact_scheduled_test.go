package gateway

// TestRedactScheduledMaskOnce 锁定 scheduled 路径的入站脱敏是**单遍**调用。
//
// 背景：task-30 收到的「forwardScheduled/forwardPlugin 双重脱敏」经代码核对不成立——
// 两者是互斥分支（未调度/pluginWithoutAccounts 走 forwardPlugin；scheduled 走
// forwardScheduled→dispatch.attempt，根本不经过 forwardPlugin）。真正的风险是
// 若按字面删掉某一处，scheduled 路径会彻底失去脱敏。因此本测试固化不变式：
//
//   请求经 scheduled 路径时，最终写入请求日志的审计行必须是「真正脱了值的那一遍」
//   （含 "redact masked:" 且计数 >0）；若同一请求存在第二遍 n=0 的重跑，
//   审计行会被覆盖成 "redact: no values masked"，本测试必红。

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"aiworkgate/internal/model"
)

func TestRedactScheduledMaskOnce(t *testing.T) {
	up := &upBody{}
	srv, _ := newUpstreamServer(t, func(w http.ResponseWriter, _ *http.Request, b []byte) {
		appendBody(up)(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	})
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	g, _ := retryGateway(t, reg, singlePoolSnapshot("p1", 1, "acc1"), nil)
	g.redact = RedactOptions{On: true}

	var rows []model.APIRequestLog
	g.logs = sinkFunc(func(row model.APIRequestLog) { rows = append(rows, row) })

	// 与 redact_e2e_test.go 同包的 key/email 常量：已验证会被脱敏的形状
	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/m1","messages":[{"role":"user","content":"write to `+email+` using `+key+` now"}]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	// ① 上游拿到的是脱敏体（scheduled 路径确实在转发前脱敏）
	if bytes.Contains(up.b, []byte(key)) || bytes.Contains(up.b, []byte(email)) {
		t.Fatalf("上游收到原文：%s", up.b)
	}
	if !bytes.Contains(up.b, []byte("{{API_KEY_")) {
		t.Fatalf("上游未收到密钥脱敏占位符：%s", up.b)
	}

	// ② 审计行是「脱到值的那一遍」：含 masked 与计数，且不是被第二遍 n=0 覆盖
	if len(rows) == 0 {
		t.Fatal("没有产生日志行")
	}
	line := rows[0].Redact
	if !strings.Contains(line, "redact masked:") {
		t.Fatalf("审计行不是有效的脱敏结果（疑似被第二遍覆盖成 no values masked）：%q", line)
	}
	if strings.Contains(line, "no values masked") {
		t.Fatalf("审计行被 n=0 的第二遍覆盖：%q", line)
	}
}
