package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// capabilities mirrors manifest.json "capabilities". embeddings/images are
// deliberately absent: the host answers 501 for those on every plugin model
// (internal/gateway/gateway.go handlePassThrough), so declaring them would be a
// promise this host cannot keep.
//
// checkin/refresh are absent because this plugin implements neither (the route
// answers 501), and quota is absent because the assistant exposes no per-account
// credit figure: every account carried "quota": {}. Declaring quota would make
// the host poll GET /accounts every 30 minutes (internal/schedule/quota.go) and
// treat the unreported Remaining=0 as low (quotaLow falls back to
// Remaining < 100 when Total is 0), producing a daily false "额度不足：aichat"
// notification for a channel that is not actually depleted.
var capabilities = []string{"upstream", "accounts"}

// proxyPaths are the inference endpoints forwarded verbatim to the assistant.
// The host rewrites "model" to the plugin-local id before calling us, so the
// body needs no rewriting here; the path is appended to base_url as-is.
var proxyPaths = map[string]bool{
	"/v1/chat/completions": true,
	"/v1/completions":      true,
	"/v1/responses":        true,
	"/v1/messages":         true,
}

func isProxyPath(p string) bool { return proxyPaths[p] }

// requestBodyLimit caps an inference request body. The host itself caps at 8
// MiB for control calls; multimodal prompts can be larger, so allow 32 MiB.
const requestBodyLimit = 32 << 20

// hopRequestHeaders are never forwarded upstream: Go manages them or they only
// describe the host<->plugin hop.
var hopRequestHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
	"Host", "Content-Length", "Authorization",
}

// handleModels forwards GET /v1/models to the assistant and returns its list
// verbatim (OpenAI shape {"object":"list","data":[...]}), which the host's
// parseModelsBody accepts. The manifest declares "models": [] on purpose so the
// host asks here instead of using a frozen static table.
func (s *server) handleModels(w http.ResponseWriter, r *http.Request) {
	base, key, ok := s.store.get()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "upstream_not_configured",
			"插件尚未配置上游 base_url（POST /admin/upstream）")
		return
	}
	q := r.URL.RawQuery
	target := base + "/v1/models"
	if q != "" {
		target += "?" + q
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "build upstream request: "+err.Error())
		return
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_unavailable", "上游不可达: "+err.Error())
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_unavailable", "读取上游响应失败: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

// handleProxy forwards an inference request to the assistant and streams the
// answer back byte for byte.
//
// SSE must not be buffered: the host forwards our writes to the client as they
// arrive, and every flush here becomes a visible token chunk there. So the body
// is copied through a 32 KiB buffer and each chunk is flushed immediately; the
// status code and response headers are taken from the upstream response.
func (s *server) handleProxy(w http.ResponseWriter, r *http.Request) {
	base, key, ok := s.store.get()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "upstream_not_configured",
			"插件尚未配置上游 base_url（POST /admin/upstream）")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, requestBodyLimit))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}

	target := base + path(r)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "build upstream request: "+err.Error())
		return
	}
	copyUpstreamHeaders(req.Header, r.Header)
	// The client's Authorization is the host's plugin token, not the
	// assistant's key: replace it, never forward both.
	req.Header.Del("Authorization")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := s.client.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// The client went away mid-request; the host has already logged
			// it as cancelled, and there is nobody left to answer.
			return
		}
		writeError(w, http.StatusBadGateway, "upstream_unavailable", "上游不可达: "+err.Error())
		return
	}
	defer resp.Body.Close()

	copyDownstreamHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	// Flush after every write so an SSE chunk reaches the client immediately
	// instead of waiting for a bigger buffer to fill. The 4 KiB copy buffer is
	// deliberately small: one SSE event is normally well below it, so the relay
	// adds no visible latency before the flush.
	buf := make([]byte, 4<<10)
	_, copyErr := io.CopyBuffer(flushWriter{w: w}, resp.Body, buf)
	if copyErr != nil && !errors.Is(copyErr, context.Canceled) {
		// The status line is already sent, so log instead of writing an error
		// envelope that would corrupt the SSE stream.
		s.log("warn", "upstream_stream_aborted", map[string]any{
			"path":  path(r),
			"error": copyErr.Error(),
		})
	}
}

// flushWriter flushes the ResponseWriter after each chunk.
type flushWriter struct{ w http.ResponseWriter }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

// copyUpstreamHeaders copies the client's headers, dropping hop-by-hop ones,
// the host's own bookkeeping headers and anything that leaks host internals.
func copyUpstreamHeaders(dst, src http.Header) {
	for _, h := range hopRequestHeaders {
		dst.Del(h)
	}
	for k, vals := range src {
		canonical := http.CanonicalHeaderKey(k)
		if isHopHeader(canonical) || strings.HasPrefix(strings.ToLower(canonical), "x-aiworkgate-") ||
			isGatewayCredentialHeader(canonical) {
			continue
		}
		for _, v := range vals {
			dst.Add(canonical, v)
		}
	}
}

// isGatewayCredentialHeader lists the header spellings the host accepts as the
// client's gateway key (internal/gateway/gateway.go clientToken). The host
// replaces only Authorization when it calls the plugin, so these would otherwise
// be copied to the assistant: that key belongs to the gateway, while the
// assistant gets its own stored key in Authorization.
func isGatewayCredentialHeader(h string) bool {
	switch http.CanonicalHeaderKey(h) {
	case "X-Client-Key", "X-Api-Key", "Api-Key":
		return true
	}
	return false
}

// copyDownstreamHeaders copies the upstream response headers, dropping the ones
// that describe the hop rather than the payload. Content-Length is kept so a
// non-streaming answer keeps its framing.
func copyDownstreamHeaders(dst, src http.Header) {
	for k, vals := range src {
		canonical := http.CanonicalHeaderKey(k)
		if isHopHeader(canonical) || canonical == "Connection" {
			continue
		}
		for _, v := range vals {
			dst.Add(canonical, v)
		}
	}
}

func isHopHeader(h string) bool {
	switch http.CanonicalHeaderKey(h) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	}
	return false
}

// handleAdminUpstream stores the assistant's base_url and key in the plugin's
// own sealed file. This is the seam the host's one-time migration (Q18/R1) will
// call; it is also how a human configures the plugin by hand.
//
// It requires the plugin token, so only the host (or someone holding the token)
// can set credentials. The response never echoes the key.
func (s *server) handleAdminUpstream(w http.ResponseWriter, r *http.Request) {
	var in struct {
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if err := s.store.set(in.BaseURL, in.APIKey); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	base, key, _ := s.store.get()
	s.log("info", "upstream_configured", map[string]any{
		"base_url":        base,
		"key_present":     key != "",
		"key_fingerprint": fingerprint(key),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"base_url":        base,
		"key_present":     key != "",
		"key_fingerprint": fingerprint(key),
	})
}

// fingerprint is a short, non-reversible tag of a key so logs can tell two
// rotations apart without exposing the secret.
func fingerprint(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("sha256:%x", sum[:4])
}
