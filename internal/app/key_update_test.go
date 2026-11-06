package app

import (
	"testing"
	"time"

	"aiworkgate/internal/webui"
)

func TestUpdateKeyPersistsLimits(t *testing.T) {
	app, st := newSchedulerApp(t)
	if err := st.UpsertClientKey("ck_1", "laptop", "agk_abcd", "hash", true, 1_700_000_000); err != nil {
		t.Fatalf("UpsertClientKey: %v", err)
	}
	expires := time.Now().Add(time.Hour).Unix()
	_, err := app.updateKey("ck_1", webui.KeySpec{
		Name: "laptop", Enabled: true,
		ModelScope: []string{"p/*"}, BindPool: "p", DailyQuota: 10,
		ExpiresAt: expires, AllowedIPs: []string{"10.0.0.0/8"},
		Owner: true, MaxRequestsTotal: 5, Share: true,
	})
	if err != nil {
		t.Fatalf("updateKey: %v", err)
	}
	key, ok, err := st.GetClientKey("ck_1")
	if err != nil || !ok {
		t.Fatalf("GetClientKey: ok=%v err=%v", ok, err)
	}
	if key.ExpiresAt != expires || len(key.AllowedIPs) != 1 || key.AllowedIPs[0] != "10.0.0.0/8" ||
		!key.Owner || key.MaxRequestsTotal != 5 || !key.Share ||
		key.BindPool != "p" || key.DailyQuota != 10 || len(key.ModelScope) != 1 {
		t.Fatalf("updated key = %+v", key)
	}
}
