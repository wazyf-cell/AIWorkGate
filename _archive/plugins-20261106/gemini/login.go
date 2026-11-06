package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The OAuth login is Google's installed-app flow (loopback redirect), which is
// what the reference implements in vendor/channel-pack/src/gemini-oauth.ts:
//
//  1. pick a free port and listen on 127.0.0.1 for the redirect;
//  2. hand the operator <auth endpoint>?client_id=...&prompt=consent;
//  3. the browser returns to /oauth-callback?code=&state=;
//  4. exchange the code at oauth2.googleapis.com/token and store the grant.
//
// The reference also supports a paste-code path (the operator copies the code out
// of the browser when no loopback listener is reachable). Both are implemented so
// a host without a browser-open capability can still finish the login.

// callbackPath is the loopback redirect path the reference registers.
const callbackPath = "/oauth-callback"

// loginFlow is one in-flight authorization.
type loginFlow struct {
	State       string
	RedirectURI string
	// Verifier is the PKCE code_verifier for this flow; it is sent only on the
	// token request and never leaves the process otherwise.
	Verifier string
	// Status is pending/success/failed.
	Status string
	// URL is the consent URL handed to the operator.
	URL    string
	ErrMsg string
	// Server is the loopback listener, closed when the flow ends.
	Server *http.Server
}

// server is the plugin's dependencies.
type server struct {
	cfg    pluginConfig
	store  *store
	client *upstreamClient
	// sigs is the thoughtSignature cache; nil-safe methods keep a server built
	// without a data dir working.
	sigs *sigStore

	loginMu sync.Mutex
	logins  map[string]*loginFlow

	// projectCache memoises the loadCodeAssist probe per identity, as the
	// reference's projectCache does (60 s TTL is not applied there either; the
	// cached value lives for the process).
	projectMu    sync.Mutex
	projectCache map[string]string

	quotaMu    sync.Mutex
	quotaCache map[string]quotaEntry
}

type quotaEntry struct {
	at     time.Time
	result map[string]any
}

// server.balances is what /accounts reports; see quotaSnapshot.
func newServer(cfg pluginConfig, st *store) *server {
	dir := ""
	if st != nil {
		dir = filepath.Dir(st.path)
	}
	s := &server{
		cfg:          cfg,
		store:        st,
		client:       newUpstreamClient(),
		sigs:         newSigStore(dir),
		logins:       map[string]*loginFlow{},
		projectCache: map[string]string{},
		quotaCache:   map[string]quotaEntry{},
	}
	if s.client != nil && s.client.egress != nil {
		if ef, err := loadEgressFile(cfg.EgressFile); err == nil {
			s.client.egress.set(ef)
		} else {
			fmt.Fprintln(os.Stderr, err.Error())
		}
	}
	return s
}

// generateState returns a random opaque OAuth state value.
func generateState() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "state"
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// pickFreePort asks the OS for an ephemeral loopback port, matching pickFreePort in
// the reference.
func pickFreePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// startLoopback begins listening for the redirect and returns the flow. The
// listener answers one callback and then settles the flow.
func (s *server) startLoopback() (*loginFlow, error) {
	port, err := pickFreePort()
	if err != nil {
		return nil, fmt.Errorf("gemini: cannot reserve a loopback port: %w", err)
	}
	state := generateState()
	redirect := fmt.Sprintf("http://localhost:%d%s", port, callbackPath)
	verifier, challenge := generatePKCE()
	flow := &loginFlow{
		State:       state,
		RedirectURI: redirect,
		Verifier:    verifier,
		Status:      "pending",
		URL:         authURL(redirect, state, challenge),
	}

	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("state"); got != state {
			// A mismatched state must not complete the flow.
			http.Error(w, "state mismatch", http.StatusBadRequest)
			return
		}
		if errMsg := r.URL.Query().Get("error"); errMsg != "" {
			s.settle(flow, "failed", "authorization refused: "+errMsg)
			fmt.Fprint(w, callbackHTML("授权失败，可以关闭此页。"))
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			s.settle(flow, "failed", "callback carried no code")
			fmt.Fprint(w, callbackHTML("回调里没有 code，请重新发起登录。"))
			return
		}
		go func() {
			if err := s.finishCode(context.Background(), flow, code); err != nil {
				s.settle(flow, "failed", err.Error())
				return
			}
			s.settle(flow, "success", "")
		}()
		fmt.Fprint(w, callbackHTML("授权已完成，可以关闭此页回到面板。"))
	})

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("gemini: cannot listen on the redirect port: %w", err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	flow.Server = srv
	go func() { _ = srv.Serve(ln) }()
	// The whole flow expires after the reference's 6 minutes.
	time.AfterFunc(authFlowTimeout, func() { s.settle(flow, "failed", "authorization timed out") })
	return flow, nil
}

// finishCode redeems an authorization code and stores the resulting credential.
// The account is registered only after the token endpoint returned a real
// access_token: a failed exchange leaves the store untouched.
func (s *server) finishCode(ctx context.Context, flow *loginFlow, code string) error {
	prev, _ := s.store.get()
	grant, err := s.client.exchangeCode(ctx, defaultAccountID, code, flow.RedirectURI, flow.Verifier)
	if err != nil {
		return err
	}
	next := grantToCredentials(grant, prev)
	next.ensureIdentity(time.Now())
	// Enrich the identity, which the reference treats as best-effort.
	if next.Sub == "" || next.Email == "" {
		if sub, email := s.client.userInfo(ctx, next.AccessToken, defaultAccountID); sub != "" || email != "" {
			next.Sub = firstNonEmpty(next.Sub, sub)
			next.Email = firstNonEmpty(next.Email, email)
		}
	}
	return s.store.set(next)
}

// settle moves a flow to a terminal status and stops its listener.
func (s *server) settle(flow *loginFlow, status, errMsg string) {
	s.loginMu.Lock()
	if flow.Status == "pending" {
		flow.Status = status
		flow.ErrMsg = errMsg
	}
	srv := flow.Server
	s.loginMu.Unlock()
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
}

// callbackHTML is the page the browser lands on. It deliberately carries no data.
func callbackHTML(message string) string {
	return `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><title>授权</title></head>` +
		`<body style="font:15px/1.7 sans-serif;text-align:center;padding-top:20vh">` +
		message + `</body></html>`
}

// errUnknownModel is returned for a model outside this channel's one-model
// catalog. The reference attaches 404 so the gateway does not retry.
var errUnknownModel = errors.New("gemini: model is not in this provider's catalog (only " + upstreamFlash + " is supported)")

// pollUntilDone waits for a flow to leave pending, or the deadline. The reference
// answers a poll immediately rather than blocking; the host polls repeatedly, so
// this returns the current status. The lock is required because settle writes the
// same fields from the callback goroutine and the timeout timer.
func (s *server) flowStatus(flow *loginFlow) (status, url, errMsg string) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	return flow.Status, flow.URL, flow.ErrMsg
}

// trimBearer is a small helper for the token check.
func trimBearer(h string) string {
	return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
}

// exchangeErrorStatus separates a rejected code from a broken path: only the
// former is the operator's mistake.
func exchangeErrorStatus(err error) (int, string) {
	var config *oauthConfigError
	if errors.As(err, &config) {
		return http.StatusNotImplemented, "client_secret_required"
	}
	var refused *grantRefusedError
	if errors.As(err, &refused) {
		return http.StatusBadRequest, "invalid_credential"
	}
	return http.StatusBadGateway, "upstream_error"
}
