package main

// The three chat channels plus the model catalog.
//
// Ported from workbuddy2api-panel/internal/autoclaw/upstream.go and chat.go:
//
//	channel 1  proxy      base(region) + /autoclaw-proxy/proxy/autoclaw/v1/chat/completions
//	channel 2  sandbox    <sandbox_endpoint> + /v1/chat/completions
//	channel 3  relay      <relay base>/api/events  (SSE) + /api/electron/agent/send
//
// Channel 1 refuses agent targets by design; channel 2 answers "not enabled" on
// some sandboxes, which is why the reference falls through to channel 3 in auto
// mode. This port keeps that ladder.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// relayJSON calls a relay endpoint and checks the ok/code envelope.
func (u *upstream) relayJSON(accountID, token, base, method, path string, body any) (json.RawMessage, error) {
	headers := map[string]string{"Accept": "application/json, text/plain, */*"}
	if token != "" {
		headers["Authorization"] = bearer(token)
	}
	full := strings.TrimRight(base, "/") + path
	req, err := http.NewRequest(method, full, marshalReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := u.do(req, accountID)
	if err != nil {
		return nil, upstreamErrf("server", 0, 0, "%v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, upstreamErrf("server", resp.StatusCode, 0, "read: %v", err)
	}
	text := string(raw)
	if resp.StatusCode >= 400 {
		return nil, upstreamErrf(classify(resp.StatusCode, 0, text), resp.StatusCode, 0, "%.200s", text)
	}
	var env struct {
		OK    *bool `json:"ok"`
		Code  any   `json:"code"`
		Error any   `json:"error"`
		Msg   any   `json:"msg"`
		Data  any   `json:"data"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return json.RawMessage(raw), nil
	}
	code := toInt64(env.Code)
	msg := toString(env.Error)
	if msg == "" {
		msg = toString(env.Msg)
	}
	if (env.OK != nil && !*env.OK) || (env.Code != nil && code != 0) {
		return nil, upstreamErrf(classify(resp.StatusCode, code, msg), resp.StatusCode, code, "%s", msg)
	}
	if env.Data != nil {
		out, err := json.Marshal(env.Data)
		if err != nil {
			return json.RawMessage(raw), nil
		}
		return out, nil
	}
	return json.RawMessage(raw), nil
}

// chatProxy is channel 1. The caller closes the body.
func (u *upstream) chatProxy(a *account, body []byte, model string) (*http.Response, error) {
	snap := a.snapshot()
	full := u.base(snap.Region) + proxyChatPath
	payload := prepareProxyBody(body, model)
	req, err := http.NewRequest(http.MethodPost, full, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	for k, v := range proxyChatHeaders(snap.AccessToken, model) {
		req.Header.Set(k, v)
	}
	resp, err := u.do(req, stateKey(snap.Region, snap.UserID))
	if err != nil {
		return nil, upstreamErrf("server", 0, 0, "%v", err)
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		text := string(raw)
		return nil, upstreamErrf(classify(resp.StatusCode, 0, text), resp.StatusCode, 0, "%.200s", text)
	}
	return resp, nil
}

// chatOpenAI is channel 2. A nil response with a non-zero status means the
// sandbox answered 4xx/5xx, which the caller may treat as "not enabled".
func (u *upstream) chatOpenAI(a *account, body []byte) (*http.Response, int, string) {
	snap := a.snapshot()
	if snap.SandboxEndpoint == "" {
		return nil, 0, fmt.Sprintf("no sandbox endpoint for account %s", snap.UserID)
	}
	_, xm := normalizeModel(peekModel(body))
	payload := prepareModelBody(body)
	headers := map[string]string{
		"Accept":       "application/json, text/plain, */*",
		"Content-Type": "application/json",
	}
	if snap.AccessToken != "" {
		headers["Authorization"] = bearer(snap.AccessToken)
	}
	if xm != "" {
		headers["x-openclaw-model"] = xm
	}
	target := strings.TrimRight(snap.SandboxEndpoint, "/") + "/v1/chat/completions"
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, err.Error()
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := u.do(req, stateKey(snap.Region, snap.UserID))
	if err != nil {
		return nil, 0, err.Error()
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, resp.StatusCode, string(raw)
	}
	return resp, resp.StatusCode, ""
}

// relayStart is channel 3's handshake: put the device online over the auth.inject
// websocket, open the event stream, then send the request. It does not own the
// returned response; the caller closes it and calls closeWS (which is always
// non-nil on success).
func (u *upstream) relayStart(a *account, body []byte) (runID, session string, events *http.Response, closeWS func(), err error) {
	closeWS = func() {}
	snap := a.snapshot()
	if snap.SandboxEndpoint == "" {
		return "", "", nil, nil, upstreamErrf("client", 0, 0, "no sandbox endpoint for account %s", snap.UserID)
	}
	reqObj, err := buildRelayChatReq(body)
	if err != nil {
		return "", "", nil, nil, upstreamErrf("client", 0, 0, "%v", err)
	}
	session = toString(reqObj["sessionKey"])
	acctID := stateKey(snap.Region, snap.UserID)
	if wsClose, werr := u.openDeviceWS(a); werr != nil {
		// The reference only warns here: without a device socket the send still
		// goes through the HTTP bridge on some upstream builds.
		log.Printf("autoglm: device online ws: %v (continuing without auth.inject)", werr)
	} else {
		closeWS = wsClose
	}
	events, err = u.openRelayEvents(snap.AccessToken, acctID, snap.SandboxEndpoint)
	if err != nil {
		if closeWS != nil {
			closeWS()
		}
		return "", "", nil, nil, err
	}
	sendBody := map[string]any{"args": []any{reqObj}}
	xm := requestModelHeader(body)
	headers := relayHeaders(snap.AccessToken, xm)
	req, err := http.NewRequest(http.MethodPost,
		strings.TrimRight(snap.SandboxEndpoint, "/")+"/api/electron/agent/send", marshalReader(sendBody))
	if err != nil {
		events.Body.Close()
		if closeWS != nil {
			closeWS()
		}
		return "", "", nil, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := u.do(req, acctID)
	if err != nil {
		events.Body.Close()
		if closeWS != nil {
			closeWS()
		}
		return "", "", nil, nil, upstreamErrf("server", 0, 0, "%v", err)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	text := string(raw)
	if resp.StatusCode >= 400 {
		events.Body.Close()
		if closeWS != nil {
			closeWS()
		}
		return "", "", nil, nil, upstreamErrf(classify(resp.StatusCode, 0, text), resp.StatusCode, 0, "%.200s", text)
	}
	var env struct {
		OK    *bool `json:"ok"`
		Code  any   `json:"code"`
		Error any   `json:"error"`
		Msg   any   `json:"msg"`
		Data  any   `json:"data"`
	}
	if json.Unmarshal(raw, &env) == nil {
		code := toInt64(env.Code)
		msg := toString(env.Error)
		if msg == "" {
			msg = toString(env.Msg)
		}
		if (env.OK != nil && !*env.OK) || (env.Code != nil && code != 0) {
			events.Body.Close()
			closeWS()
			return "", "", nil, nil, upstreamErrf(classify(resp.StatusCode, code, msg), resp.StatusCode, code, "%s", msg)
		}
		runID = extractRunID(env.Data)
		if runID == "" {
			runID = extractRunID(env)
		}
	}
	return runID, session, events, closeWS, nil
}

// openRelayEvents opens the SSE event stream on the sandbox relay.
func (u *upstream) openRelayEvents(token, accountID, base string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(base, "/")+"/api/events", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", bearer(token))
	}
	resp, err := u.do(req, accountID)
	if err != nil {
		return nil, upstreamErrf("server", 0, 0, "%v", err)
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, upstreamErrf(classify(resp.StatusCode, 0, string(raw)), resp.StatusCode, 0, "%.200s", string(raw))
	}
	return resp, nil
}
