package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ------------------------------------------------------------------ OAuth

// tokenGrant is the parsed answer of the token endpoint.
type tokenGrant struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
}

// refresh exchanges a refresh_token at oauth2.googleapis.com/token. The form is
// exactly what refreshGeminiCredential posts.
func (c *upstreamClient) refresh(ctx context.Context, accountID, refreshToken string) (tokenGrant, error) {
	form := url.Values{
		"refresh_token": {refreshToken},
		"grant_type":    {"refresh_token"},
	}
	form = withOAuthClient(form)
	return c.token(ctx, accountID, form)
}

// exchangeCode redeems an authorization code. codeVerifier is the PKCE verifier
// that matches the challenge in the authorize URL; an empty verifier omits the
// field (the reference's non-PKCE form).
func (c *upstreamClient) exchangeCode(ctx context.Context, accountID, code, redirectURI, codeVerifier string) (tokenGrant, error) {
	form := url.Values{
		"code":         {code},
		"grant_type":   {"authorization_code"},
		"redirect_uri": {redirectURI},
	}
	if strings.TrimSpace(codeVerifier) != "" {
		form.Set("code_verifier", codeVerifier)
	}
	form = withOAuthClient(form)
	return c.token(ctx, accountID, form)
}

// grantRefusedError is a token-endpoint refusal of the authorization code
// itself (invalid_grant and friends). The operator's input is wrong; a dial
// failure or a 5xx must not be confused with it.
type grantRefusedError struct{ msg string }

func (e *grantRefusedError) Error() string { return e.msg }

// oauthConfigError is a token-endpoint refusal caused by missing configuration
// on this machine rather than by a bad grant. It is surfaced as 501 so the
// operator is told exactly which environment variable to set.
type oauthConfigError struct{ msg string }

func (e *oauthConfigError) Error() string { return e.msg }

// withOAuthClient adds client_id, and client_secret only when the operator
// configured one. An absent secret is left out of the form entirely rather than
// sent as an empty or placeholder value, which the token endpoint would reject.
func withOAuthClient(form url.Values) url.Values {
	form.Set("client_id", clientID)
	if secret := clientSecretOrEmpty(); secret != "" {
		form.Set("client_secret", secret)
	}
	return form
}

// token POSTs the form and parses the grant.
func (c *upstreamClient) token(ctx context.Context, accountID string, form url.Values) (tokenGrant, error) {
	ctx, cancel := context.WithTimeout(ctx, creditsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenGrant{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client, err := c.httpFor(accountID)
	if err != nil {
		return tokenGrant{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return tokenGrant{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return tokenGrant{}, err
	}
	var parsed struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		TokenType        string `json:"token_type"`
		ExpiresIn        int    `json:"expires_in"`
		Scope            string `json:"scope"`
		IDToken          string `json:"id_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return tokenGrant{}, fmt.Errorf("gemini: token endpoint returned unparseable JSON (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || parsed.Error != "" {
		code := parsed.Error
		if code == "" {
			code = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		if parsed.ErrorDescription != "" {
			code += ": " + parsed.ErrorDescription
		}
		// A client_secret refusal with no secret configured is not a bad code:
		// it is a configuration the operator can fix in one step.
		if clientSecretOrEmpty() == "" &&
			(parsed.Error == "invalid_client" || strings.Contains(strings.ToLower(parsed.ErrorDescription), "client_secret")) {
			return tokenGrant{}, &oauthConfigError{msg: "gemini: Google refused this OAuth client (invalid_client). " +
				"Set " + clientSecretEnv + " in the host environment and retry; the value is read at run time and never stored."}
		}
		return tokenGrant{}, &grantRefusedError{msg: "gemini: token endpoint refused the grant: " + code}
	}
	if parsed.AccessToken == "" {
		return tokenGrant{}, fmt.Errorf("gemini: token endpoint returned no access_token")
	}
	g := tokenGrant{
		AccessToken:  parsed.AccessToken,
		RefreshToken: parsed.RefreshToken,
		TokenType:    firstNonEmpty(parsed.TokenType, "Bearer"),
		ExpiresIn:    parsed.ExpiresIn,
		Scope:        parsed.Scope,
		IDToken:      parsed.IDToken,
	}
	if g.ExpiresIn <= 0 {
		g.ExpiresIn = 3600
	}
	return g, nil
}

// userInfo reads the account identity from the userinfo endpoint. It returns zero
// values on failure: the reference treats it as a best-effort enrichment.
func (c *upstreamClient) userInfo(ctx context.Context, accessToken, accountID string) (sub, email string) {
	ctx, cancel := context.WithTimeout(ctx, creditsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.userInfoURL, nil)
	if err != nil {
		return "", ""
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	client, err := c.httpFor(accountID)
	if err != nil {
		return "", ""
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", ""
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", ""
	}
	var payload struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return "", ""
	}
	return payload.ID, payload.Email
}

// authURL builds the consent URL. The query is buildGeminiAuthUrl exactly, plus
// the PKCE challenge when one was generated: offline access and prompt=consent so
// a refresh_token always comes back.
func authURL(redirectURI, state, codeChallenge string) string {
	q := url.Values{
		"client_id":              {clientID},
		"response_type":          {"code"},
		"redirect_uri":           {redirectURI},
		"scope":                  {strings.Join(scopes, " ")},
		"state":                  {state},
		"access_type":            {"offline"},
		"include_granted_scopes": {"true"},
		"prompt":                 {"consent"},
	}
	if codeChallenge != "" {
		q.Set("code_challenge", codeChallenge)
		q.Set("code_challenge_method", "S256")
	}
	return authEndpoint + "?" + q.Encode()
}
