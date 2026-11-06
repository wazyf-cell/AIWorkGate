package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// authSchemes lists the upstream auth shapes this credential can present, in
// try order. Basic/HMAC (OfficeAce's model app key) wins when configured; a
// session that only carries maas_api_key uses the MaaS bearer key instead.
func authSchemes(c *Credential) []string {
	if c.HasBasic() || c.HasHMAC() {
		schemes := make([]string, 0, 2)
		if c.HasBasic() {
			schemes = append(schemes, "basic")
		}
		if c.HasHMAC() {
			schemes = append(schemes, "hmac")
		}
		return schemes
	}
	if c.HasMaasKey() {
		return []string{"bearer"}
	}
	return nil
}

// authHeaders builds the request headers for one auth scheme. HMAC is signed
// per attempt so a refresh between attempts is reflected.
func (p *Plugin) authHeaders(c *Credential, scheme, endpoint string, body []byte) (map[string]string, error) {
	switch scheme {
	case "basic":
		basic := "Basic " + base64.StdEncoding.EncodeToString([]byte(c.ModelAppKey+":"+c.ModelAppSecret))
		return map[string]string{"Authorization": basic, "Content-Type": "application/json"}, nil
	case "bearer":
		return map[string]string{"Authorization": "Bearer " + c.MaasAPIKey, "Content-Type": "application/json"}, nil
	case "hmac":
		signed, err := signSDKHMAC(signInput{
			Method:      http.MethodPost,
			URL:         endpoint,
			Body:        string(body),
			ContentType: "application/json",
			Credential: signCredential{
				AccessKey:     c.ModelAppKey,
				SecretKey:     c.ModelAppSecret,
				SecurityToken: c.STSToken,
				ProjectID:     c.ProjectID,
			},
			Now: p.now(),
		})
		if err != nil {
			return nil, err
		}
		return signed.Header, nil
	default:
		return nil, fmt.Errorf("unsupported auth scheme %q", scheme)
	}
}

func hasScheme(schemes []string, want string) bool {
	for _, s := range schemes {
		if s == want {
			return true
		}
	}
	return false
}

// sendChat tries each supported auth scheme in order, refreshing once on the
// first 401/403. refreshOnAuth is false for hedged requests (no refresh, no
// replay) or when auto refresh is off.
func (p *Plugin) sendChat(ctx context.Context, cred *Credential, accountID, proxyURL, endpoint string, body []byte, refreshOnAuth bool) (*http.Response, string, error) {
	schemes := authSchemes(cred)
	if len(schemes) == 0 {
		return nil, "", &upstreamError{Status: http.StatusUnauthorized, Body: "no usable upstream credential"}
	}
	var last *upstreamError
	refreshed := false
	for i := 0; i < len(schemes); i++ {
		headers, err := p.authHeaders(cred, schemes[i], endpoint, body)
		if err != nil {
			return nil, "", err
		}
		resp, err := p.postMaaS(ctx, accountID, proxyURL, endpoint, body, headers)
		if err != nil {
			return nil, "", err
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, schemes[i], nil
		}
		status := resp.StatusCode
		bodyText, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if status != http.StatusUnauthorized && status != http.StatusForbidden {
			return nil, "", &upstreamError{Status: status, Body: string(bodyText)}
		}
		last = &upstreamError{Status: status, Body: string(bodyText)}
		if !refreshOnAuth || refreshed {
			continue
		}
		// Refresh once on 401/403, then continue with the remaining schemes.
		refreshed = true
		if refreshedCred, rerr := p.refreshCredential(ctx, cred, accountID, proxyURL, false); rerr == nil {
			cred = refreshedCred
			if cred.HasHMAC() && !hasScheme(schemes, "hmac") {
				schemes = append(schemes, "hmac")
			}
		} else {
			p.logLine("warn", "refresh after upstream 401/403 failed", map[string]any{"account": accountID, "error": rerr.Error()})
		}
	}
	if last == nil {
		last = &upstreamError{Status: http.StatusBadGateway, Body: "upstream rejected every auth scheme"}
	}
	return nil, "", last
}

func (p *Plugin) postMaaS(ctx context.Context, accountID, proxyURL, endpoint string, body []byte, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range headers {
		if strings.EqualFold(k, "Host") {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Transport: p.transportFor(accountID, proxyURL)}
	return client.Do(req)
}
