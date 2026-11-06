package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// refreshAuth POSTs the keyfrom body plus refreshToken to /api/auth/refresh and
// returns the new token tuple. Ported from lobsteraiRefreshBody +
// refreshCredential; latestKeyfrom is NOT updated by a refresh.
func (c *upstreamClient) refreshAuth(ctx context.Context, base string, cred credentials, version string, proxy string) (tokenPayload, error) {
	body, err := json.Marshal(map[string]string{
		"firstKeyfrom":  cred.FirstKeyfrom,
		"latestKeyfrom": cred.LatestKeyfrom,
		"version":       version,
		"uuid":          cred.UUID,
		"userId":        cred.UserID,
		"refreshToken":  cred.RefreshToken,
	})
	if err != nil {
		return tokenPayload{}, err
	}
	fetchCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodPost, originOf(base)+refreshPath, bytes.NewReader(body))
	if err != nil {
		return tokenPayload{}, err
	}
	req.Header = anonymousHeaders()
	resp, err := c.clientFor(proxy).Do(req)
	if err != nil {
		return tokenPayload{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return tokenPayload{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return tokenPayload{}, fmt.Errorf("refresh HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	return parseLobsteraiTokenPayload(raw)
}

// --- header sets, ported verbatim from lobsterai.ts ---

// authHeaders is lobsteraiAuthHeaders.
func authHeaders(token, accept string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Accept", accept)
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", userAgent)
	return h
}

// chatHeaders is lobsteraiChatHeaders: auth + the two X-LobsterAI-* headers.
func chatHeaders(token, clientVersion string) http.Header {
	h := authHeaders(token, "text/event-stream, application/json")
	h.Set("X-LobsterAI-Client-Capabilities", clientCapabilities)
	h.Set("X-LobsterAI-Client-Version", clientVersion)
	return h
}

// modelsHeaders is lobsteraiModelsHeaders.
func modelsHeaders(token, clientVersion string) http.Header {
	h := authHeaders(token, "application/json")
	h.Set("X-LobsterAI-Client-Capabilities", clientCapabilities)
	h.Set("X-LobsterAI-Client-Version", clientVersion)
	return h
}

// anonymousHeaders is lobsteraiAnonymousHeaders, used by the token exchange.
func anonymousHeaders() http.Header {
	h := http.Header{}
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", userAgent)
	return h
}

// exchangeAuthCode posts a portal auth code and returns the token payload.
//
// Ported from exchangeLobsteraiAuthCode: the body carries authCode plus the
// keyfrom identity tuple and the version; no credential is needed.
func (c *upstreamClient) exchangeAuthCode(ctx context.Context, base, code string, session loginSession, version string, proxy string) (credentials, error) {
	body, err := json.Marshal(map[string]string{
		"authCode":      code,
		"firstKeyfrom":  session.FirstKeyfrom,
		"latestKeyfrom": strconvNow(),
		"uuid":          session.UUID,
		"version":       version,
	})
	if err != nil {
		return credentials{}, err
	}
	fetchCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodPost, originOf(base)+exchangePath, bytes.NewReader(body))
	if err != nil {
		return credentials{}, err
	}
	req.Header = anonymousHeaders()
	resp, err := c.clientFor(proxy).Do(req)
	if err != nil {
		return credentials{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return credentials{}, err
	}
	payload, err := parseLobsteraiTokenPayload(raw)
	if err != nil {
		return credentials{}, err
	}
	if payload.AccessToken == "" {
		return credentials{}, fmt.Errorf("lobsterai: exchange response has no accessToken")
	}
	return payload.toCredentials(session), nil
}
