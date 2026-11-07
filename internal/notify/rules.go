package notify

// S5-4 / S7-5 应用侧告警规则引擎（内存态，随宿主进程生灭）。
//
// 设计：引擎与数据源解耦（可插拔输入）。宿主（internal/app）负责喂数：
//
//   - 规则 1（auth_spike）的输入是「认证失败」事件流。api_logs 行没有来源
//     IP 列，store 既有查询无法按 IP 聚合，所以计数不能靠回查日志；宿主在
//     /v1 网关入口包一层响应观察（401 即失败），把 (IP, 时刻) 推进来，引擎
//     内存滑窗计数。
//   - 规则 2（usage_spike）的输入是周期（5 分钟）评估：宿主按 key 查
//     api_logs 近 1 小时行数（store.Filter{KeyID, From, To} 的 COUNT 总数
//     与行数上限无关，精确），连同该 Key 的 daily_quota 推进来。
//
// 引擎只做判定与「同一事件只告一次」（episode 去重）：同一主体在异常持续
// 期间只发一条，观察值回落到阈值以下才重新武装；节流（MinInterval）由
// Dispatcher.Emit 承担，这里不重复。Emit 的 pluginID 参数传事件主体（IP /
// keyID），恰好把 Dispatcher 的 (kind, plugin) 节流键变成按主体节流：同一
// 主体持续异常每 MinInterval 至多一条，不同主体互不挤占。
//
// 已知口径（有意为之，非缺陷）：网关 D-42 在同一 IP 每分钟失败 10 次后改答
// 429，之后不再计 401——所以「慢速爆破」（<10 次/分钟）会被本规则完整捕捉，
// 而「急速爆破」在窗口内至少留下阈值以下的前 10 次 401，由 D-42 限速兜底；
// 若仍想覆盖急速场景，把宿主观察扩到 429 auth_rate_limited 需要识别响应体，
// 噪音（正常的每 IP 限速）会大于收益，本轮不做。
//
// 规则 3（设计记录，本轮不实现）：「凭据在异常时段被调用」按任务口径降级为
// 「7 天未用的 Key 突然被使用」。数据依赖：判定需要 last_used_at 的历史序列
// （最近 7 天每天是否用过），而 client_keys.last_used_at 只保留最近一次时间
// 戳，api_logs 的 key_id 行受保留窗口与配额门限影响不能当作完整使用史；
// P4 用量同步（usage 同步管线）落库后才有可靠历史。届时实现方式：评估时读
// 近 7 天按 key×天 的使用存在性查询（Store 侧需新增该聚合），对「7 天未用、
// 本窗口首次使用」的 Key 发事件（kind 届时拍板，可复用 usage_spike 或独立
// kind），复用本文件的 sink/episode 去重框架。

import (
	"fmt"
	"sync"
	"time"
)

// Rule thresholds. Deliberately constants this round: they describe an
// emergency, not a tunable preference, and a mis-tuned spike rule is either
// silent or noisy in a way the user cannot debug from numbers.
const (
	// AuthSpikeWindow is the sliding window for rule 1.
	AuthSpikeWindow = 5 * time.Minute
	// AuthSpikeThreshold is the per-IP failure count inside the window that
	// fires rule 1. Deliberately above the gateway's own D-42 per-minute
	// bucket (10/min): the alert marks a sustained attempt, not one typo.
	AuthSpikeThreshold = 20
	// UsageSpikeWindow is the evaluation window for rule 2.
	UsageSpikeWindow = time.Hour
	// UsageSpikeQuotaShare caps rule 2 at this share of the key's daily quota.
	UsageSpikeQuotaShare = 0.5
	// UsageSpikeAbsolute is the absolute bound of rule 2: the threshold is
	// min(50% of quota, this); unlimited keys use it directly.
	UsageSpikeAbsolute = 1000
)

// UsageSpikeThreshold computes rule 2's per-key threshold: min(50% of the key's
// daily quota, UsageSpikeAbsolute); a non-positive quota (unlimited) falls back
// to the absolute value, and the result never drops below 1 so a tiny quota
// still has a meaningful trigger point.
func UsageSpikeThreshold(dailyQuota int) int {
	limit := UsageSpikeAbsolute
	if dailyQuota > 0 {
		shared := int(float64(dailyQuota) * UsageSpikeQuotaShare)
		if shared < limit {
			limit = shared
		}
	}
	if limit < 1 {
		limit = 1
	}
	return limit
}

// RulesSink receives the events the rules fire. *Dispatcher satisfies it; the
// indirection keeps the engine testable without a delivery worker.
type RulesSink interface {
	Emit(pluginID string, event Event)
}

// RulesOptions wires a Rules engine. The engine keeps no clock of its own:
// every input (failure, usage observation) carries the observation time, so
// tests inject timestamps instead of faking time.Now.
type RulesOptions struct {
	// Sink receives fired events; nil means record-and-drop (counting still
	// works, which keeps the engine usable in hosts that only want counters).
	Sink RulesSink
	// AuthWindow/AuthThreshold/UsageWindow override the package constants
	// (tests inject tiny windows); zero values keep the defaults.
	AuthWindow    time.Duration
	AuthThreshold int
	UsageWindow   time.Duration
}

// Rules is the in-memory alert-rule engine. All state dies with the process:
// after a restart the windows refill, which is the correct semantics for
// "spike" alerts (a restart is not evidence the attack stopped, but the
// gateway's D-42 throttle is the actual protection; this layer only reports).
type Rules struct {
	sink          RulesSink
	authWindow    time.Duration
	authThreshold int
	usageWindow   time.Duration

	mu          sync.Mutex
	authHits    map[string][]int64 // ip → failure timestamps (unix seconds)
	authAlerted map[string]bool    // ip → an auth_spike fired and the window has not drained yet
	usageActive map[string]bool    // keyID → a usage_spike fired and usage has not fallen back yet
}

// NewRules builds the engine.
func NewRules(opts RulesOptions) *Rules {
	window := opts.AuthWindow
	if window <= 0 {
		window = AuthSpikeWindow
	}
	threshold := opts.AuthThreshold
	if threshold <= 0 {
		threshold = AuthSpikeThreshold
	}
	usageWindow := opts.UsageWindow
	if usageWindow <= 0 {
		usageWindow = UsageSpikeWindow
	}
	return &Rules{
		sink:          opts.Sink,
		authWindow:    window,
		authThreshold: threshold,
		usageWindow:   usageWindow,
		authHits:      map[string][]int64{},
		authAlerted:   map[string]bool{},
		usageActive:   map[string]bool{},
	}
}

// RecordAuthFailure feeds one failed /v1 authentication (rule 1). The caller
// owns the IP semantics: the app layer reports the direct peer address, so a
// reverse-proxy deployment collapses clients onto the proxy IP (documented
// limitation — TrustedProxies resolution lives inside the gateway).
// Thread-safe; evaluates immediately so the alert latency is one failure, not
// one poll interval.
func (r *Rules) RecordAuthFailure(ip string, at time.Time) {
	if r == nil || ip == "" {
		return
	}
	ts := at.Unix()

	// Evaluate under one lock; emit outside it so a slow webhook cannot stall
	// the caller that observed the failure.
	r.mu.Lock()
	hits := append(pruneBefore(r.authHits[ip], ts-int64(r.authWindow/time.Second)), ts)
	r.authHits[ip] = hits
	count := len(hits)
	threshold := r.authThreshold
	shouldEmit := false
	if count < threshold {
		// Below threshold: any open episode for this IP re-arms.
		delete(r.authAlerted, ip)
	} else if !r.authAlerted[ip] {
		r.authAlerted[ip] = true
		shouldEmit = true
	}
	sink := r.sink
	windowText := formatWindow(r.authWindow)
	r.mu.Unlock()

	if !shouldEmit || sink == nil {
		return
	}
	sink.Emit(ip, Event{
		Kind:  KindAuthSpike,
		Title: "认证失败激增",
		Body: fmt.Sprintf("来源 IP %s 在 %s 内认证失败 %d 次（阈值 %d），疑似密钥爆破，请检查该 IP 的客户端 Key。",
			ip, windowText, count, threshold),
		At: at,
	})
}

// RecordKeyUsage feeds one rule-2 observation for a key: hourCount is the
// request count inside the usage window, quota the key's daily_quota (0 =
// unlimited). The caller polls every few minutes; episode semantics match
// rule 1 (one alert while over threshold, re-arm on the first below-threshold
// observation). Thread-safe.
func (r *Rules) RecordKeyUsage(keyID, keyName string, hourCount, quota int, at time.Time) {
	if r == nil || keyID == "" {
		return
	}
	threshold := UsageSpikeThreshold(quota)
	if hourCount <= threshold {
		r.mu.Lock()
		delete(r.usageActive, keyID)
		r.mu.Unlock()
		return
	}

	r.mu.Lock()
	if r.usageActive[keyID] {
		r.mu.Unlock()
		return
	}
	r.usageActive[keyID] = true
	sink := r.sink
	windowText := formatWindow(r.usageWindow)
	r.mu.Unlock()

	if sink == nil {
		return
	}
	label := keyName
	if label == "" {
		label = keyID
	}
	sink.Emit(keyID, Event{
		Kind:  KindUsageSpike,
		Title: "Key 用量异常",
		Body: fmt.Sprintf("客户端 Key %s（%s）%s 内请求 %d 次，超过阈值 %d（日配额 %d 的 50%%，上限 %d）。请确认该 Key 是否被滥用。",
			label, keyID, windowText, hourCount, threshold, quota, UsageSpikeAbsolute),
		At: at,
	})
}

// PruneKeys drops rule-2 episode state for keys that no longer exist so a
// deleted key cannot hold memory (and cannot keep stale alert state if its id
// is later reused by a fresh key).
func (r *Rules) PruneKeys(live map[string]bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for keyID := range r.usageActive {
		if !live[keyID] {
			delete(r.usageActive, keyID)
		}
	}
}

// AuthHitCount reports the current window count for one IP (tests/diagnostics).
func (r *Rules) AuthHitCount(ip string) int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.authHits[ip])
}

// pruneBefore keeps only timestamps strictly after cutoff.
func pruneBefore(hits []int64, cutoff int64) []int64 {
	kept := hits[:0]
	for _, ts := range hits {
		if ts > cutoff {
			kept = append(kept, ts)
		}
	}
	return kept
}

// formatWindow renders a duration for the alert body (5 分钟 / 60 分钟).
func formatWindow(d time.Duration) string {
	minutes := int(d / time.Minute)
	if minutes < 1 {
		return "不足 1 分钟"
	}
	return fmt.Sprintf("%d 分钟", minutes)
}
