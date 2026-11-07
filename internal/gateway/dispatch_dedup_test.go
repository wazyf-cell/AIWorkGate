package gateway

import (
	"net/http"
	"strings"
	"sync"
	"testing"

	"aiworkgate/internal/model"
	"aiworkgate/internal/scheduler"
)

// D-46①: when the same unattributed failure (403/5xx with an identical error
// signature) hits several candidate accounts inside one dispatch round, only
// the first report parks its account; the later duplicates carry NoPenalty.

// fakeSchedCooldowns is a minimal scheduler.CooldownStore for gateway tests.
type fakeSchedCooldowns struct {
	mu   sync.Mutex
	rows map[cooldownRowKey]scheduler.CooldownRecord
}

type cooldownRowKey struct {
	plugin, account, model string
}

func (f *fakeSchedCooldowns) LoadCooldowns(int64) ([]scheduler.CooldownRecord, error) {
	return nil, nil
}

func (f *fakeSchedCooldowns) SaveCooldown(record scheduler.CooldownRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[cooldownRowKey{record.PluginID, record.AccountID, record.Model}] = record
	return nil
}

func (f *fakeSchedCooldowns) DeleteCooldown(plugin, account, model string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows, cooldownRowKey{plugin, account, model})
	return nil
}

func (f *fakeSchedCooldowns) accounts() map[string]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]bool{}
	for row := range f.rows {
		out[row.account] = true
	}
	return out
}

func TestDispatchDedupSkipsSecondStrike(t *testing.T) {
	const body = `{"error":{"message":"forbidden by upstream policy"}}`
	srv, _ := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(body))
	})
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	snap := singlePoolSnapshot("p1", 2, "acc1", "acc2")
	store := &fakeSchedCooldowns{rows: map[cooldownRowKey]scheduler.CooldownRecord{}}
	sched := scheduler.New(scheduler.Options{Cooldowns: store})
	g := newTestGateway(t, Options{
		Plugins:  reg,
		Schedule: sched,
		Snapshot: staticSnapshot(snap),
	})

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/m1","messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d (body %s), want the replayed 403", rr.Code, rr.Body.String())
	}

	sched.FlushCooldowns()
	parked := store.accounts()
	if len(parked) != 1 || !parked["acc1"] {
		t.Fatalf("parked accounts = %v, want exactly acc1 (identical second failure must not park acc2)", parked)
	}
	selectable, total := sched.SelectableCount(snap)
	if selectable != 1 || total != 2 {
		t.Fatalf("selectable = %d/%d, want 1/2 (only the first signature hit cools down)", selectable, total)
	}
}

// TestDispatchDistinctErrorsCoolBothAccounts guards against over-dedup: two
// different error signatures are two account failures and both accounts park.
func TestDispatchDistinctErrorsCoolBothAccounts(t *testing.T) {
	srv, _ := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		if r.Header.Get(accountHeader) == "acc1" {
			_, _ = w.Write([]byte(`{"error":{"message":"key revoked"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"error":{"message":"region blocked for this tenant"}}`))
	})
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "m1"}), srv.URL, "tok")
	snap := singlePoolSnapshot("p1", 2, "acc1", "acc2")
	store := &fakeSchedCooldowns{rows: map[cooldownRowKey]scheduler.CooldownRecord{}}
	sched := scheduler.New(scheduler.Options{Cooldowns: store})
	g := newTestGateway(t, Options{
		Plugins:  reg,
		Schedule: sched,
		Snapshot: staticSnapshot(snap),
	})

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/m1","messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d (body %s)", rr.Code, rr.Body.String())
	}
	sched.FlushCooldowns()
	parked := store.accounts()
	if len(parked) != 2 {
		t.Fatalf("parked accounts = %v, want both (distinct signatures must each count)", parked)
	}
}

func TestFailureSignatureNormalization(t *testing.T) {
	same := []struct {
		a, b string
	}{
		{"  Forbidden\n\t ACCESS   Denied  ", "forbidden access denied"},
		{"line one\r\nline two", "line one line two"},
		{strings.Repeat("x", 200) + "-A", strings.Repeat("x", 200) + "-B"},
	}
	for i, pair := range same {
		if failureSignature(403, pair.a) != failureSignature(403, pair.b) {
			t.Fatalf("pair %d: signatures differ, want dedup", i)
		}
	}
	if failureSignature(403, "boom") == failureSignature(500, "boom") {
		t.Fatal("status must be part of the signature")
	}
	if failureSignature(403, "boom") == failureSignature(403, "different") {
		t.Fatal("distinct messages must not share a signature")
	}
}
