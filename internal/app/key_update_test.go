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

func TestCreateKeyPersistsLimits(t *testing.T) {
	app := newCustomModelApp(t)
	st := app.store
	expires := time.Now().Add(24 * time.Hour).Unix()
	_, key, err := app.createKeySpec(webui.KeySpec{
		Name: "friend", Enabled: true,
		ModelScope: []string{"p/*"}, BindPool: "p", DailyQuota: 3,
		ExpiresAt: expires, AllowedIPs: []string{"192.168.1.0/24"},
		Owner: true, MaxRequestsTotal: 9, Share: true,
	})
	if err != nil {
		t.Fatalf("createKeySpec: %v", err)
	}
	stored, ok, err := st.GetClientKey(key.ID)
	if err != nil || !ok {
		t.Fatalf("GetClientKey: ok=%v err=%v", ok, err)
	}
	if stored.ExpiresAt != expires || len(stored.AllowedIPs) != 1 || stored.AllowedIPs[0] != "192.168.1.0/24" ||
		!stored.Owner || stored.MaxRequestsTotal != 9 || !stored.Share ||
		stored.BindPool != "p" || stored.DailyQuota != 3 || len(stored.ModelScope) != 1 {
		t.Fatalf("stored key = %+v", stored)
	}
}
