package store

import "testing"

// P5-1: the key expiry survives a create and a scope update, and a negative
// value is rejected before it reaches SQLite.
func TestClientKeyExpiryRoundtrip(t *testing.T) {
	s := openTestStore(t)
	scope := ClientKeyScope{DailyQuota: 5, ExpiresAt: 1_792_651_200}
	if err := s.UpsertClientKeyScoped("k1", "laptop", "agk_a", "hash-1", true, 100, scope); err != nil {
		t.Fatalf("UpsertClientKeyScoped: %v", err)
	}
	keys, err := s.ListClientKeys()
	if err != nil {
		t.Fatalf("ListClientKeys: %v", err)
	}
	if len(keys) != 1 || keys[0].ExpiresAt != 1_792_651_200 {
		t.Fatalf("keys = %+v, want expires_at 1792651200", keys)
	}

	scope.ExpiresAt = 0
	if err := s.SetClientKeyScope("k1", scope); err != nil {
		t.Fatalf("SetClientKeyScope: %v", err)
	}
	keys, err = s.ListClientKeys()
	if err != nil || len(keys) != 1 || keys[0].ExpiresAt != 0 {
		t.Fatalf("keys after clear = %+v err=%v, want expires_at 0", keys, err)
	}

	if err := s.SetClientKeyScope("k1", ClientKeyScope{ExpiresAt: -1}); err == nil {
		t.Fatalf("negative expires_at must be rejected")
	}
}
