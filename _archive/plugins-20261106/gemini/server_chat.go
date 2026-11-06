package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// handleChat converts the OpenAI body, resolves the project, calls the Cloud Code
// upstream, and writes either a translated SSE stream or a folded completion.
//
// The reference rotates endpoints (daily then sandbox) and retries once without
// thought signatures on a signature rejection. This port implements the endpoint
// rotation and keeps the retry to the single documented case, so a transient
// sandbox success is still reachable.
func (s *server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "cannot read request body")
		return
	}
	creds, ok := s.store.get()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "not_configured",
			"Gemini 插件尚未登录：请先在设置页完成 OAuth 登录，或调用 /admin/credentials 写入 access_token")
		return
	}
	if err := s.client.egress.checkEgress(defaultAccountID); err != nil {
		var em *egressMissingError
		if errors.As(err, &em) {
			writeError(w, http.StatusForbidden, "egress_missing", em.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "egress_error", err.Error())
		return
	}
	// Signatures captured from this response must be durable before the next
	// tool-call turn arrives, so the flush runs on the way out.
	defer func() {
		if err := s.sigs.flush(); err != nil {
			fmt.Fprintln(os.Stderr, "gemini: signature cache flush:", err.Error())
		}
	}()
	if creds.nearExpiry(time.Now()) && creds.refreshable() {
		if refreshed, rerr := s.refreshCredentials(r.Context(), creds); rerr == nil {
			creds = refreshed
		}
	}
	if creds.ensureIdentity(time.Now()) {
		// An older credential file gains a real identity on first use.
		_ = s.store.set(creds)
	}

	var meta struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(body, &meta)

	effort := reasoningEffortOf(body)
	if pinned := effortFromModelID(meta.Model); pinned != "" && effort == "" {
		effort = pinned
	}
	spec, err := modelSpecFor(meta.Model, effort)
	if err != nil {
		writeError(w, http.StatusNotFound, "invalid_request_error", err.Error())
		return
	}

	project, resolved, err := s.resolveProject(r.Context(), creds)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	if resolved && project != creds.CloudaicompanionProject && project != defaultProject {
		// Persist the discovery, as persistGeminiProject does. A write failure
		// must not block inference.
		next := creds
		next.CloudaicompanionProject = project
		_ = s.store.set(next)
		creds.CloudaicompanionProject = project
	}

	images := decodeInlineImages(body)
	opts := requestOptions{
		ModelID:         meta.Model,
		Spec:            spec,
		Project:         project,
		Images:          images,
		MaxTokens:       maxTokensOf(body),
		Identity:        creds.Identity,
		LookupSignature: s.sigs.lookup,
	}
	wire, err := translateRequest(body, opts)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "cannot convert body: "+err.Error())
		return
	}

	endpoints := []string{endpointDaily, endpointSandbox}
	if base := strings.TrimSpace(creds.EndpointBase); base != "" {
		endpoints = []string{base}
	}
	resp, lastText, err := s.postStream(r.Context(), creds.AccessToken, endpoints, wire)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "upstream request failed: "+lastText)
		return
	}
	// The documented signature rejection: resend once with every signature
	// dropped. Only that documented case retries; any other 4xx is reported.
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && isSignatureError(lastText) {
		_ = resp.Body.Close()
		opts.DropSignatures = true
		stripped, terr := translateRequest(body, opts)
		if terr != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "cannot convert body: "+terr.Error())
			return
		}
		resp, lastText, err = s.postStream(r.Context(), creds.AccessToken, endpoints, stripped)
		if err != nil {
			writeError(w, http.StatusBadGateway, "upstream_error", "upstream request failed: "+lastText)
			return
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writeError(w, resp.StatusCode, "upstream_error", firstNonEmpty(lastText, "upstream request failed"))
		return
	}

	if meta.Stream {
		s.streamOpenAI(w, resp.Body, meta.Model, s.sigs.put)
		return
	}
	t := newGeminiTranslator(func(map[string]any, any) {})
	t.onSignature = s.sigs.put
	if err := pumpGemini(resp.Body, t); err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "upstream stream error: "+err.Error())
		return
	}
	if t.textBuf.Len() == 0 && len(t.toolCalls) == 0 {
		writeError(w, http.StatusBadGateway, "upstream_error", "upstream returned no content")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(completionFromTranslator(t, meta.Model))
}

// postStream sends the envelope to each endpoint in order and returns the first
// attempt that answered 2xx, together with the last error text for the failure
// path.
//
// Endpoint rotation is the reference's documented behaviour: a 403/404 or a
// switchable 400 (quota/permission/unsupported/project) means the daily
// endpoint cannot serve this account right now, so the sandbox one is tried. A
// 401, a 429, or any other 4xx is returned as-is: rotating would only double
// the failed calls, and the signature retry needs to see the body.
func (s *server) postStream(ctx context.Context, accessToken string, endpoints []string, body []byte) (*http.Response, string, error) {
	var lastText string
	for _, endpoint := range endpoints {
		candidate, err := s.client.stream(ctx, accessToken, defaultAccountID, endpoint, body)
		if err != nil {
			lastText = err.Error()
			continue
		}
		if candidate.StatusCode >= 200 && candidate.StatusCode < 300 {
			return candidate, "", nil
		}
		raw, _ := io.ReadAll(io.LimitReader(candidate.Body, 1<<20))
		_ = candidate.Body.Close()
		lastText = truncate(string(raw), 512)
		status := candidate.StatusCode
		if status == http.StatusUnauthorized || status == http.StatusTooManyRequests {
			return syntheticResponse(status, lastText), lastText, nil
		}
		if status >= 400 && status < 500 && !isSwitchableText(lastText) {
			return syntheticResponse(status, lastText), lastText, nil
		}
	}
	if lastText == "" {
		lastText = "no endpoint answered"
	}
	return nil, lastText, fmt.Errorf("gemini: %s", lastText)
}

// syntheticResponse re-wraps a consumed error body so the caller's normal
// status/body handling still works.
func syntheticResponse(status int, text string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(text)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}

// isSwitchableText is isGeminiSwitchableText: only these 400 reasons mean "this
// endpoint cannot serve the account", so only they rotate.
func isSwitchableText(text string) bool {
	lower := strings.ToLower(text)
	for _, word := range []string{"quota", "permission", "unsupported", "project"} {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return false
}
