package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func egressJSON(strict bool, accounts map[string]map[string]string) string {
	if accounts == nil {
		accounts = map[string]map[string]string{}
	}
	b, _ := json.Marshal(map[string]any{
		"version":  1,
		"strict":   strict,
		"default":  map[string]string{"mode": "direct"},
		"accounts": accounts,
	})
	return string(b)
}

func applyReload(t *testing.T, h http.Handler) reloadResp {
	t.Helper()
	rec := doReq(t, h, http.MethodPost, "/reload", reqOpts{})
	if rec.Code != http.StatusOK {
		t.Fatalf("reload status = %d, body %s", rec.Code, rec.Body.String())
	}
	return decodeAs[reloadResp](t, rec)
}

func TestEgressStrictDirectRejectsAccountAndLogin(t *testing.T) {
	dir := t.TempDir()
	egressPath := filepath.Join(dir, "egress.json")
	p, h := newTestPlugin(t, func(o *Options) { o.EgressFile = egressPath })
	acct := seedAccount(t, p, "alice")

	writeTestFile(t, egressPath, egressJSON(true, nil))
	res := applyReload(t, h)
	if !res.OK || !res.Strict {
		t.Fatalf("reload = %+v", res)
	}

	rec := doReq(t, h, http.MethodPost, "/accounts/"+acct.ID+"/checkin", reqOpts{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("checkin status = %d, want 403, body %s", rec.Code, rec.Body.String())
	}
	if env := decodeAs[errorEnvelope](t, rec); env.Error.Type != "egress_missing" {
		t.Fatalf("error = %+v", env.Error)
	}

	rec = doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{body: chatRequestBody(t, "GLM-5.2", false, nil)})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("chat status = %d, want 403, body %s", rec.Code, rec.Body.String())
	}

	rec = doReq(t, h, http.MethodPost, "/accounts/login", reqOpts{body: `{"method":"paste_code"}`})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("login status = %d, want 403, body %s", rec.Code, rec.Body.String())
	}
}

func TestEgressBlockedAccountRejectsChat(t *testing.T) {
	dir := t.TempDir()
	egressPath := filepath.Join(dir, "egress.json")
	p, h := newTestPlugin(t, func(o *Options) { o.EgressFile = egressPath })
	acct := seedAccount(t, p, "alice")

	writeTestFile(t, egressPath, egressJSON(false, map[string]map[string]string{
		acct.ID: {"mode": "blocked"},
	}))
	res := applyReload(t, h)
	if !res.OK || res.EgressAccounts != 1 {
		t.Fatalf("reload = %+v", res)
	}
	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", reqOpts{
		body:    chatRequestBody(t, "GLM-5.2", false, nil),
		headers: map[string]string{"X-CodeArts-Account": acct.ID},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("chat status = %d, want 403, body %s", rec.Code, rec.Body.String())
	}
	if env := decodeAs[errorEnvelope](t, rec); env.Error.Code != "egress_missing" {
		t.Fatalf("error = %+v", env.Error)
	}
}

func TestEgressProxyIsUsedAndDirectNeverFallsBack(t *testing.T) {
	var mu sync.Mutex
	proxyHits := 0
	proxyHost := ""
	proxyAuth := ""
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		proxyHits++
		proxyHost = r.Host
		proxyAuth = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, sseLine(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "via proxy"}}}}))
		_, _ = fmt.Fprint(w, sseLine(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}}))
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer proxy.Close()

	dir := t.TempDir()
	egressPath := filepath.Join(dir, "egress.json")
	ep := testEndpoints()
	ep.Chat = "http://chat.invalid/api/v2/chat/completions"
	p, h := newTestPlugin(t, func(o *Options) {
		o.Endpoints = ep
		o.EgressFile = egressPath
	})
	acct := seedAccount(t, p, "alice")
	chatOpts := reqOpts{
		body:    chatRequestBody(t, "GLM-5.2", false, nil),
		headers: map[string]string{"X-CodeArts-Account": acct.ID},
	}

	writeTestFile(t, egressPath, egressJSON(false, map[string]map[string]string{
		acct.ID: {"mode": "proxy", "url": proxy.URL, "proxy_id": "px_1"},
	}))
	applyReload(t, h)
	rec := doReq(t, h, http.MethodPost, "/v1/chat/completions", chatOpts)
	if rec.Code != http.StatusOK {
		t.Fatalf("proxied chat status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "via proxy" {
		t.Fatalf("proxied response = %+v", resp)
	}
	mu.Lock()
	gotHits, gotHost, gotAuth := proxyHits, proxyHost, proxyAuth
	mu.Unlock()
	if gotHits != 1 || gotHost != "chat.invalid" {
		t.Fatalf("proxy hits/host = %d/%q", gotHits, gotHost)
	}
	if !strings.HasPrefix(gotAuth, "SDK-HMAC-SHA256 Access=AK-alice,") {
		t.Fatalf("proxy saw authorization = %q", gotAuth)
	}

	// Switch to direct: the unreachable host must fail and the proxy must not
	// be used as a fallback.
	writeTestFile(t, egressPath, egressJSON(false, nil))
	applyReload(t, h)
	rec = doReq(t, h, http.MethodPost, "/v1/chat/completions", chatOpts)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("direct chat status = %d, want 502, body %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	finalHits := proxyHits
	mu.Unlock()
	if finalHits != 1 {
		t.Fatalf("proxy hits after direct request = %d, want 1", finalHits)
	}
}
