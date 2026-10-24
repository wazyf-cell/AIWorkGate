package store

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestReserveKeyUsageIsAtomic ports A's single-lock admission (auth.rs:74-91,
// api_keys.rs:327-390): racing reservations can never overshoot the quota.
func TestReserveKeyUsageIsAtomic(t *testing.T) {
	s := openTestStore(t)
	const (
		keyID = "k1"
		day   = "2026-10-21"
		quota = 10
		races = 50
	)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		reserved int
	)
	for i := 0; i < races; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, _, err := s.ReserveKeyUsage(keyID, day, quota)
			if err != nil {
				t.Errorf("ReserveKeyUsage: %v", err)
				return
			}
			if ok {
				mu.Lock()
				reserved++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if reserved != quota {
		t.Fatalf("reserved = %d, want exactly %d", reserved, quota)
	}
	if used, err := s.KeyUsageRequests(keyID, day); err != nil || used != quota {
		t.Fatalf("stored requests = %d, err=%v, want %d", used, err, quota)
	}
	// The refused reservation reports the counter so the 429 can name it.
	ok, used, err := s.ReserveKeyUsage(keyID, day, quota)
	if err != nil || ok || used != quota {
		t.Fatalf("ReserveKeyUsage at the limit = (%v, %d, %v), want (false, %d, nil)", ok, used, err, quota)
	}

	// A released slot is reusable and the counter never goes negative.
	if err := s.ReleaseKeyUsageReservation(keyID, day); err != nil {
		t.Fatalf("ReleaseKeyUsageReservation: %v", err)
	}
	if used, err := s.KeyUsageRequests(keyID, day); err != nil || used != quota-1 {
		t.Fatalf("after release = %d, err=%v, want %d", used, err, quota-1)
	}
	if ok, _, err := s.ReserveKeyUsage(keyID, day, quota); err != nil || !ok {
		t.Fatalf("reserve after release = (%v, %v), want granted", ok, err)
	}
	for i := 0; i < quota+5; i++ {
		_ = s.ReleaseKeyUsageReservation(keyID, day)
	}
	if used, err := s.KeyUsageRequests(keyID, day); err != nil || used != 0 {
		t.Fatalf("after over-release = %d, err=%v, want 0", used, err)
	}

	// quota 0 is unlimited: every reservation is granted and counted.
	if ok, _, err := s.ReserveKeyUsage("k2", day, 0); err != nil || !ok {
		t.Fatalf("unlimited reserve = (%v, %v)", ok, err)
	}
	if used, err := s.KeyUsageRequests("k2", day); err != nil || used != 1 {
		t.Fatalf("unlimited counter = %d, err=%v, want 1", used, err)
	}

	// Validation mirrors IncrementKeyUsage.
	if _, _, err := s.ReserveKeyUsage("k1", "21/10/2026", quota); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("ReserveKeyUsage(bad day) = %v, want ErrInvalidArgument", err)
	}
	if _, _, err := s.ReserveKeyUsage("", day, quota); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("ReserveKeyUsage(no key) = %v, want ErrInvalidArgument", err)
	}
	if err := s.ReleaseKeyUsageReservation("k1", ""); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("ReleaseKeyUsageReservation(no day) = %v, want ErrInvalidArgument", err)
	}
}

func TestClientKeyScopeRoundTrip(t *testing.T) {
	s := openTestStore(t)
	scope := ClientKeyScope{
		ModelScope: []string{" p/* ", "m", "m"},
		BindPool:   "trae",
		DailyQuota: 10,
	}
	if err := s.UpsertClientKeyScoped("k1", "laptop", "agk_a", "hash-1", true, 100, scope); err != nil {
		t.Fatalf("UpsertClientKeyScoped: %v", err)
	}

	key, ok, err := s.GetClientKey("k1")
	if err != nil || !ok {
		t.Fatalf("GetClientKey = %+v, ok=%v, err=%v", key, ok, err)
	}
	if got, want := strings.Join(key.ModelScope, ","), "p/*,m"; got != want {
		t.Fatalf("ModelScope = %q, want %q (trimmed and de-duplicated)", got, want)
	}
	if key.BindPool != "trae" || key.DailyQuota != 10 {
		t.Fatalf("scope = %+v", key)
	}
	if key.UsedToday != 0 || key.QuotaLeft != 10 {
		t.Fatalf("usage = used %d left %d, want 0/10", key.UsedToday, key.QuotaLeft)
	}

	list, err := s.ListClientKeys()
	if err != nil || len(list) != 1 {
		t.Fatalf("ListClientKeys = %+v, err=%v", list, err)
	}
	if got := list[0]; got.BindPool != "trae" || got.DailyQuota != 10 || len(got.ModelScope) != 2 {
		t.Fatalf("listed key = %+v", got)
	}

	if _, ok, err := s.GetClientKey("missing"); err != nil || ok {
		t.Fatalf("GetClientKey(missing) = ok=%v err=%v, want not found", ok, err)
	}
}

func TestIncrementKeyUsageAccumulatesAndSeparatesDays(t *testing.T) {
	s := openTestStore(t)
	const keyID = "k1"
	credits := 0.5
	if err := s.IncrementKeyUsage(keyID, "2026-10-20", 1, 10, 5, 2, &credits); err != nil {
		t.Fatalf("increment 1: %v", err)
	}
	if err := s.IncrementKeyUsage(keyID, "2026-10-20", 1, 3, 1, 0, nil); err != nil {
		t.Fatalf("increment 2: %v", err)
	}
	if err := s.IncrementKeyUsage(keyID, "2026-10-21", 2, 0, 0, 0, nil); err != nil {
		t.Fatalf("increment other day: %v", err)
	}

	usage, ok, err := s.KeyUsage(keyID, "2026-10-20")
	if err != nil || !ok {
		t.Fatalf("KeyUsage day 1 = %+v ok=%v err=%v", usage, ok, err)
	}
	if usage.Requests != 2 || usage.PromptTokens != 13 || usage.CompletionTokens != 6 || usage.CachedTokens != 2 {
		t.Fatalf("day 1 usage = %+v", usage)
	}
	if usage.Credits == nil || *usage.Credits != 0.5 {
		t.Fatalf("day 1 credits = %v, want 0.5 kept when the second request reports none", usage.Credits)
	}

	other, ok, err := s.KeyUsage(keyID, "2026-10-21")
	if err != nil || !ok {
		t.Fatalf("KeyUsage day 2 = %+v ok=%v err=%v", other, ok, err)
	}
	if other.Requests != 2 || other.PromptTokens != 0 || other.Credits != nil {
		t.Fatalf("day 2 must be isolated: %+v", other)
	}

	if got, err := s.KeyUsageRequests(keyID, "2026-10-20"); err != nil || got != 2 {
		t.Fatalf("KeyUsageRequests = %d, err=%v", got, err)
	}
	if got, err := s.KeyUsageRequests(keyID, "2026-09-01"); err != nil || got != 0 {
		t.Fatalf("KeyUsageRequests(empty day) = %d, err=%v", got, err)
	}
}

func TestListClientKeysUsesOnlyToday(t *testing.T) {
	s := openTestStore(t)
	scope := ClientKeyScope{DailyQuota: 5}
	if err := s.UpsertClientKeyScoped("k1", "laptop", "agk_a", "hash-1", true, 100, scope); err != nil {
		t.Fatalf("UpsertClientKeyScoped: %v", err)
	}
	today := ClientKeyUsageDay(time.Now())
	yesterday := ClientKeyUsageDay(time.Now().AddDate(0, 0, -1))
	if err := s.IncrementKeyUsage("k1", today, 2, 7, 3, 0, nil); err != nil {
		t.Fatalf("increment today: %v", err)
	}
	if err := s.IncrementKeyUsage("k1", yesterday, 9, 90, 90, 0, nil); err != nil {
		t.Fatalf("increment yesterday: %v", err)
	}

	list, err := s.ListClientKeys()
	if err != nil || len(list) != 1 {
		t.Fatalf("ListClientKeys = %+v, err=%v", list, err)
	}
	if list[0].UsedToday != 2 {
		t.Fatalf("UsedToday = %d, want 2 (yesterday's row must not leak)", list[0].UsedToday)
	}
	if list[0].QuotaLeft != 3 {
		t.Fatalf("QuotaLeft = %d, want 3", list[0].QuotaLeft)
	}
	if got, err := s.KeyUsageToday("k1"); err != nil || got != 2 {
		t.Fatalf("KeyUsageToday = %d, err=%v", got, err)
	}

	// Unlimited keys report -1 so the UI can render 不限 instead of 0.
	if err := s.UpsertClientKeyScoped("k2", "desktop", "agk_b", "hash-2", true, 200, ClientKeyScope{}); err != nil {
		t.Fatalf("UpsertClientKeyScoped(unlimited): %v", err)
	}
	if key, _, _ := s.GetClientKey("k2"); key.QuotaLeft != -1 {
		t.Fatalf("unlimited QuotaLeft = %d, want -1", key.QuotaLeft)
	}
}

func TestClientKeyScopeValidation(t *testing.T) {
	s := openTestStore(t)
	if err := s.UpsertClientKey("k1", "laptop", "agk_a", "hash-1", true, 100); err != nil {
		t.Fatalf("UpsertClientKey: %v", err)
	}

	tooMany := make([]string, MaxClientKeyModelScope+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("m%d", i)
	}
	cases := map[string]ClientKeyScope{
		"too many entries": {ModelScope: tooMany},
		"entry too long":   {ModelScope: []string{strings.Repeat("x", MaxClientKeyScopeRunes+1)}},
		"negative quota":   {DailyQuota: -1},
		"quota too large":  {DailyQuota: MaxClientKeyDailyQuota + 1},
		"bind pool long":   {BindPool: strings.Repeat("p", MaxClientKeyBindPoolRunes+1)},
	}
	for name, scope := range cases {
		t.Run(name, func(t *testing.T) {
			if err := s.SetClientKeyScope("k1", scope); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("SetClientKeyScope = %v, want ErrInvalidArgument", err)
			}
		})
	}

	if err := s.SetClientKeyScope("missing", ClientKeyScope{DailyQuota: 1}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetClientKeyScope(missing) = %v, want ErrNotFound", err)
	}
	if err := s.UpdateClientKey("missing", "x", true, ClientKeyScope{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateClientKey(missing) = %v, want ErrNotFound", err)
	}

	if err := s.UpdateClientKey("k1", "renamed", false, ClientKeyScope{ModelScope: []string{"p/*"}}); err != nil {
		t.Fatalf("UpdateClientKey: %v", err)
	}
	key, _, _ := s.GetClientKey("k1")
	if key.Name != "renamed" || key.Enabled || len(key.ModelScope) != 1 {
		t.Fatalf("updated key = %+v", key)
	}

	if err := s.IncrementKeyUsage("k1", "21/10/2026", 1, 0, 0, 0, nil); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("IncrementKeyUsage(bad day) = %v, want ErrInvalidArgument", err)
	}
	negative := -1.0
	if err := s.IncrementKeyUsage("k1", "2026-10-21", 1, 0, 0, 0, &negative); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("IncrementKeyUsage(negative credits) = %v, want ErrInvalidArgument", err)
	}
}
