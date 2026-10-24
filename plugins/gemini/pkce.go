package main

// PKCE (RFC 7636) for the loopback authorization-code flow.
//
// Google's installed-app flow accepts a PKCE challenge in addition to the
// client_id, and sending one means a leaked authorization code cannot be
// redeemed by another process watching the loopback port. The verifier is
// generated per flow, never persisted, and sent only on the token request.
//
// The reference bundle (gemini-oauth.ts) omits PKCE; this port adds it as a
// strict superset: the authorize URL carries code_challenge and the exchange
// carries code_verifier. A server that ignores the extra parameter behaves
// exactly as before.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// generatePKCE returns a verifier and its S256 challenge, both base64url
// without padding, as RFC 7636 §4.1/§4.2 require.
func generatePKCE() (verifier, challenge string) {
	var b [48]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A random failure must not silently produce a weak verifier: an
		// all-zero 48-byte verifier is still 384 bits of fixed input, which
		// would be guessable. Refuse by returning an empty pair, and the
		// caller then starts the flow without PKCE rather than with a bad one.
		return "", ""
	}
	verifier = base64.RawURLEncoding.EncodeToString(b[:])
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge
}
