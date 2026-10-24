package secrets

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const (
	// ClientKeyPrefix starts every client API key.
	ClientKeyPrefix = "agk_"
	// ClientKeyIDPrefix starts every client key id.
	ClientKeyIDPrefix = "ck_"
	// ClientKeyPrefixLen is how many leading characters of the plain key are
	// kept as a human-recognizable display prefix.
	ClientKeyPrefixLen = 8
	// ClientKeyHashHexLen is the length of a hex-encoded SHA-256 hash.
	ClientKeyHashHexLen = sha256.Size * 2

	// clientKeyBytes of randomness yield exactly 32 URL-safe base64 characters.
	clientKeyBytes = 24
	// clientKeyIDBytes of randomness yield exactly 8 hex characters.
	clientKeyIDBytes = 4
)

// NewClientKey mints a client key. Only hash and prefix may be persisted; the
// plain value is shown to the user once. The key has the form
// "agk_" + 32 URL-safe random characters.
func NewClientKey() (plain, id, prefix, hash string) {
	body := make([]byte, clientKeyBytes)
	mustRand(body)
	plain = ClientKeyPrefix + base64.RawURLEncoding.EncodeToString(body)
	zero(body)

	rawID := make([]byte, clientKeyIDBytes)
	mustRand(rawID)
	id = ClientKeyIDPrefix + hexString(rawID)
	zero(rawID)

	prefix = plain[:ClientKeyPrefixLen]
	hash = HashClientKey(plain)
	return plain, id, prefix, hash
}

// HashClientKey returns the lowercase hex SHA-256 of a plain client key.
func HashClientKey(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hexString(sum[:])
}

// AddClientKey stores the SHA-256 hash for a client key id.
func (s *Store) AddClientKey(id, hash string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("secrets: empty client key id")
	}
	normalized, err := normalizeClientKeyHash(hash)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initMaps()
	old, had := s.blob.ClientKeyHashes[id]
	s.blob.ClientKeyHashes[id] = normalized
	if err := s.saveLocked(); err != nil {
		if had {
			s.blob.ClientKeyHashes[id] = old
		} else {
			delete(s.blob.ClientKeyHashes, id)
		}
		return err
	}
	return nil
}

// VerifyClientKey hashes plain and compares it against every stored hash in
// constant time. It returns the matching key id, or ok=false.
func (s *Store) VerifyClientKey(plain string) (string, bool) {
	if plain == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(plain))

	s.mu.Lock()
	defer s.mu.Unlock()

	var matched string
	var hit int
	for id, stored := range s.blob.ClientKeyHashes {
		storedBytes, err := hex.DecodeString(stored)
		if err != nil || len(storedBytes) != sha256.Size {
			continue
		}
		eq := subtle.ConstantTimeCompare(storedBytes, sum[:])
		if eq == 1 && hit == 0 {
			matched = id
		}
		hit |= eq
	}
	if hit != 1 {
		return "", false
	}
	return matched, true
}

// RemoveClientKey deletes a client key hash. Unknown ids are a no-op.
func (s *Store) RemoveClientKey(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, had := s.blob.ClientKeyHashes[id]
	if !had {
		return nil
	}
	delete(s.blob.ClientKeyHashes, id)
	if err := s.saveLocked(); err != nil {
		s.blob.ClientKeyHashes[id] = old
		return err
	}
	return nil
}

// ClientKeyHashes returns a copy of the id -> hex(SHA-256) map.
func (s *Store) ClientKeyHashes() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.blob.ClientKeyHashes))
	for id, hash := range s.blob.ClientKeyHashes {
		out[id] = hash
	}
	return out
}

func normalizeClientKeyHash(hash string) (string, error) {
	hash = strings.ToLower(strings.TrimSpace(hash))
	if len(hash) != ClientKeyHashHexLen {
		return "", fmt.Errorf("secrets: client key hash must be %d hex chars, got %d", ClientKeyHashHexLen, len(hash))
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return "", fmt.Errorf("secrets: client key hash is not hex: %w", err)
	}
	return hash, nil
}

func mustRand(b []byte) {
	if _, err := rand.Read(b); err != nil {
		panic("secrets: crypto/rand failed: " + err.Error())
	}
}

func hexString(b []byte) string {
	return hex.EncodeToString(b)
}
