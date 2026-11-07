package gateway

import (
	"net/http"
	"strconv"
	"testing"
)

// D-42 回归：同一来源 IP 的认证失败到达阈值后，后续失败请求拿 429 + Retry-After
// 而不是再回 401（不再是 401 oracle）；有效 Key 不受影响。
func TestAuthFailuresRateLimitedAfterThreshold(t *testing.T) {
	reg, calls1, _ := chatPluginServers(t, scopedChatReply)
	g := newTestGateway(t, Options{Plugins: reg})

	// authFailLimit - 1 次失败仍是 401。
	for i := 0; i < authFailLimit-1; i++ {
		rr := serve(g, newChatRequest(t, "/v1/chat/completions", "agk_wrong", `{"model":"p1/alpha","messages":[]}`))
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: status = %d, want 401", i+1, rr.Code)
		}
	}
	// 第 authFailLimit 次失败仍 401（allow 先记录并放行），之后开始 429。
	rr := serve(g, newChatRequest(t, "/v1/chat/completions", "agk_wrong", `{"model":"p1/alpha","messages":[]}`))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("failure %d: status = %d, want 401", authFailLimit, rr.Code)
	}
	rr = serve(g, newChatRequest(t, "/v1/chat/completions", "agk_wrong", `{"model":"p1/alpha","messages":[]}`))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("failure %d: status = %d, want 429", authFailLimit+1, rr.Code)
	}
	if got := rr.Header().Get("Retry-After"); got == "" {
		t.Fatal("429 must carry Retry-After")
	} else if _, err := strconv.Atoi(got); err != nil {
		t.Fatalf("Retry-After = %q, want seconds", got)
	}

	// 有效 Key 的请求不受同 IP 失败限速影响。
	rr = serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, `{"model":"p1/alpha","messages":[]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("valid key after failures: status = %d (body %s)", rr.Code, rr.Body.String())
	}
	if calls1.count() != 1 {
		t.Fatalf("plugin calls = %d, want 1", calls1.count())
	}
}
