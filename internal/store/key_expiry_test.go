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

func TestClientKeyAllowedIPsRoundtrip(t *testing.T) {
	s := openTestStore(t)
	scope := ClientKeyScope{AllowedIPs: []string{"192.168.1.0/24", "10.0.0.5"}}
	if err := s.UpsertClientKeyScoped("k1", "laptop", "agk_a", "hash-1", true, 100, scope); err != nil {
		t.Fatalf("UpsertClientKeyScoped: %v", err)
	}
	key, ok, err := s.GetClientKey("k1")
	if err != nil || !ok {
		t.Fatalf("GetClientKey: ok=%v err=%v", ok, err)
	}
	if len(key.AllowedIPs) != 2 || key.AllowedIPs[0] != "192.168.1.0/24" || key.AllowedIPs[1] != "10.0.0.5" {
		t.Fatalf("allowed_ips = %v", key.AllowedIPs)
	}

	if err := s.SetClientKeyScope("k1", ClientKeyScope{AllowedIPs: []string{"not-an-ip"}}); err == nil {
		t.Fatalf("invalid allowed_ips must be rejected")
	}
	if err := s.SetClientKeyScope("k1", ClientKeyScope{}); err != nil {
		t.Fatalf("clear allowed_ips: %v", err)
	}
	key, _, _ = s.GetClientKey("k1")
	if len(key.AllowedIPs) != 0 {
		t.Fatalf("allowed_ips after clear = %v", key.AllowedIPs)
	}
}

func TestClientKeyOwnerRoundtrip(t *testing.T) {
	s := openTestStore(t)
	if err := s.UpsertClientKeyScoped("k1", "laptop", "agk_a", "hash-1", true, 100, ClientKeyScope{Owner: true}); err != nil {
		t.Fatalf("UpsertClientKeyScoped: %v", err)
	}
	key, ok, err := s.GetClientKey("k1")
	if err != nil || !ok || !key.Owner {
		t.Fatalf("owner = %v ok=%v err=%v", key.Owner, ok, err)
	}
	if err := s.SetClientKeyScope("k1", ClientKeyScope{Owner: false}); err != nil {
		t.Fatalf("SetClientKeyScope: %v", err)
	}
	key, _, _ = s.GetClientKey("k1")
	if key.Owner {
		t.Fatalf("owner should be cleared")
	}
}
