package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// dpopKey carries the ES256 key pair used for the DPoP proof on STS calls.
// Only the private JWK is persisted (inside accounts.dat, DPAPI encrypted).
type dpopKey struct {
	private *ecdsa.PrivateKey
	jwk     map[string]string // private JWK: kty, crv, x, y, d
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func b64urlDecode(s string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}

func generateDPoPKey() (*dpopKey, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	size := (priv.Curve.Params().BitSize + 7) / 8
	x := make([]byte, size)
	y := make([]byte, size)
	d := make([]byte, size)
	priv.X.FillBytes(x)
	priv.Y.FillBytes(y)
	priv.D.FillBytes(d)
	return &dpopKey{
		private: priv,
		jwk: map[string]string{
			"kty": "EC",
			"crv": "P-256",
			"x":   b64url(x),
			"y":   b64url(y),
			"d":   b64url(d),
		},
	}, nil
}

func dpopKeyFromJWK(raw json.RawMessage) (*dpopKey, error) {
	if len(raw) == 0 {
		return nil, errors.New("missing dpop_private_key_jwk")
	}
	var jwk map[string]string
	if err := json.Unmarshal(raw, &jwk); err != nil {
		return nil, fmt.Errorf("parse dpop jwk: %w", err)
	}
	if jwk["kty"] != "EC" || jwk["crv"] != "P-256" {
		return nil, errors.New("dpop jwk must be an EC P-256 key")
	}
	xb, err := b64urlDecode(jwk["x"])
	if err != nil {
		return nil, fmt.Errorf("decode jwk x: %w", err)
	}
	yb, err := b64urlDecode(jwk["y"])
	if err != nil {
		return nil, fmt.Errorf("decode jwk y: %w", err)
	}
	db, err := b64urlDecode(jwk["d"])
	if err != nil {
		return nil, fmt.Errorf("decode jwk d: %w", err)
	}
	priv := &ecdsa.PrivateKey{
		PublicKey: ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(xb), Y: new(big.Int).SetBytes(yb)},
		D:         new(big.Int).SetBytes(db),
	}
	if priv.X.Sign() == 0 || priv.Y.Sign() == 0 || priv.D.Sign() == 0 {
		return nil, errors.New("dpop jwk has empty parameters")
	}
	return &dpopKey{private: priv, jwk: jwk}, nil
}

func (k *dpopKey) marshalJWK() json.RawMessage {
	b, _ := json.Marshal(k.jwk)
	return b
}

func (k *dpopKey) publicJWK() map[string]string {
	return map[string]string{
		"kty": k.jwk["kty"],
		"crv": k.jwk["crv"],
		"x":   k.jwk["x"],
		"y":   k.jwk["y"],
	}
}

// signDPoP produces the compact JWS used in the DPoP header (typ dpop+jwt).
// The ECDSA signature is encoded as the fixed-length JOSE r||s form, not ASN.1.
func (k *dpopKey) signDPoP(htm, htu string, now time.Time) (string, error) {
	header := map[string]any{
		"alg": "ES256",
		"typ": "dpop+jwt",
		"jwk": k.publicJWK(),
	}
	payload := map[string]any{
		"htm": htm,
		"htu": htu,
		"iat": now.Unix(),
		"jti": randomHex(32),
	}
	hb, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	pb, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	signingInput := b64url(hb) + "." + b64url(pb)
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, k.private, digest[:])
	if err != nil {
		return "", err
	}
	size := (k.private.Curve.Params().BitSize + 7) / 8
	sig := make([]byte, size*2)
	r.FillBytes(sig[:size])
	s.FillBytes(sig[size:])
	return signingInput + "." + b64url(sig), nil
}
