package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type stsRecorder struct {
	mu          sync.Mutex
	forms       []url.Values
	dpopJWKs    []string
	current     string
	next        int
	expiryDelta time.Duration
}

func (s *stsRecorder) handler(t *testing.T, now func() time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		s.mu.Lock()
		s.forms = append(s.forms, r.PostForm)
		proof := r.Header.Get("DPoP")
		if proof != "" {
			parts := strings.Split(proof, ".")
			if len(parts) == 3 {
				if hb, err := base64.RawURLEncoding.DecodeString(parts[0]); err == nil {
					var h struct {
						JWK map[string]string `json:"jwk"`
					}
					_ = json.Unmarshal(hb, &h)
					s.dpopJWKs = append(s.dpopJWKs, h.JWK["x"]+"|"+h.JWK["y"])
				}
			}
		}
		grant := r.PostForm.Get("grant_type")
		delta := s.expiryDelta
		s.mu.Unlock()

		expiry := now().Add(delta).UTC().Format(time.RFC3339)
		if grant == "authorization_code" {
			s.mu.Lock()
			s.current = "R1"
			s.next = 2
			s.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{
				"credentials": map[string]any{
					"access_key_id":     "AK1",
					"secret_access_key": "SK1",
					"security_token":    "ST1",
					"expiration":        expiry,
				},
				"refresh_token": "R1",
			})
			return
		}
		if grant == "refresh_token" {
			s.mu.Lock()
			valid := r.PostForm.Get("refresh_token") == s.current
			n := s.next
			nextToken := fmt.Sprintf("R%d", n)
			if valid {
				s.current = nextToken
				s.next++
			}
			s.mu.Unlock()
			if !valid {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"expired"}`))
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"credentials": map[string]any{
					"access_key_id":     fmt.Sprintf("AK%d", n),
					"secret_access_key": fmt.Sprintf("SK%d", n),
					"security_token":    fmt.Sprintf("ST%d", n),
					"expiration":        expiry,
				},
				"refresh_token": nextToken,
			})
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}
}

func TestTokenExchangeAndRefreshRotation(t *testing.T) {
	rec := &stsRecorder{expiryDelta: 30 * time.Minute}
	clock := newFakeClock()
	sts := httptest.NewServer(rec.handler(t, clock.Now))
	defer sts.Close()

	ep := testEndpoints()
	ep.STS = sts.URL
	p, _ := newTestPlugin(t, func(o *Options) {
		o.Endpoints = ep
		o.Now = clock.Now
	})

	key, err := generateDPoPKey()
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	ctx := context.Background()
	cred, err := p.exchangeAuthorizationCode(ctx, "login", "", "CODE-1", "VERIFIER-1", 23456, key)
	if err != nil {
		t.Fatalf("exchange code: %v", err)
	}
	if cred.AccessKeyID != "AK1" || cred.SecretAccessKey != "SK1" || cred.SecurityToken != "ST1" || cred.RefreshToken != "R1" {
		t.Fatalf("credential = %+v", cred)
	}
	if cred.CodeVerifier != "VERIFIER-1" || len(cred.DPoPPrivateJWK) == 0 {
		t.Fatalf("credential PKCE/DPoP missing: %+v", cred)
	}

	rec.mu.Lock()
	first := rec.forms[0]
	rec.mu.Unlock()
	want := map[string]string{
		"client_id":     "codearts-agent",
		"code":          "CODE-1",
		"code_verifier": "VERIFIER-1",
		"grant_type":    "authorization_code",
		"redirect_uri":  "http://127.0.0.1:23456/oauth/callback",
	}
	for k, v := range want {
		if first.Get(k) != v {
			t.Fatalf("token form %s = %q, want %q", k, first.Get(k), v)
		}
	}

	acct, err := p.upsertAccount(cred, clock.Now())
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// The refreshed token will be valid for two hours.
	rec.mu.Lock()
	rec.expiryDelta = 2 * time.Hour
	rec.mu.Unlock()

	// 30 minutes of validity is inside the one-hour refresh lead: a request
	// must rotate the token before using it.
	updated, err := p.ensureCredential(ctx, acct.ID, false)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if updated.Credential.AccessKeyID != "AK2" || updated.Credential.RefreshToken != "R2" {
		t.Fatalf("refreshed credential = %+v", updated.Credential)
	}
	if updated.Credential.CodeVerifier != "VERIFIER-1" || updated.Credential.UserID != cred.UserID {
		t.Fatalf("refresh must preserve verifier and identity: %+v", updated.Credential)
	}
	stored, _ := p.findAccount(acct.ID)
	if stored.Credential.RefreshToken != "R2" || stored.Credential.AccessKeyID != "AK2" {
		t.Fatalf("stored credential = %+v", stored.Credential)
	}

	rec.mu.Lock()
	second := rec.forms[1]
	jwks := append([]string(nil), rec.dpopJWKs...)
	rec.mu.Unlock()
	if second.Get("grant_type") != "refresh_token" || second.Get("refresh_token") != "R1" || second.Get("client_id") != "codearts-agent" {
		t.Fatalf("refresh form = %v", second)
	}
	if second.Get("code_verifier") != "VERIFIER-1" {
		t.Fatalf("refresh must reuse the stored code_verifier: %v", second)
	}
	if len(jwks) != 2 || jwks[0] != jwks[1] {
		t.Fatalf("DPoP key must stay stable across refresh: %v", jwks)
	}

	// The old refresh token is invalid after rotation.
	if _, err := p.exchangeRefreshToken(ctx, acct.ID, "", cred); !errors.Is(err, errRefreshTokenExpired) {
		t.Fatalf("old refresh token error = %v, want errRefreshTokenExpired", err)
	}

	// With a fresh two-hour token no network call happens.
	before := len(rec.forms)
	if _, err := p.ensureCredential(ctx, acct.ID, false); err != nil {
		t.Fatalf("ensure fresh: %v", err)
	}
	if len(rec.forms) != before {
		t.Fatalf("fresh credential triggered refresh: %v", rec.forms[before:])
	}

	// A manual refresh always goes to the network.
	if _, err := p.ensureCredential(ctx, acct.ID, true); err != nil {
		t.Fatalf("force refresh: %v", err)
	}
	if len(rec.forms) != before+1 {
		t.Fatalf("force refresh did not call STS")
	}
}
