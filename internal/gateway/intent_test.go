package gateway

import (
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"aiworkgate/internal/model"
	"aiworkgate/internal/scheduler"
)

// intentSnapshot builds two pools and three bindings: "grp" (pa) owns the
// intent rule, "strong" (pb) and "fallback" (pb) are its possible targets.
func intentSnapshot(intent *scheduler.IntentBinding, defaultGroup string) *scheduler.Snapshot {
	if intent != nil && intent.Default == "" {
		intent.Default = defaultGroup
	}
	return &scheduler.Snapshot{
		InterPool: scheduler.InterPoolSmart,
		Bindings: map[string]scheduler.Binding{
			"grp": {Logical: "grp", Items: []scheduler.Source{{PluginID: "pa", Model: "m1"}}, Intent: intent},
			"strong": {Logical: "strong", Items: []scheduler.Source{
				{PluginID: "pb", Model: "m1"},
			}},
			"fallback": {Logical: "fallback", Items: []scheduler.Source{
				{PluginID: "pb", Model: "m1"},
			}},
		},
		Pools: map[string]*scheduler.Pool{
			"pa": {PluginID: "pa", APIVersion: 2, UpstreamEnabled: true,
				Strategy: scheduler.StrategyFixed, Accounts: fixedPoolAccounts("acc_a"),
				Models: map[string]model.ModelInfo{"m1": {ID: "m1"}}},
			"pb": {PluginID: "pb", APIVersion: 2, UpstreamEnabled: true,
				Strategy: scheduler.StrategyFixed, Accounts: fixedPoolAccounts("acc_b"),
				Models: map[string]model.ModelInfo{"m1": {ID: "m1"}}},
		},
	}
}

func intentGateway(t *testing.T, jevURL string, snap *scheduler.Snapshot) (*Gateway, *int64, *int64) {
	t.Helper()
	var paCalls, pbCalls int64
	paSrv, _ := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		atomic.AddInt64(&paCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"served":"pa"}`)
	})
	pbSrv, _ := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		atomic.AddInt64(&pbCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"served":"pb"}`)
	})
	reg := newFakeRegistry().
		add(pluginStatus("pa", true, model.ModelInfo{ID: "m1"}), paSrv.URL, "tok-a").
		add(pluginStatus("pb", true, model.ModelInfo{ID: "m1"}), pbSrv.URL, "tok-b")
	g := newTestGateway(t, Options{
		Plugins:  reg,
		Schedule: scheduler.New(scheduler.Options{}),
		Snapshot: staticSnapshot(snap),
		DecisionUpstreams: func() []DecisionUpstream {
			return []DecisionUpstream{{ID: "jev", BaseURL: jevURL + "/v1", Protocol: "systemone", ModelID: "jev"}}
		},
	})
	return g, &paCalls, &pbCalls
}

func intentBody(question string) string {
	return `{"model":"grp","messages":[{"role":"user","content":"` + question + `"}]}`
}

func TestIntentRoutesToStrongGroupAndCaches(t *testing.T) {
	var jevCalls int64
	jevSrv, _ := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		atomic.AddInt64(&jevCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"answers":[{"type":"choice","value":2}]}`)
	})
	snap := intentSnapshot(&scheduler.IntentBinding{
		Labels: []string{"simple", "hard"},
		Route:  map[string]string{"simple": "grp", "hard": "strong"},
	}, "grp")
	g, paCalls, pbCalls := intentGateway(t, jevSrv.URL, snap)

	for i := 0; i < 2; i++ {
		rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, intentBody("prove this theorem")))
		if rr.Code != http.StatusOK {
			t.Fatalf("attempt %d status = %d (%s)", i, rr.Code, rr.Body.String())
		}
		if body := rr.Body.String(); body != `{"served":"pb"}` {
			t.Fatalf("attempt %d body = %s, want pb", i, body)
		}
	}
	if got := atomic.LoadInt64(pbCalls); got != 2 {
		t.Fatalf("pb calls = %d, want 2", got)
	}
	if got := atomic.LoadInt64(paCalls); got != 0 {
		t.Fatalf("pa calls = %d, want 0", got)
	}
	if got := atomic.LoadInt64(&jevCalls); got != 1 {
		t.Fatalf("jev calls = %d, want 1 (the probe fingerprint is cached)", got)
	}
}

func TestIntentTimeoutFallsBackToDefaultGroup(t *testing.T) {
	jevSrv, _ := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, `{"answers":[{"choice":2}]}`)
	})
	snap := intentSnapshot(&scheduler.IntentBinding{
		Labels: []string{"simple", "hard"},
		Route:  map[string]string{"simple": "grp", "hard": "strong"},
	}, "grp")
	g, paCalls, pbCalls := intentGateway(t, jevSrv.URL, snap)
	g.intentTimeout = 50 * time.Millisecond

	start := time.Now()
	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, intentBody("slow classifier")))
	if rr.Code != http.StatusOK || rr.Body.String() != `{"served":"pa"}` {
		t.Fatalf("status = %d body = %s, want the original group", rr.Code, rr.Body.String())
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("request took %v, the 50ms intent budget did not bound it", elapsed)
	}
	if atomic.LoadInt64(paCalls) != 1 || atomic.LoadInt64(pbCalls) != 0 {
		t.Fatalf("pa=%d pb=%d, want pa only", *paCalls, *pbCalls)
	}
}

func TestIntentFailureUsesConfiguredDefaultGroup(t *testing.T) {
	jevSrv, _ := newUpstreamServer(t, jsonResponder(http.StatusBadGateway, `{"error":"down"}`))
	snap := intentSnapshot(&scheduler.IntentBinding{
		Labels: []string{"simple", "hard"},
		Route:  map[string]string{"simple": "grp", "hard": "strong"},
	}, "fallback")
	g, paCalls, pbCalls := intentGateway(t, jevSrv.URL, snap)

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, intentBody("anything")))
	if rr.Code != http.StatusOK || rr.Body.String() != `{"served":"pb"}` {
		t.Fatalf("status = %d body = %s, want the fallback group", rr.Code, rr.Body.String())
	}
	if atomic.LoadInt64(paCalls) != 0 || atomic.LoadInt64(pbCalls) != 1 {
		t.Fatalf("pa=%d pb=%d, want pb only", *paCalls, *pbCalls)
	}
}

func TestIntentUnparsableAnswerKeepsOriginalGroup(t *testing.T) {
	jevSrv, _ := newUpstreamServer(t, jsonResponder(http.StatusOK, `{"answers":[{"type":"choice","value":9}]}`))
	snap := intentSnapshot(&scheduler.IntentBinding{
		Labels: []string{"simple", "hard"},
		Route:  map[string]string{"simple": "grp", "hard": "strong"},
	}, "")
	g, paCalls, _ := intentGateway(t, jevSrv.URL, snap)

	rr := serve(g, newChatRequest(t, "/v1/chat/completions", clientKey, intentBody("unknown shape")))
	if rr.Body.String() != `{"served":"pa"}` {
		t.Fatalf("body = %s, want the original group", rr.Body.String())
	}
	if atomic.LoadInt64(paCalls) != 1 {
		t.Fatalf("pa calls = %d, want 1", *paCalls)
	}
}

func TestParseIntentLabelSpellings(t *testing.T) {
	labels := []string{"simple", "hard"}
	cases := []struct {
		body string
		want string
		ok   bool
	}{
		{`{"label":"hard"}`, "hard", true},
		{`{"answer":"Simple"}`, "simple", true},
		{`{"answers":[{"choice":2}]}`, "hard", true},
		{`{"answers":[{"value":1}]}`, "simple", true},
		{`{"answers":[{"type":"choice","value":2}]}`, "hard", true},
		{`{"answers":[{"label":"master"}]}`, "", false},
		{`{"answers":[]}`, "", false},
		{`not json`, "", false},
	}
	for _, tc := range cases {
		got, ok := parseIntentLabel([]byte(tc.body), labels)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("parseIntentLabel(%s) = (%q,%v), want (%q,%v)", tc.body, got, ok, tc.want, tc.ok)
		}
	}
}

func TestPromptProbeShapes(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"ok"},{"role":"user","content":"last"}]}`, "last"},
		{`{"contents":[{"role":"user","parts":[{"text":"gemini"}]}]}`, "gemini"},
		{`{"input":"responses"}`, "responses"},
		{`{"prompt":"legacy"}`, "legacy"},
		{`{"messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}]}`, "a\nb"},
		{`{"messages":[]}`, ""},
	}
	for _, tc := range cases {
		if got := promptProbe([]byte(tc.body), 100); got != tc.want {
			t.Fatalf("promptProbe(%s) = %q, want %q", tc.body, got, tc.want)
		}
	}
	if got := promptProbe([]byte(`{"messages":[{"role":"user","content":"abcdef"}]}`), 3); got != "abc" {
		t.Fatalf("truncated probe = %q, want abc", got)
	}
}

func TestIntentCacheExpires(t *testing.T) {
	g := newTestGateway(t, Options{})
	base := time.Unix(1_800_000_000, 0)
	g.now = func() time.Time { return base }
	g.intentCachePut("fp", "hard")
	if label, ok := g.intentCacheGet("fp"); !ok || label != "hard" {
		t.Fatalf("cached label = %q ok=%v", label, ok)
	}
	g.now = func() time.Time { return base.Add(defaultIntentTTL + time.Second) }
	if _, ok := g.intentCacheGet("fp"); ok {
		t.Fatal("cache entry survived the 10-minute TTL")
	}
}

func TestIntentDefaults(t *testing.T) {
	if defaultIntentTimeout != 3*time.Second {
		t.Fatalf("intent timeout = %v, want 3s", defaultIntentTimeout)
	}
	if defaultIntentTTL != 10*time.Minute {
		t.Fatalf("intent TTL = %v, want 10m", defaultIntentTTL)
	}
}
