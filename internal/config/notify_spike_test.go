package config

import (
	"strings"
	"testing"
)

// TestDefaultNotifyEventsExcludesSpikeKinds pins the S5-4/S7-5 订阅口径：两个
// 应用侧告警 kind（auth_spike/usage_spike）在闭集里可订阅，但不在默认订阅集
// ——缺省不推送（避免爆破/用量噪音），用户在设置页手动勾选或改 config.json。
func TestDefaultNotifyEventsExcludesSpikeKinds(t *testing.T) {
	if !notifyEvents["auth_spike"] || !notifyEvents["usage_spike"] {
		t.Fatal("closed set must accept the two spike kinds (subscribable)")
	}
	for _, kind := range DefaultNotifyEvents {
		if kind == "auth_spike" || kind == "usage_spike" {
			t.Fatalf("default subscription must not include %s (opt-in only)", kind)
		}
	}
	// 默认集仍然完整（既有 kind 一个不少）。
	for _, want := range []string{"checkin_fail", "checkin_done", "account_dead",
		"quota_low", "quota_expiring", "quota_resetting"} {
		found := false
		for _, kind := range DefaultNotifyEvents {
			if kind == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("default subscription lost %s", want)
		}
	}
}

// TestNormalizeKeepsSpikeSubscription: 手动订阅的两个新 kind 过 Normalize 不被
// 当作未知事件丢弃（大小写/空白归一后保留）。
func TestNormalizeKeepsSpikeSubscription(t *testing.T) {
	cfg := Default()
	cfg.Notify.Enabled = true
	cfg.Notify.Events = []string{"Auth_Spike ", "usage_spike", "quota_low"}
	cfg.Normalize()
	got := strings.Join(cfg.Notify.Events, ",")
	if !strings.Contains(got, "auth_spike") || !strings.Contains(got, "usage_spike") {
		t.Fatalf("Normalize dropped the spike subscription: %v", cfg.Notify.Events)
	}
	if !strings.Contains(got, "quota_low") {
		t.Fatalf("Normalize dropped a plain kind: %v", cfg.Notify.Events)
	}
}
