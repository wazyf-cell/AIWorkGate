package app

// S5-4 / S7-5 应用侧告警：宿主把两条规则的输入喂给 internal/notify 的内存
// 规则引擎，引擎命中后经既有 notify 通道（Dispatcher）推送。本文件只做数据
// 采集与周期调度，判定与去重在引擎里（见 internal/notify/rules.go 头注释，
// 含规则 3 的数据依赖说明）。
//
// 输入源（选型的取舍）：
//
//   - 规则 1（认证失败激增，按 IP）：api_logs 没有 IP 列，store 既有查询
//     无法按 IP 聚合，所以不回查日志，而是在 /v1 网关入口包一层响应观察：
//     serveGateway 包住 ResponseWriter 记下状态码，401 记一次失败
//     (RemoteAddr, now)。这是真实数据（客户端确实收到了 401），不造假；
//     已知口径见引擎头注释（D-42 之后急速爆破转 429 不再计数）与
//     clientIPForRules（反向代理下所有来源聚合为代理 IP）。
//   - 规则 2（单 Key 小时用量）：store 只读查询 api_logs——
//     store.Filter{KeyID, From, To} 的 COUNT 总数不受行数上限影响（精确），
//     每个启用 Key 一次小查询；UsedToday 预过滤让安静 Key 免于查询。

import (
	"net"
	"net/http"
	"sync"
	"time"

	"aiworkgate/internal/notify"
	"aiworkgate/internal/store"
)

// alertRulesInterval is how often rule 2 re-evaluates. Rule 1 evaluates on
// each failure (push); the poll only feeds rule 2 and prunes stale state.
// Five minutes bounds the alert latency on hourly windows without making the
// per-key COUNT queries noticeable.
const alertRulesInterval = 5 * time.Minute

// serveGateway forwards /v1 to the active gateway handler and, when the alert
// engine is wired, records every 401 answer into the auth-spike rule. The
// wrapper only observes the status code; the gateway's Flush-based streaming
// stays intact because statusResponseWriter implements http.Flusher.
func (a *App) serveGateway(w http.ResponseWriter, r *http.Request) {
	gw := a.currentGateway()
	if gw == nil {
		http.NotFound(w, r)
		return
	}
	if a.rules == nil {
		gw.ServeHTTP(w, r)
		return
	}
	sw := &statusResponseWriter{ResponseWriter: w}
	gw.ServeHTTP(sw, r)
	if sw.status() == http.StatusUnauthorized {
		a.rules.RecordAuthFailure(clientIPForRules(r), time.Now())
	}
}

// statusResponseWriter records the first status code written and delegates
// everything else, including Flusher (SSE paths assert it through the chain).
type statusResponseWriter struct {
	http.ResponseWriter

	mu           sync.Mutex
	wroteHeader  bool
	responseCode int
}

func (s *statusResponseWriter) WriteHeader(code int) {
	s.mu.Lock()
	if !s.wroteHeader {
		s.wroteHeader = true
		s.responseCode = code
	}
	s.mu.Unlock()
	s.ResponseWriter.WriteHeader(code)
}

// status returns the recorded response code; 200 when nothing was written
// explicitly (Go's implicit default), 401 never arrives that way because the
// gateway's fail path always calls WriteHeader.
func (s *statusResponseWriter) status() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.wroteHeader {
		return http.StatusOK
	}
	return s.responseCode
}

// Flush forwards so streaming handlers keep their flush semantics.
func (s *statusResponseWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// clientIPForRules extracts the peer host for the alert rule. It reads
// RemoteAddr directly and deliberately not the gateway's TrustedProxies
// resolution (internal/gateway owns that logic and must stay untouched), so
// behind a reverse proxy every client collapses onto the proxy IP — the alert
// then reports the proxy as the spiking source, which is still the right
// first thing for the user to look at.
func clientIPForRules(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return host
}

// alertRulesLoop feeds rule 2 while the host runs. Rule 1 needs no loop. The
// first pass waits a minute so startup fan-out (plugin discovery, rollup)
// settles before the first per-key queries.
func (a *App) alertRulesLoop() {
	select {
	case <-a.shutdownCh:
		return
	case <-time.After(time.Minute):
	}
	for {
		a.evaluateUsageSpikes()
		select {
		case <-a.shutdownCh:
			return
		case <-time.After(alertRulesInterval):
		}
	}
}

// evaluateUsageSpikes runs rule 2 once: per enabled key, the request count of
// the last usage window goes into the engine with the key's daily quota.
//
// 有意不用 client_key_usage 的 UsedToday 做免查询预过滤：那个计数只含计费请求
// （配额门的 billable 口径），而 api_logs 计所有带 key_id 的行——错误风暴
// （500/429 的失败请求）不进配额计数却正是要告警的滥用形态，预过滤会把这类
// 峰值漏掉。每个启用 Key 一次窗口内 COUNT（idx_api_logs_ts 先裁掉一小时之外
// 的行），Key 数量级是几十，代价可忽略。
func (a *App) evaluateUsageSpikes() {
	if a == nil || a.rules == nil || a.store == nil {
		return
	}
	keys, err := a.store.ListClientKeys()
	if err != nil {
		if a.logger != nil {
			a.logger.Warn("app", "usage-spike rule skipped: cannot list client keys", map[string]any{
				"error": err.Error(),
			})
		}
		return
	}
	now := time.Now()
	from := now.Add(-notify.UsageSpikeWindow).Unix()
	live := make(map[string]bool, len(keys))
	for _, key := range keys {
		live[key.ID] = true
		if !key.Enabled {
			// A disabled key cannot receive traffic; feeding it would only
			// re-arm stale episodes.
			continue
		}
		// The COUNT total ignores the row limit, so the number is exact even
		// for a spike far beyond one page.
		_, total, err := a.store.QueryAPIRequestLogs(store.Filter{
			From:     from,
			To:       now.Unix(),
			KeyID:    key.ID,
			NoBodies: true,
			Limit:    1,
		})
		if err != nil {
			if a.logger != nil {
				a.logger.Warn("app", "usage-spike rule skipped key", map[string]any{
					"key_id": key.ID, "error": err.Error(),
				})
			}
			continue
		}
		a.rules.RecordKeyUsage(key.ID, key.Name, total, key.DailyQuota, now)
	}
	a.rules.PruneKeys(live)
}
