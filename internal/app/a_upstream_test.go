package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"aiworkgate/internal/hosts"
	"aiworkgate/internal/model"
)

// fakeAUpstream starts a server answering GET /health with the given status.
func fakeAUpstream(t *testing.T, statusCode int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if statusCode != http.StatusOK {
			w.WriteHeader(statusCode)
			return
		}
		io.WriteString(w, `{"status":"ok"}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func serverPort(t *testing.T, raw string) int {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil || u.Port() == "" {
		t.Fatalf("cannot parse %q: %v", raw, err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("bad port in %q: %v", raw, err)
	}
	return p
}

// TestAAdapterFollowsChangedPort mimics A restarting on a new loopback port:
// the fake discovery dir reports the new port once the old URL stops
// answering, and BaseURL() must follow without a host restart.
func TestAAdapterFollowsChangedPort(t *testing.T) {
	dead := fakeAUpstream(t, http.StatusOK)
	alive := fakeAUpstream(t, http.StatusOK)
	a := &App{aBaseURL: dead.URL, aURL: dead.URL, aPortLive: serverPort(t, dead.URL)}
	a.aProbeTTL = time.Millisecond
	a.aDiscoverPort = func() (int, bool) { return serverPort(t, alive.URL), true }

	dead.Close() // A restarted: the captured port is now dead.

	ad := &aAdapter{app: a, baseURL: a.aBaseURL}
	if got := ad.BaseURL(); got != alive.URL {
		t.Fatalf("BaseURL() = %q, want the rediscovered %q", got, alive.URL)
	}
	if got := a.aLivePort(); got != serverPort(t, alive.URL) {
		t.Fatalf("aLivePort() = %d, want %d", got, serverPort(t, alive.URL))
	}
	if st, _, _ := a.aEnsure(false); st != hosts.AStateOK {
		t.Fatalf("state after follow = %q, want ok", st)
	}
}

// TestAEnsureThreeStates checks the down/unauthorized/ok mapping end to end
// through the app resolver.
func TestAEnsureThreeStates(t *testing.T) {
	okSrv := fakeAUpstream(t, http.StatusOK)
	a := &App{aBaseURL: okSrv.URL, aURL: okSrv.URL}
	a.aProbeTTL = time.Hour
	if st, detail, url := a.aEnsure(true); st != hosts.AStateOK || detail != "" || url != okSrv.URL {
		t.Fatalf("ok case = %q/%q/%q", st, detail, url)
	}

	auth := fakeAUpstream(t, http.StatusUnauthorized)
	a = &App{aBaseURL: auth.URL, aURL: auth.URL}
	a.aDiscoverPort = func() (int, bool) { return 0, false }
	st, detail, url := a.aEnsure(true)
	if st != hosts.AStateUnauthorized || url != auth.URL || detail == "" {
		t.Fatalf("401 case = %q/%q/%q", st, detail, url)
	}
	a = &App{aBaseURL: auth.URL, aURL: auth.URL}
	a.aDiscoverPort = func() (int, bool) { return 0, false }
	if st, _, _ := a.aEnsure(true); st != hosts.AStateUnauthorized {
		t.Fatalf("403/401 must stay unauthorized (port discovery irrelevant), got %q", st)
	}

	refused := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	refusedURL := refused.URL
	refused.Close()
	a = &App{aBaseURL: refusedURL, aURL: refusedURL}
	a.aDiscoverPort = func() (int, bool) { return 0, false }
	if st, detail, _ := a.aEnsure(true); st != hosts.AStateDown || detail == "" {
		t.Fatalf("refused case = %q/%q", st, detail)
	}
}

// invalidatingGateway stands in for internal/gateway: the app talks to it
// only through the narrow InvalidateAModelCache interface assertion.
type invalidatingGateway struct {
	mu   sync.Mutex
	hits int
}

func (g *invalidatingGateway) ServeHTTP(http.ResponseWriter, *http.Request) {}

func (g *invalidatingGateway) InvalidateAModelCache() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.hits++
}

func (g *invalidatingGateway) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hits
}

// TestRecoveryInvalidatesAModelCache: a down -> ok transition forces the A
// model cache invalidation hook; no other transition fires it.
func TestRecoveryInvalidatesAModelCache(t *testing.T) {
	alive := fakeAUpstream(t, http.StatusOK)
	refused := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	refusedURL := refused.URL
	refused.Close()

	gw := &invalidatingGateway{}
	a := &App{aBaseURL: refusedURL, aURL: refusedURL, gatewayHandler: gw}
	a.aProbeTTL = time.Millisecond
	follow := false
	a.aDiscoverPort = func() (int, bool) {
		if !follow {
			return 0, false
		}
		return serverPort(t, alive.URL), true
	}

	if st, _, _ := a.aEnsure(true); st != hosts.AStateDown {
		t.Fatalf("first probe = %q, want down", st)
	}
	follow = true // A restarted on a new port that now answers.
	if st, _, _ := a.aEnsure(true); st != hosts.AStateOK {
		t.Fatalf("second probe = %q, want ok after follow", st)
	}
	if got := gw.count(); got != 1 {
		t.Fatalf("invalidations = %d, want 1 (down -> ok)", got)
	}
	a.aEnsure(true) // ok -> ok must not fire again
	if got := gw.count(); got != 1 {
		t.Fatalf("invalidations after ok -> ok = %d, want still 1", got)
	}
}

func TestHostStatusAFieldsJSON(t *testing.T) {
	raw, err := json.Marshal(model.HostStatus{
		ARunning: true, APort: 7864, AState: "unauthorized", ADetail: "HTTP 401",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"a_running":true`, `"a_state":"unauthorized"`, `"a_detail":"HTTP 401"`, `"a_port":7864`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("payload %s lacks %s", raw, want)
		}
	}
	// The new fields stay optional for older front ends.
	raw, err = json.Marshal(model.HostStatus{ARunning: true})
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	if strings.Contains(string(raw), "a_state") || strings.Contains(string(raw), "a_detail") {
		t.Fatalf("payload %s must omit empty a_state/a_detail", raw)
	}
}

// TestConcurrentBaseURLRace drives repeated adapter BaseURL()/status probes
// from many goroutines; run with -race.
func TestConcurrentBaseURLRace(t *testing.T) {
	srv := fakeAUpstream(t, http.StatusOK)
	a := &App{aBaseURL: srv.URL, aURL: srv.URL, aPortLive: serverPort(t, srv.URL)}
	a.aProbeTTL = time.Millisecond
	a.aDiscoverPort = func() (int, bool) { return serverPort(t, srv.URL), true }
	ad := &aAdapter{app: a, baseURL: a.aBaseURL}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				if got := ad.BaseURL(); got != srv.URL {
					t.Errorf("BaseURL() = %q, want %q", got, srv.URL)
					return
				}
				state, _, port, running := a.aStatusProbe()
				if state != string(hosts.AStateOK) || !running || port != serverPort(t, srv.URL) {
					t.Errorf("aStatusProbe = %q/%v port %d", state, running, port)
					return
				}
			}
		}()
	}
	wg.Wait()
}
