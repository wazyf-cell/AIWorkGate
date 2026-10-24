package gateway

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"aiworkgate/internal/model"
	"aiworkgate/internal/store"
)

// keyUsageCall records one Options.KeyUsage invocation.
type keyUsageCall struct {
	keyID      string
	day        string
	requests   int
	prompt     int
	completion int
	cached     int
	credits    *float64
}

// staticKeyRecord returns a KeyRecord callback that always resolves to kr.
func staticKeyRecord(kr KeyRecord) func(string) (KeyRecord, bool, error) {
	return func(string) (KeyRecord, bool, error) { return kr, true, nil }
}

// chatPluginServers returns two plugin upstreams plus a registry wiring them.
func chatPluginServers(t *testing.T, reply string) (*fakeRegistry, *upstreamRecorder, *upstreamRecorder) {
	t.Helper()
	srv1, calls1 := newUpstreamServer(t, jsonResponder(http.StatusOK, reply))
	srv2, calls2 := newUpstreamServer(t, jsonResponder(http.StatusOK, reply))
	reg := newFakeRegistry().
		add(pluginStatus("p1", true, model.ModelInfo{ID: "alpha"}, model.ModelInfo{ID: "beta"}), srv1.URL, "tok-p1").
		add(pluginStatus("p2", true, model.ModelInfo{ID: "gamma"}), srv2.URL, "tok-p2")
	return reg, calls1, calls2
}

const scopedChatReply = `{"id":"chatcmpl-1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":6}}}`

func TestModelsFilteredByKeyScope(t *testing.T) {
	reg, _, _ := chatPluginServers(t, scopedChatReply)
	a := &fakeA{models: []model.ModelInfo{{ID: "bare-1"}, {ID: "bare-2"}}}
	g := newTestGateway(t, Options{
		Plugins: reg,
		A:       a,
		KeyRecord: staticKeyRecord(KeyRecord{
			Enabled:    true,
			ModelScope: []string{"p1/*", "bare-1"},
		}),
	})

	rr := serve(g, newGetRequest(t, "/v1/models", clientKey))
	if rr.Code != http.StatusOK {
		t.Fatalf("models = %d, body %s", rr.Code, rr.Body.String())
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	decodeInto(t, rr.Body.Bytes(), &payload)
	ids := make([]string, 0, len(payload.Data))
	for _, entry := range payload.Data {
		ids = append(ids, entry.ID)
	}
	if got, want := strings.Join(ids, ","), "p1/alpha,p1/beta,bare-1"; got != want {
		t.Fatalf("model ids = %q, want %q", got, want)
	}
}

func TestKeyScopeRejectsModelBeforeRouting(t *testing.T) {
	reg, calls1, calls2 := chatPluginServers(t, scopedChatReply)

	t.Run("outside scope", func(t *testing.T) {
		g := newTestGateway(t, Options{
			Plugins:   reg,
			KeyRecord: staticKeyRecord(KeyRecord{Enabled: true, ModelScope: []string{"p2/*"}}),
		})
		rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
			`{"model":"p1/alpha","messages":[]}`))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body %s)", rr.Code, rr.Body.String())
		}
		payload := decodeError(t, rr)
		if payload.Error.Code != "model_not_found" {
			t.Fatalf("code = %q", payload.Error.Code)
		}
		if !strings.Contains(payload.Error.Message, "Key") {
			t.Fatalf("message must name the key restriction: %q", payload.Error.Message)
		}
		if calls1.count() != 0 || calls2.count() != 0 {
			t.Fatalf("upstreams called: %d/%d", calls1.count(), calls2.count())
		}
	})

	t.Run("wildcard allows the pool", func(t *testing.T) {
		g := newTestGateway(t, Options{
			Plugins:   reg,
			KeyRecord: staticKeyRecord(KeyRecord{Enabled: true, ModelScope: []string{"p1/*"}}),
		})
		rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
			`{"model":"p1/alpha","messages":[]}`))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
		}
		if calls1.count() != 1 {
			t.Fatalf("plugin p1 calls = %d, want 1", calls1.count())
		}
	})

	t.Run("exact bare A model", func(t *testing.T) {
		aSrv, aCalls := newUpstreamServer(t, jsonResponder(http.StatusOK, scopedChatReply))
		a := &fakeA{base: aSrv.URL, key: "sk-a"}
		g := newTestGateway(t, Options{
			Plugins:   reg,
			A:         a,
			KeyRecord: staticKeyRecord(KeyRecord{Enabled: true, ModelScope: []string{"bare-1"}}),
		})
		rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
			`{"model":"bare-1","messages":[]}`))
		if rr.Code != http.StatusOK || aCalls.count() != 1 {
			t.Fatalf("status = %d, A calls = %d (body %s)", rr.Code, aCalls.count(), rr.Body.String())
		}
		rr = serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
			`{"model":"bare-2","messages":[]}`))
		if rr.Code != http.StatusNotFound || aCalls.count() != 1 {
			t.Fatalf("unlisted bare model: status = %d, A calls = %d", rr.Code, aCalls.count())
		}
	})
}

func TestKeyBindPoolRoutesBareModel(t *testing.T) {
	reg, calls1, calls2 := chatPluginServers(t, scopedChatReply)
	g := newTestGateway(t, Options{
		Plugins:   reg,
		KeyRecord: staticKeyRecord(KeyRecord{Enabled: true, BindPool: "p1"}),
	})

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"alpha","messages":[]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("bare bound model = %d (body %s)", rr.Code, rr.Body.String())
	}
	if calls1.count() != 1 || calls2.count() != 0 {
		t.Fatalf("bound calls: p1=%d p2=%d", calls1.count(), calls2.count())
	}
	up := calls1.last(t)
	var sent struct {
		Model string `json:"model"`
	}
	decodeInto(t, up.Body, &sent)
	if sent.Model != "alpha" {
		t.Fatalf("upstream model = %q, want the bare name", sent.Model)
	}

	rr = serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/beta","messages":[]}`))
	if rr.Code != http.StatusOK || calls1.count() != 2 {
		t.Fatalf("explicit bound prefix = %d, calls = %d", rr.Code, calls1.count())
	}

	rr = serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p2/gamma","messages":[]}`))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("foreign prefix = %d, want 404", rr.Code)
	}
	if msg := decodeError(t, rr).Error.Message; !strings.Contains(msg, "绑定") {
		t.Fatalf("message must explain the binding: %q", msg)
	}
	if calls2.count() != 0 {
		t.Fatalf("p2 must not be called, got %d", calls2.count())
	}

	ghost := newTestGateway(t, Options{
		Plugins:   reg,
		KeyRecord: staticKeyRecord(KeyRecord{Enabled: true, BindPool: "ghost"}),
	})
	rr = serve(ghost, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"alpha","messages":[]}`))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("missing bound plugin = %d, want 404", rr.Code)
	}
	if msg := decodeError(t, rr).Error.Message; !strings.Contains(msg, "未安装") {
		t.Fatalf("message must mention the missing plugin: %q", msg)
	}
}

func TestKeyQuotaRejectsWithRetryAfter(t *testing.T) {
	reg, calls1, _ := chatPluginServers(t, scopedChatReply)
	used := 3
	var seenDay string
	g := newTestGateway(t, Options{
		Plugins:       reg,
		KeyRecord:     staticKeyRecord(KeyRecord{Enabled: true, DailyQuota: 3}),
		KeyUsageToday: func(_ string, day string) (int, error) { seenDay = day; return used, nil },
	})
	fixed := time.Date(2026, 10, 21, 23, 59, 30, 0, time.Local)
	g.now = func() time.Time { return fixed }

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/alpha","messages":[]}`))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body %s)", rr.Code, rr.Body.String())
	}
	payload := decodeError(t, rr)
	if payload.Error.Code != "key_quota_exceeded" {
		t.Fatalf("code = %q", payload.Error.Code)
	}
	if got := rr.Header().Get("Retry-After"); got != "30" {
		t.Fatalf("Retry-After = %q, want 30 (until local midnight)", got)
	}
	if seenDay != "2026-10-21" {
		t.Fatalf("quota day = %q", seenDay)
	}
	if calls1.count() != 0 {
		t.Fatalf("quota rejection reached the plugin")
	}

	// One request left: allowed.
	used = 2
	rr = serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/alpha","messages":[]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("under quota = %d (body %s)", rr.Code, rr.Body.String())
	}

	// Accounting trouble fails open instead of blocking traffic.
	broken := newTestGateway(t, Options{
		Plugins:       reg,
		KeyRecord:     staticKeyRecord(KeyRecord{Enabled: true, DailyQuota: 1}),
		KeyUsageToday: func(string, string) (int, error) { return 0, errors.New("db closed") },
	})
	rr = serve(broken, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/alpha","messages":[]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("fail-open status = %d", rr.Code)
	}
}

func TestKeyUsageCountsRequestsAndTokens(t *testing.T) {
	reg, _, _ := chatPluginServers(t, scopedChatReply)
	var calls []keyUsageCall
	g := newTestGateway(t, Options{
		Plugins:   reg,
		KeyRecord: staticKeyRecord(KeyRecord{Enabled: true, ModelScope: []string{"p1/*"}}),
		KeyUsage: func(keyID, day string, requests, prompt, completion, cached int, credits *float64) {
			calls = append(calls, keyUsageCall{keyID, day, requests, prompt, completion, cached, credits})
		},
	})
	g.now = func() time.Time { return time.Date(2026, 10, 21, 10, 0, 0, 0, time.Local) }

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/alpha","messages":[]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("chat = %d (body %s)", rr.Code, rr.Body.String())
	}
	if len(calls) != 1 {
		t.Fatalf("usage calls = %d, want 1", len(calls))
	}
	first := calls[0]
	if first.keyID != "key-1" || first.day != "2026-10-21" || first.requests != 1 {
		t.Fatalf("usage call = %+v", first)
	}
	if first.prompt != 11 || first.completion != 7 || first.cached != 6 {
		t.Fatalf("tokens = %d/%d cached %d, want 11/7 cached 6", first.prompt, first.completion, first.cached)
	}

	// A scope-rejected request never reaches a model, so it must not consume
	// the daily quota (otherwise a typo in the model name burns budget).
	rr = serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p2/gamma","messages":[]}`))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("rejected chat = %d", rr.Code)
	}
	if len(calls) != 1 {
		t.Fatalf("usage calls after a scope rejection = %d, want 1", len(calls))
	}

	// An admitted request that fails upstream still counts, without tokens.
	g2 := newTestGateway(t, Options{
		Plugins:   reg,
		KeyRecord: staticKeyRecord(KeyRecord{Enabled: true, ModelScope: []string{"p1/*"}}),
		KeyUsage: func(keyID, day string, requests, prompt, completion, cached int, credits *float64) {
			calls = append(calls, keyUsageCall{keyID, day, requests, prompt, completion, cached, credits})
		},
	})
	g2.now = g.now
	rr = serve(g2, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/missing","messages":[]}`))
	if rr.Code == http.StatusOK {
		t.Fatalf("missing model unexpectedly succeeded")
	}
	if len(calls) != 2 {
		t.Fatalf("usage calls after an upstream failure = %d, want 2", len(calls))
	}

	// With an installed reserver the slot is consumed at admission, so the
	// completion call must only add tokens/credits (requests = 0) and must not
	// count the request again.
	reserver := &countingReserver{}
	useReserver(t, reserver)
	g3 := newTestGateway(t, Options{
		Plugins:   reg,
		KeyRecord: staticKeyRecord(KeyRecord{ID: "key-1", Enabled: true, DailyQuota: 5}),
		KeyUsage: func(keyID, day string, requests, prompt, completion, cached int, credits *float64) {
			calls = append(calls, keyUsageCall{keyID, day, requests, prompt, completion, cached, credits})
		},
	})
	g3.now = g.now
	rr = serve(g3, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/alpha","messages":[]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("reserved chat = %d (body %s)", rr.Code, rr.Body.String())
	}
	if len(calls) != 3 {
		t.Fatalf("usage calls after a reserved request = %d, want 3", len(calls))
	}
	last := calls[2]
	if last.requests != 0 || last.prompt != 11 || last.completion != 7 {
		t.Fatalf("reserved completion = %+v, want requests=0 with tokens 11/7", last)
	}
	if reserver.used("key-1", "2026-10-21") != 1 {
		t.Fatalf("reserved slots = %d, want 1", reserver.used("key-1", "2026-10-21"))
	}
}

// countingReserver is an in-memory KeyUsageReserver.
type countingReserver struct {
	mu       sync.Mutex
	requests map[string]int
	reserves int
	released int
	err      error
}

func (r *countingReserver) slot(keyID, day string) string { return day + "\x00" + keyID }

func (r *countingReserver) ReserveKeyUsage(keyID, day string, quota int) (bool, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reserves++
	if r.err != nil {
		return false, 0, r.err
	}
	if r.requests == nil {
		r.requests = make(map[string]int)
	}
	slot := r.slot(keyID, day)
	used := r.requests[slot]
	if quota > 0 && used >= quota {
		return false, used, nil
	}
	r.requests[slot] = used + 1
	return true, used + 1, nil
}

func (r *countingReserver) ReleaseKeyUsageReservation(keyID, day string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.released++
	if slot := r.slot(keyID, day); r.requests[slot] > 0 {
		r.requests[slot]--
	}
	return nil
}

func (r *countingReserver) used(keyID, day string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests[r.slot(keyID, day)]
}

func useReserver(t *testing.T, reserver KeyUsageReserver) {
	t.Helper()
	InstallKeyUsageReserver(reserver)
	t.Cleanup(func() { InstallKeyUsageReserver(nil) })
}

// TestKeyQuotaReservationIsAtomic ports A auth.rs:74-91 / api_keys.rs:327-390:
// the slot is taken before routing, so 50 concurrent requests against quota 10
// admit exactly 10. The real store is used because its conditional upsert is
// the lock under test.
func TestKeyQuotaReservationIsAtomic(t *testing.T) {
	st, err := store.Open(testTempDir(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	useReserver(t, st)

	reg, _, _ := chatPluginServers(t, scopedChatReply)
	var usageMu sync.Mutex
	usageCalls := 0
	g := newTestGateway(t, Options{
		Plugins:   reg,
		KeyRecord: staticKeyRecord(KeyRecord{ID: "key-1", Enabled: true, DailyQuota: 10}),
		KeyUsage: func(keyID, day string, requests, prompt, completion, cached int, credits *float64) {
			usageMu.Lock()
			usageCalls++
			usageMu.Unlock()
			if err := st.IncrementKeyUsage(keyID, day, requests, prompt, completion, cached, credits); err != nil {
				t.Errorf("IncrementKeyUsage: %v", err)
			}
		},
	})
	fixed := time.Date(2026, 10, 21, 10, 0, 0, 0, time.Local)
	g.now = func() time.Time { return fixed }

	const racing = 50
	var (
		wg                 sync.WaitGroup
		mu                 sync.Mutex
		admitted, rejected int
	)
	for i := 0; i < racing; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
				`{"model":"p1/alpha","messages":[]}`))
			mu.Lock()
			defer mu.Unlock()
			switch rr.Code {
			case http.StatusOK:
				admitted++
			case http.StatusTooManyRequests:
				rejected++
			default:
				t.Errorf("unexpected status %d (body %s)", rr.Code, rr.Body.String())
			}
		}()
	}
	wg.Wait()
	if admitted != 10 || rejected != racing-10 {
		t.Fatalf("admitted/rejected = %d/%d, want 10/%d", admitted, rejected, racing-10)
	}
	used, err := st.KeyUsageRequests("key-1", "2026-10-21")
	if err != nil || used != 10 {
		t.Fatalf("stored requests = %d, err=%v, want exactly the quota", used, err)
	}
	// Completion must not count the request a second time.
	if usageCalls != 10 {
		t.Fatalf("completion calls = %d, want 10 (only admitted requests)", usageCalls)
	}
}

func TestKeyQuotaReservationReleaseOnScopeRejection(t *testing.T) {
	reg, _, _ := chatPluginServers(t, scopedChatReply)
	reserver := &countingReserver{}
	useReserver(t, reserver)
	var calls []keyUsageCall
	g := newTestGateway(t, Options{
		Plugins:   reg,
		KeyRecord: staticKeyRecord(KeyRecord{ID: "key-1", Enabled: true, DailyQuota: 5, ModelScope: []string{"p1/*"}}),
		KeyUsage: func(keyID, day string, requests, prompt, completion, cached int, credits *float64) {
			calls = append(calls, keyUsageCall{keyID, day, requests, prompt, completion, cached, credits})
		},
	})
	g.now = func() time.Time { return time.Date(2026, 10, 21, 10, 0, 0, 0, time.Local) }

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p2/gamma","messages":[]}`))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rr.Code, rr.Body.String())
	}
	if reserver.reserves != 1 {
		t.Fatalf("reserve calls = %d, want 1 at admission", reserver.reserves)
	}
	if reserver.released != 1 || reserver.used("key-1", "2026-10-21") != 0 {
		t.Fatalf("released = %d, used = %d; a scope rejection must not consume quota",
			reserver.released, reserver.used("key-1", "2026-10-21"))
	}
	if len(calls) != 0 {
		t.Fatalf("completion recorded a scope rejection: %+v", calls)
	}
}

func TestKeyQuotaReservationErrorFailsOpen(t *testing.T) {
	reg, calls1, _ := chatPluginServers(t, scopedChatReply)
	reserver := &countingReserver{err: errors.New("db locked")}
	useReserver(t, reserver)
	var calls []keyUsageCall
	g := newTestGateway(t, Options{
		Plugins:   reg,
		KeyRecord: staticKeyRecord(KeyRecord{Enabled: true, DailyQuota: 1}),
		KeyUsage: func(keyID, day string, requests, prompt, completion, cached int, credits *float64) {
			calls = append(calls, keyUsageCall{keyID, day, requests, prompt, completion, cached, credits})
		},
	})

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/alpha","messages":[]}`))
	if rr.Code != http.StatusOK || calls1.count() != 1 {
		t.Fatalf("fail-open status = %d, plugin calls = %d", rr.Code, calls1.count())
	}
	// Nothing was reserved, so the legacy counter still counts the request.
	if len(calls) != 1 || calls[0].requests != 1 {
		t.Fatalf("completion = %+v, want requests=1", calls)
	}
}

func TestKeyUsageCountsStreamingUsage(t *testing.T) {
	const sse = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"prompt_tokens_details\":{\"cached_tokens\":3}}}\n\n" +
		"data: [DONE]\n\n"
	srv, _ := newUpstreamServer(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	})
	reg := newFakeRegistry().add(pluginStatus("p1", true, model.ModelInfo{ID: "alpha"}), srv.URL, "tok")

	var calls []keyUsageCall
	g := newTestGateway(t, Options{
		Plugins:   reg,
		KeyRecord: staticKeyRecord(KeyRecord{Enabled: true}),
		KeyUsage: func(keyID, day string, requests, prompt, completion, cached int, credits *float64) {
			calls = append(calls, keyUsageCall{keyID, day, requests, prompt, completion, cached, credits})
		},
	})

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/alpha","stream":true,"messages":[]}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("stream = %d (body %s)", rr.Code, rr.Body.String())
	}
	if len(calls) != 1 {
		t.Fatalf("usage calls = %d, want 1", len(calls))
	}
	if calls[0].prompt != 5 || calls[0].completion != 2 || calls[0].cached != 3 {
		t.Fatalf("streaming usage = %+v, want 5/2 cached 3", calls[0])
	}
}

func TestKeyQuotaRejectionIsNotCounted(t *testing.T) {
	reg, _, _ := chatPluginServers(t, scopedChatReply)
	var calls []keyUsageCall
	g := newTestGateway(t, Options{
		Plugins:       reg,
		KeyRecord:     staticKeyRecord(KeyRecord{Enabled: true, DailyQuota: 1}),
		KeyUsageToday: func(string, string) (int, error) { return 1, nil },
		KeyUsage: func(keyID, day string, requests, prompt, completion, cached int, credits *float64) {
			calls = append(calls, keyUsageCall{keyID, day, requests, prompt, completion, cached, credits})
		},
	})
	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/alpha","messages":[]}`))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d", rr.Code)
	}
	if len(calls) != 0 {
		t.Fatalf("quota rejection was counted: %+v", calls)
	}
}

func TestKeyScopeOnMessages(t *testing.T) {
	reg, calls1, calls2 := chatPluginServers(t, scopedChatReply)

	t.Run("outside scope", func(t *testing.T) {
		g := newTestGateway(t, Options{
			Plugins:   reg,
			KeyRecord: staticKeyRecord(KeyRecord{Enabled: true, ModelScope: []string{"p1/*"}}),
		})
		rr := serve(g, newChatRequest(t, "/v1/messages", clientKey,
			`{"model":"p2/gamma","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body %s)", rr.Code, rr.Body.String())
		}
		if code := decodeError(t, rr).Error.Code; code != "model_not_found" {
			t.Fatalf("code = %q", code)
		}
		if calls1.count() != 0 || calls2.count() != 0 {
			t.Fatalf("upstreams called: %d/%d", calls1.count(), calls2.count())
		}
	})

	t.Run("bound bare model", func(t *testing.T) {
		g := newTestGateway(t, Options{
			Plugins:   reg,
			KeyRecord: staticKeyRecord(KeyRecord{Enabled: true, BindPool: "p1"}),
		})
		rr := serve(g, newChatRequest(t, "/v1/messages", clientKey,
			`{"model":"alpha","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d (body %s)", rr.Code, rr.Body.String())
		}
		up := calls1.last(t)
		if up.Path != "/v1/chat/completions" {
			t.Fatalf("upstream path = %q", up.Path)
		}
		var sent struct {
			Model string `json:"model"`
		}
		decodeInto(t, up.Body, &sent)
		if sent.Model != "alpha" {
			t.Fatalf("upstream model = %q", sent.Model)
		}
	})
}

func TestDisabledKeyRejected(t *testing.T) {
	reg, calls1, _ := chatPluginServers(t, scopedChatReply)
	g := newTestGateway(t, Options{
		Plugins:   reg,
		KeyRecord: staticKeyRecord(KeyRecord{Enabled: false}),
	})
	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey,
		`{"model":"p1/alpha","messages":[]}`))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("disabled key = %d, want 401 (body %s)", rr.Code, rr.Body.String())
	}
	if calls1.count() != 0 {
		t.Fatalf("disabled key reached the plugin")
	}
}

// TestPassThroughHonorsKeyScopeAndQuota covers the audited bypass: embeddings
// and images used to reach A without the key's model scope and without daily
// quota admission. Both are billable inference now.
func TestPassThroughHonorsKeyScopeAndQuota(t *testing.T) {
	paths := []string{"/v1/embeddings", "/v1/images/generations"}

	t.Run("scope mismatch is 404", func(t *testing.T) {
		for _, path := range paths {
			aSrv, aCalls := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"ok":true}`))
			g := newTestGateway(t, Options{
				A:         &fakeA{base: aSrv.URL, key: "sk-a"},
				KeyRecord: staticKeyRecord(KeyRecord{Enabled: true, ModelScope: []string{"p1/*"}}),
			})
			rr := serve(g, newChatRequest(t, path, clientKey, `{"model":"gpt-4o","input":"x"}`))
			if rr.Code != http.StatusNotFound {
				t.Fatalf("%s status = %d, want 404 (body %s)", path, rr.Code, rr.Body.String())
			}
			if code := decodeError(t, rr).Error.Code; code != "model_not_found" {
				t.Fatalf("%s error code = %q", path, code)
			}
			if aCalls.count() != 0 {
				t.Fatalf("%s reached A despite the key scope", path)
			}
		}
	})

	t.Run("quota exhausted is 429", func(t *testing.T) {
		for _, path := range paths {
			aSrv, aCalls := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"ok":true}`))
			g := newTestGateway(t, Options{
				A:             &fakeA{base: aSrv.URL, key: "sk-a"},
				KeyRecord:     staticKeyRecord(KeyRecord{Enabled: true, DailyQuota: 1}),
				KeyUsageToday: func(string, string) (int, error) { return 1, nil },
			})
			rr := serve(g, newChatRequest(t, path, clientKey, `{"model":"gpt-4o","input":"x"}`))
			if rr.Code != http.StatusTooManyRequests {
				t.Fatalf("%s status = %d, want 429 (body %s)", path, rr.Code, rr.Body.String())
			}
			if code := decodeError(t, rr).Error.Code; code != "key_quota_exceeded" {
				t.Fatalf("%s error code = %q", path, code)
			}
			if aCalls.count() != 0 {
				t.Fatalf("%s reached A despite the exhausted quota", path)
			}
		}
	})

	t.Run("admitted request is counted once", func(t *testing.T) {
		aSrv, aCalls := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"ok":true}`))
		var usageCalls int
		g := newTestGateway(t, Options{
			A:         &fakeA{base: aSrv.URL, key: "sk-a"},
			KeyRecord: staticKeyRecord(KeyRecord{ID: "key-1", Enabled: true, ModelScope: []string{"gpt-4o"}}),
			KeyUsage: func(keyID, day string, requests, prompt, completion, cached int, credits *float64) {
				usageCalls++
				if keyID != "key-1" || requests != 1 {
					t.Errorf("usage call = key %q requests %d, want key-1/1", keyID, requests)
				}
			},
		})
		rr := serve(g, newChatRequest(t, "/v1/embeddings", clientKey, `{"model":"gpt-4o","input":"x"}`))
		if rr.Code != http.StatusOK || aCalls.count() != 1 {
			t.Fatalf("status = %d, A calls = %d (body %s)", rr.Code, aCalls.count(), rr.Body.String())
		}
		if usageCalls != 1 {
			t.Fatalf("usage calls = %d, want exactly 1", usageCalls)
		}
	})

	t.Run("scope rejection releases the reservation", func(t *testing.T) {
		aSrv, aCalls := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"ok":true}`))
		reserver := &countingReserver{}
		useReserver(t, reserver)
		usageCalls := 0
		g := newTestGateway(t, Options{
			A:         &fakeA{base: aSrv.URL, key: "sk-a"},
			KeyRecord: staticKeyRecord(KeyRecord{ID: "key-1", Enabled: true, DailyQuota: 5, ModelScope: []string{"p1/*"}}),
			KeyUsage:  func(string, string, int, int, int, int, *float64) { usageCalls++ },
		})
		g.now = func() time.Time { return time.Date(2026, 10, 21, 10, 0, 0, 0, time.Local) }

		rr := serve(g, newChatRequest(t, "/v1/images/generations", clientKey, `{"model":"gpt-4o"}`))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rr.Code)
		}
		if aCalls.count() != 0 {
			t.Fatalf("A called %d times", aCalls.count())
		}
		if reserver.reserves != 1 || reserver.released != 1 || reserver.used("key-1", "2026-10-21") != 0 {
			t.Fatalf("reserve/release/used = %d/%d/%d, want 1/1/0",
				reserver.reserves, reserver.released, reserver.used("key-1", "2026-10-21"))
		}
		if usageCalls != 0 {
			t.Fatalf("scope rejection was counted %d times", usageCalls)
		}
	})
}

func TestKeyBindPoolModelsListingHidesOtherPools(t *testing.T) {
	reg, _, _ := chatPluginServers(t, scopedChatReply)
	g := newTestGateway(t, Options{
		Plugins:   reg,
		KeyRecord: staticKeyRecord(KeyRecord{Enabled: true, BindPool: "p1"}),
	})
	rr := serve(g, newGetRequest(t, "/v1/models", clientKey))
	if rr.Code != http.StatusOK {
		t.Fatalf("models = %d, body %s", rr.Code, rr.Body.String())
	}
	ids := modelIDs(decodeModels(t, rr))
	want := map[string]bool{"p1/alpha": true, "p1/beta": true}
	if len(ids) != len(want) {
		t.Fatalf("bound key models = %v, want only p1's", ids)
	}
	for _, id := range ids {
		if !want[id] {
			t.Fatalf("bind_pool=p1 key saw %q; catalog must be limited to its pool (%v)", id, ids)
		}
	}
}
