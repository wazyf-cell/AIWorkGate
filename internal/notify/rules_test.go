package notify

import (
	"strings"
	"testing"
	"time"
)

// recordingSink captures what the engine fired: Emit(pluginID, Event).
type recordingSink struct {
	events []emitRecord
}

type emitRecord struct {
	plugin string
	kind   string
	title  string
	body   string
}

func (r *recordingSink) Emit(pluginID string, event Event) {
	r.events = append(r.events, emitRecord{plugin: pluginID, kind: event.Kind, title: event.Title, body: event.Body})
}

func (r *recordingSink) kinds() []string {
	out := make([]string, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.kind)
	}
	return out
}

// TestUsageSpikeThreshold pins the 取小 rule: min(50% quota, 1000); unlimited
// quota falls back to the absolute; a tiny quota clamps at 1.
func TestUsageSpikeThreshold(t *testing.T) {
	cases := []struct {
		quota int
		want  int
	}{
		{0, 1000},    // unlimited → absolute
		{-5, 1000},   // negative treated as unlimited
		{1000, 500},  // 50% under the absolute
		{3000, 1000}, // 50% (1500) above the absolute → 取小
		{100, 50},    // exact share
		{1, 1},       // share floors to 0, clamp keeps a trigger point
		{1998, 999},  // 50% exactly
	}
	for _, tc := range cases {
		if got := UsageSpikeThreshold(tc.quota); got != tc.want {
			t.Errorf("quota %d → threshold %d, want %d", tc.quota, got, tc.want)
		}
	}
}

// TestAuthSpikeBelowThresholdDoesNotEmit keeps quiet IPs silent.
func TestAuthSpikeBelowThresholdDoesNotEmit(t *testing.T) {
	sink := &recordingSink{}
	now := time.Unix(1_800_000_000, 0)
	r := NewRules(RulesOptions{Sink: sink, AuthWindow: 5 * time.Minute, AuthThreshold: 3})

	for i := 0; i < 2; i++ {
		r.RecordAuthFailure("10.0.0.1", now.Add(time.Duration(i)*time.Second))
	}
	if len(sink.events) != 0 {
		t.Fatalf("below threshold fired: %+v", sink.events)
	}
	if r.AuthHitCount("10.0.0.1") != 2 {
		t.Fatalf("window count = %d, want 2", r.AuthHitCount("10.0.0.1"))
	}
}

// TestAuthSpikeFiresOncePerEpisode: 跨过阈值发一条；异常持续期间（后续失败）不
// 重复；窗口滑出后重新武装，会再发。
func TestAuthSpikeFiresOncePerEpisode(t *testing.T) {
	sink := &recordingSink{}
	now := time.Unix(1_800_000_000, 0)
	r := NewRules(RulesOptions{Sink: sink, AuthWindow: 5 * time.Minute, AuthThreshold: 3})

	// 第 3 次失败跨阈值：发一条。
	for i := 0; i < 3; i++ {
		r.RecordAuthFailure("10.0.0.1", now.Add(time.Duration(i)*time.Second))
	}
	if len(sink.events) != 1 {
		t.Fatalf("crossing threshold fired %d events, want 1: %+v", len(sink.events), sink.events)
	}
	first := sink.events[0]
	if first.plugin != "10.0.0.1" || first.kind != KindAuthSpike {
		t.Fatalf("event = %+v (plugin 应为主体验证 key)", first)
	}
	if !strings.Contains(first.body, "10.0.0.1") || !strings.Contains(first.body, "3 次") {
		t.Fatalf("body does not name the IP and count: %s", first.body)
	}

	// 异常持续：第 4、5 次失败不再发。
	r.RecordAuthFailure("10.0.0.1", now.Add(4*time.Second))
	r.RecordAuthFailure("10.0.0.1", now.Add(5*time.Second))
	if len(sink.events) != 1 {
		t.Fatalf("episode repeated: %+v", sink.events)
	}

	// 窗口滑出（5 分钟窗，7 分钟后只剩 0 个旧失败）→ 重新武装。
	r.RecordAuthFailure("10.0.0.1", now.Add(7*time.Minute))
	if r.AuthHitCount("10.0.0.1") != 1 {
		t.Fatalf("stale hits not pruned: %d", r.AuthHitCount("10.0.0.1"))
	}
	r.RecordAuthFailure("10.0.0.1", now.Add(7*time.Minute+time.Second))
	r.RecordAuthFailure("10.0.0.1", now.Add(7*time.Minute+2*time.Second))
	if len(sink.events) != 2 {
		t.Fatalf("episode did not re-arm after the window drained: %+v", sink.kinds())
	}
	if second := sink.events[1]; second.plugin != "10.0.0.1" || second.kind != KindAuthSpike {
		t.Fatalf("second event = %+v", second)
	}
}

// TestAuthSpikePerIPIndependence: 不同来源 IP 互不挤占（各自计数、各自去重）。
func TestAuthSpikePerIPIndependence(t *testing.T) {
	sink := &recordingSink{}
	now := time.Unix(1_800_000_000, 0)
	r := NewRules(RulesOptions{Sink: sink, AuthWindow: 5 * time.Minute, AuthThreshold: 2})

	r.RecordAuthFailure("10.0.0.1", now)
	r.RecordAuthFailure("10.0.0.1", now.Add(time.Second))
	r.RecordAuthFailure("10.0.0.2", now)
	r.RecordAuthFailure("10.0.0.2", now.Add(time.Second))
	if len(sink.events) != 2 {
		t.Fatalf("events = %d, want one per IP: %+v", len(sink.events), sink.events)
	}
	if sink.events[0].plugin == sink.events[1].plugin {
		t.Fatalf("both events share one plugin scope: %+v", sink.events)
	}
}

// TestUsageSpikeFiresAndRearms: 越阈值发一条；持续越线不重复；回落到阈值以下
// 重新武装。
func TestUsageSpikeFiresAndRearms(t *testing.T) {
	sink := &recordingSink{}
	now := time.Unix(1_800_000_000, 0)
	r := NewRules(RulesOptions{Sink: sink}) // 默认窗长只影响文案

	// quota 100 → 阈值 50；51 次越过。
	r.RecordKeyUsage("key-1", "测试 Key", 51, 100, now)
	if len(sink.events) != 1 {
		t.Fatalf("spike not fired: %+v", sink.events)
	}
	first := sink.events[0]
	if first.kind != KindUsageSpike || first.plugin != "key-1" {
		t.Fatalf("event = %+v", first)
	}
	if !strings.Contains(first.body, "测试 Key") || !strings.Contains(first.body, "51") || !strings.Contains(first.body, "50") {
		t.Fatalf("body missing key/count/threshold: %s", first.body)
	}

	// 持续越线（轮询每 5 分钟喂一次）：不重复。
	r.RecordKeyUsage("key-1", "测试 Key", 66, 100, now.Add(5*time.Minute))
	if len(sink.events) != 1 {
		t.Fatalf("episode repeated: %+v", sink.events)
	}

	// 回落到阈值以下 → 重新武装。
	r.RecordKeyUsage("key-1", "测试 Key", 10, 100, now.Add(10*time.Minute))
	r.RecordKeyUsage("key-1", "测试 Key", 51, 100, now.Add(15*time.Minute))
	if len(sink.events) != 2 {
		t.Fatalf("episode did not re-arm: %+v", sink.kinds())
	}

	// 恰好等于阈值不触发（超过才触发）。
	r.RecordKeyUsage("key-1", "测试 Key", 10, 100, now.Add(20*time.Minute))
	r.RecordKeyUsage("key-1", "测试 Key", 50, 100, now.Add(25*time.Minute))
	if len(sink.events) != 2 {
		t.Fatalf("threshold boundary triggered: %+v", sink.events)
	}
}

// TestUsageSpikeUnlimitedQuotaUsesAbsolute: 无配额 Key 用绝对值 1000 兜底。
func TestUsageSpikeUnlimitedQuotaUsesAbsolute(t *testing.T) {
	sink := &recordingSink{}
	now := time.Unix(1_800_000_000, 0)
	r := NewRules(RulesOptions{Sink: sink})

	r.RecordKeyUsage("k", "", 1000, 0, now) // 等于绝对值：不触发
	if len(sink.events) != 0 {
		t.Fatalf("boundary triggered: %+v", sink.events)
	}
	r.RecordKeyUsage("k", "", 1001, 0, now.Add(time.Minute)) // 超过：触发
	if len(sink.events) != 1 {
		t.Fatalf("absolute threshold not applied: %+v", sink.events)
	}
	if !strings.Contains(sink.events[0].body, "k（") {
		t.Fatalf("unnamed key should fall back to the id: %s", sink.events[0].body)
	}
}

// TestRulesNilSinkCountsQuietly: 无 sink 只计数不告警（宿主未配通知时无害），
// 且滑窗把窗外时间戳剪掉（30s 间隔共 25 个，最后只剩 5 分钟内的 10 个）。
func TestRulesNilSinkCountsQuietly(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	r := NewRules(RulesOptions{})
	for i := 0; i < 25; i++ {
		r.RecordAuthFailure("10.9.9.9", now.Add(time.Duration(i)*30*time.Second))
	}
	if r.AuthHitCount("10.9.9.9") != 10 {
		t.Fatalf("count = %d, want 10 (5 分钟窗保留)", r.AuthHitCount("10.9.9.9"))
	}
	r.RecordKeyUsage("k", "", 5000, 0, now)
	// 不 panic 即可（去重态已置位）。
}

// TestPruneKeysDropsDeletedKeys: 删掉的 Key 不再占用去重态。
func TestPruneKeysDropsDeletedKeys(t *testing.T) {
	sink := &recordingSink{}
	now := time.Unix(1_800_000_000, 0)
	r := NewRules(RulesOptions{Sink: sink})
	r.RecordKeyUsage("gone", "", 2000, 0, now)
	if len(sink.events) != 1 {
		t.Fatalf("setup did not fire: %+v", sink.events)
	}
	r.PruneKeys(map[string]bool{"live": true})
	// gone 已删：同 id 再越过阈值（新 Key 复用 id）应能再次告警。
	r.RecordKeyUsage("gone", "", 2000, 0, now.Add(time.Minute))
	if len(sink.events) != 2 {
		t.Fatalf("prune did not reset the episode: %+v", sink.events)
	}
	// live 保留：不重复告警。
	r.RecordKeyUsage("live", "", 2000, 0, now)
	r.RecordKeyUsage("live", "", 2100, 0, now.Add(time.Minute))
	if len(sink.events) != 3 {
		t.Fatalf("live episode was pruned: %+v", sink.events)
	}
}

// TestNewKindsAreSubscribable pins the notify.Kinds contract: 两个新 kind 必须
// 可订阅（ValidKind）且在 Kinds 列表里；默认订阅是否含它们由 config 层拍板
// （见 config 包测试 TestDefaultNotifyEventsExcludesSpikeKinds）。
func TestNewKindsAreSubscribable(t *testing.T) {
	if !ValidKind(KindAuthSpike) || !ValidKind(KindUsageSpike) {
		t.Fatal("new kinds not accepted by ValidKind")
	}
	found := map[string]bool{}
	for _, k := range Kinds {
		found[k] = true
	}
	if !found[KindAuthSpike] || !found[KindUsageSpike] {
		t.Fatalf("Kinds list incomplete: %v", Kinds)
	}
}
