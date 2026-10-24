package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var errRefreshTokenExpired = errors.New("codearts refresh token is expired or was already rotated")

type tokenResponse struct {
	Credentials struct {
		AccessKeyID     string `json:"access_key_id"`
		SecretAccessKey string `json:"secret_access_key"`
		SecurityToken   string `json:"security_token"`
		Expiration      string `json:"expiration"`
		DomainID        string `json:"domain_id"`
		UserID          string `json:"user_id"`
		UserName        string `json:"user_name"`
	} `json:"credentials"`
	RefreshToken string `json:"refresh_token"`
	DomainID     string `json:"domain_id"`
	UserID       string `json:"user_id"`
	UserName     string `json:"user_name"`
	Error        string `json:"error"`
	ErrorCode    string `json:"error_code"`
	ErrorDesc    string `json:"error_description"`
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func (p *Plugin) httpClientFor(scope, proxyURL string) *http.Client {
	return &http.Client{Transport: p.transportFor(scope, proxyURL)}
}

// requestToken posts to the STS endpoint with a fresh DPoP proof. The proof
// key is the account's persisted key so refresh keeps working after restarts.
func (p *Plugin) requestToken(ctx context.Context, scope, proxyURL string, form url.Values, key *dpopKey) (tokenResponse, error) {
	// TOKEN_TIMEOUT_MS in the reference implementation.
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var out tokenResponse
	proof, err := key.signDPoP(http.MethodPost, p.endpoints.STS, p.now())
	if err != nil {
		return out, fmt.Errorf("sign DPoP: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoints.STS, strings.NewReader(form.Encode()))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("DPoP", proof)
	client := p.httpClientFor(scope, proxyURL)
	resp, err := client.Do(req)
	if err != nil {
		return out, fmt.Errorf("CodeArts token request network error: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = json.Unmarshal(body, &out)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || out.Credentials.AccessKeyID == "" {
		msg := fmt.Sprintf("CodeArts token request failed: %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
		if out.Error == "invalid_grant" || strings.Contains(out.ErrorCode, "ExpiredRefreshToken") || strings.Contains(string(body), "ExpiredRefreshToken") {
			return out, fmt.Errorf("%w: %s", errRefreshTokenExpired, msg)
		}
		return out, errors.New(msg)
	}
	return out, nil
}

func credentialFromToken(t tokenResponse, verifier string, key *dpopKey) Credential {
	return Credential{
		AccessKeyID:     t.Credentials.AccessKeyID,
		SecretAccessKey: t.Credentials.SecretAccessKey,
		SecurityToken:   t.Credentials.SecurityToken,
		ExpiresAt:       t.Credentials.Expiration,
		RefreshToken:    t.RefreshToken,
		CodeVerifier:    verifier,
		DPoPPrivateJWK:  key.marshalJWK(),
		DomainID:        firstNonEmpty(t.Credentials.DomainID, t.DomainID),
		UserID:          firstNonEmpty(t.Credentials.UserID, t.UserID),
		UserName:        firstNonEmpty(t.Credentials.UserName, t.UserName),
	}
}

func (p *Plugin) exchangeAuthorizationCode(ctx context.Context, scope, proxyURL, code, verifier string, port int, key *dpopKey) (Credential, error) {
	form := url.Values{
		"client_id":     {oauthClientID},
		"code":          {code},
		"code_verifier": {verifier},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {fmt.Sprintf("http://127.0.0.1:%d%s", port, oauthRedirectPath)},
	}
	t, err := p.requestToken(ctx, scope, proxyURL, form, key)
	if err != nil {
		return Credential{}, err
	}
	return credentialFromToken(t, verifier, key), nil
}

func (p *Plugin) exchangeRefreshToken(ctx context.Context, scope, proxyURL string, cred Credential) (Credential, error) {
	key, err := dpopKeyFromJWK(cred.DPoPPrivateJWK)
	if err != nil {
		return Credential{}, err
	}
	form := url.Values{
		"client_id":     {oauthClientID},
		"code_verifier": {cred.CodeVerifier},
		"grant_type":    {"refresh_token"},
		"refresh_token": {cred.RefreshToken},
	}
	t, err := p.requestToken(ctx, scope, proxyURL, form, key)
	if err != nil {
		return Credential{}, err
	}
	next := credentialFromToken(t, cred.CodeVerifier, key)
	next.DomainID = cred.DomainID
	next.UserID = cred.UserID
	next.UserName = cred.UserName
	return next, nil
}

// ensureCredential returns an account whose access token is usable, refreshing
// it when it expires within one hour (or unconditionally when force is true).
// Refreshes are serialized per account; the old refresh token becomes invalid
// as soon as STS issues a new one.
func (p *Plugin) ensureCredential(ctx context.Context, accountID string, force bool) (*Account, error) {
	lock := p.refreshLock(accountID)
	lock.Lock()
	defer lock.Unlock()

	acct, ok := p.findAccount(accountID)
	if !ok {
		return nil, fmt.Errorf("account %s not found", accountID)
	}
	now := p.now()
	exp := acct.Credential.expiry()
	refreshable := acct.Credential.refreshable()
	if acct.Credential.usable() && !force && (exp.IsZero() || now.Add(credentialRefreshLead).Before(exp)) {
		return acct, nil
	}
	if !refreshable {
		if acct.Credential.usable() && (exp.IsZero() || now.Before(exp)) {
			return acct, nil
		}
		msg := "CodeArts 凭据已过期，且没有 refresh_token，请重新登录"
		// A missing refresh token is an expired credential, not a corrupted
		// account: keep status "expired" so the UI can prompt a re-login.
		p.setAccountError(accountID, msg, false)
		return nil, errors.New(msg)
	}

	entry, err := p.resolveEgress(accountID)
	if err != nil {
		var em *egressMissingError
		if errors.As(err, &em) {
			return nil, err
		}
		return nil, err
	}
	proxyURL := ""
	if entry.Mode == egressProxy {
		proxyURL = entry.URL
	}
	next, err := p.exchangeRefreshToken(ctx, accountID, proxyURL, acct.Credential)
	if err != nil {
		if errors.Is(err, errRefreshTokenExpired) {
			p.setAccountError(accountID, "refresh_token 已失效，请重新登录", true)
		} else {
			p.setAccountError(accountID, err.Error(), false)
		}
		return nil, err
	}
	p.mu.Lock()
	if a := p.accountByIDLocked(accountID); a != nil {
		a.Credential = next
		a.Invalid = false
		a.LastError = ""
		acct = deepCopyAccount(a)
	}
	p.mu.Unlock()
	if err := p.saveAccounts(); err != nil {
		p.logLine("warn", "save accounts failed", map[string]any{"error": err.Error()})
	}
	return acct, nil
}

func (p *Plugin) setAccountError(accountID, msg string, invalid bool) {
	p.mu.Lock()
	if a := p.accountByIDLocked(accountID); a != nil {
		a.LastError = msg
		if invalid {
			a.Invalid = true
		}
	}
	p.mu.Unlock()
	if err := p.saveAccounts(); err != nil {
		p.logLine("warn", "save accounts failed", map[string]any{"error": err.Error()})
	}
}

// signedRequest builds a signed SNAP API request. extraSigned is included in
// the signature; extraHeaders are added afterwards without being signed.
func (p *Plugin) signedRequest(ctx context.Context, method, rawURL string, body []byte, acct *Account, proxyURL string, scope string, extraSigned, extraHeaders map[string]string) (*http.Response, error) {
	headers, err := signRequestHuawei(method, rawURL, body, acct.Credential.AccessKeyID, acct.Credential.SecretAccessKey, acct.Credential.SecurityToken, extraSigned, p.now())
	if err != nil {
		return nil, err
	}
	var rdr io.Reader
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		switch strings.ToLower(k) {
		case "host":
			// The Host header is derived from the URL.
		case "authorization":
			req.Header.Set("Authorization", v)
		default:
			req.Header.Set(k, v)
		}
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	if scope == "" {
		scope = acct.ID
	}
	return p.httpClientFor(scope, proxyURL).Do(req)
}

func (p *Plugin) egressProxyFor(accountID string) (string, error) {
	entry, err := p.resolveEgress(accountID)
	if err != nil {
		return "", err
	}
	if entry.Mode == egressProxy {
		return entry.URL, nil
	}
	return "", nil
}
