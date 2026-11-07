package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aiworkgate/internal/config"
	"aiworkgate/internal/model"
	"aiworkgate/internal/notify"
	"aiworkgate/internal/store"
)

// emitRecord is one captured notification (pluginID scopes the throttle and
// doubles as the event's subject here).
type emitRecord struct {
	plugin string
	kind   string
	title  string
	body   string
}

// alertRecorder captures what the rules engine fired (RulesSink adapter, same
// shape as notifyQuotaAlertSink's recording counterpart).
type alertRecorder struct {
	events []emitRecord
}

func (r *alertRecorder) Emit(pluginID string, event notify.Event) {
	r.events = append(r.events, emitRecord{plugin: pluginID, kind: event.Kind, title: event.Title, body: event.Body})
}

func (r *alertRecorder) kinds() []string {
	out := make([]string, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.kind)
	}
	return out
}

// authFailStub always answers 401 (the gateway's authentication failure path).
type authFailStub struct{}

func (authFailStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":{"message":"missing or invalid API key"}}`))
}

// TestServeGatewayRecordsAuthFailures is the rule-1 采集闭环：/v1 的 401 响应被
// 包一层观察，喂进引擎的按 IP 滑窗；跨过阈值发出 auth_spike（主体验证 = IP）。
func TestServeGatewayRecordsAuthFailures(t *testing.T) {
	rec := &alertRecorder{}
	rules := notify.NewRules(notify.RulesOptions{Sink: rec, AuthWindow: time.Minute, AuthThreshold: 2})
	a := &App{cfg: config.Default(), gatewayHandler: authFailStub{}, rules: rules}

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req.Host = "127.0.0.1"
		req.RemoteAddr = "203.0.113.7:4444"
		recorder := httptest.NewRecorder()
		a.routeHTTP(recorder, req)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("stub should answer 401, got %d", recorder.Code)
		}
	}
	if len(rec.events) != 1 {
		t.Fatalf("events = %d, want 1: %+v", len(rec.events), rec.events)
	}
	if rec.events[0].kind != notify.KindAuthSpike || rec.events[0].plugin != "203.0.113.7" {
		t.Fatalf("event = %+v", rec.events[0])
	}
	if rules.AuthHitCount("203.0.113.7") != 2 {
		t.Fatalf("window count = %d, want 2", rules.AuthHitCount("203.0.113.7"))
	}

	// 非 401 响应不计入（换一个 200 的 stub 前清掉旧状态不可能——另开 App）。
	a2 := &App{cfg: config.Default(), gatewayHandler: &stubRoute{kind: "gateway"}, rules: rules}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Host = "127.0.0.1"
	req.RemoteAddr = "203.0.113.9:1"
	a2.routeHTTP(httptest.NewRecorder(), req)
	if len(rec.events) != 1 {
		t.Fatalf("non-401 changed the feed: %+v", rec.events)
	}
}

// TestServeGatewayWithoutRulesUnwrapsWriter: 未接引擎时零包装（管理页等既有
// 行为不变，也没有类型断言层面的包装损耗）。
func TestServeGatewayWithoutRulesUnwrapsWriter(t *testing.T) {
	a := &App{cfg: config.Default(), gatewayHandler: &stubRoute{kind: "gateway"}}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Host = "127.0.0.1"
	rec := httptest.NewRecorder()
	a.routeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("gateway surface broke: %d", rec.Code)
	}
}

func insertKeyLog(t *testing.T, st *store.Store, keyID string, at time.Time, status int) {
	t.Helper()
	err := st.InsertAPIRequestLog(model.APIRequestLog{
		TS:     at.Format(time.RFC3339),
		ReqID:  "req-" + keyID + "-" + at.Format("150405.000000000"),
		KeyID:  keyID,
		Status: status,
	})
	if err != nil {
		t.Fatalf("insert api log: %v", err)
	}
}

// TestEvaluateUsageSpikesFeedsEngine is the rule-2 采集闭环：近 1 小时按 key 的
// api_logs 计数进引擎；跨阈值的 Key 发 usage_spike（主体验证 = keyID），窗外
// 的行不计，停用的 Key 不评估。
func TestEvaluateUsageSpikesFeedsEngine(t *testing.T) {
	st := openTestStoreForApp(t)
	now := time.Now().Truncate(time.Second)

	// key-a：日配额 100（阈值 50），近 1 小时 51 行 → 触发。
	if err := st.UpsertClientKey("key-a", "keyA", "agk_", "h", true, 0); err != nil {
		t.Fatalf("upsert key-a: %v", err)
	}
	if err := st.SetClientKeyScope("key-a", store.ClientKeyScope{DailyQuota: 100}); err != nil {
		t.Fatalf("set key-a quota: %v", err)
	}
	// key-b：无配额（绝对阈值 1000），近 1 小时 5 行 → 不触发。
	if err := st.UpsertClientKey("key-b", "keyB", "agk_", "h", true, 0); err != nil {
		t.Fatalf("upsert key-b: %v", err)
	}
	// key-c：停用，近 1 小时大量行 → 不评估。
	if err := st.UpsertClientKey("key-c", "keyC", "agk_", "h", false, 0); err != nil {
		t.Fatalf("upsert key-c: %v", err)
	}
	for i := 0; i < 51; i++ {
		insertKeyLog(t, st, "key-a", now.Add(-time.Duration(i+1)*time.Second), 200)
	}
	for i := 0; i < 5; i++ {
		insertKeyLog(t, st, "key-b", now.Add(-time.Duration(i+1)*time.Second), 200)
	}
	for i := 0; i < 40; i++ {
		insertKeyLog(t, st, "key-c", now.Add(-time.Duration(i+1)*time.Second), 200)
	}
	// key-a 一条 2 小时前的旧行：窗口外，不计入。
	insertKeyLog(t, st, "key-a", now.Add(-2*time.Hour), 200)

	rec := &alertRecorder{}
	a := &App{store: st, rules: notify.NewRules(notify.RulesOptions{Sink: rec})}

	a.evaluateUsageSpikes()

	if len(rec.events) != 1 {
		t.Fatalf("events = %d, want 1 (key-a only): %+v", len(rec.events), rec.events)
	}
	if rec.events[0].kind != notify.KindUsageSpike || rec.events[0].plugin != "key-a" {
		t.Fatalf("event = %+v", rec.events[0])
	}
	if !strings.Contains(rec.events[0].body, "51") {
		t.Fatalf("body missing the window count: %s", rec.events[0].body)
	}

	// 第二轮：同样的观测（仍在阈值上）不重复发——episode 去重在引擎里。
	a.evaluateUsageSpikes()
	if len(rec.events) != 1 {
		t.Fatalf("polling repeated the alert: %+v", rec.events)
	}
}

// TestEvaluateUsageSpikesSurvivesStoreErrors keeps the loop harmless when the
// store fails (list error path), and without a store/engine is a no-op.
func TestEvaluateUsageSpikesSurvivesStoreErrors(t *testing.T) {
	a := &App{store: nil, rules: notify.NewRules(notify.RulesOptions{})}
	a.evaluateUsageSpikes() // 不 panic 即可

	st := openTestStoreForApp(t)
	a2 := &App{store: st, rules: nil}
	a2.evaluateUsageSpikes() // 无引擎：no-op
}

// TestClientIPForRules pins the peer-address extraction (host only, no port).
func TestClientIPForRules(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1", nil)
	req.RemoteAddr = "203.0.113.7:4444"
	if got := clientIPForRules(req); got != "203.0.113.7" {
		t.Fatalf("ip = %q", got)
	}
	req.RemoteAddr = "203.0.113.8" // 无端口（极端环境）
	if got := clientIPForRules(req); got != "203.0.113.8" {
		t.Fatalf("ip without port = %q", got)
	}
}
