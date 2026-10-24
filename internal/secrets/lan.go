package secrets

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	lanHashScheme = "argon2id"

	argonTime    = 2
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// SetLANPassword stores an Argon2id hash of pw. An empty password clears the
// stored hash (LAN mode is then disallowed by the caller).
func (s *Store) SetLANPassword(pw string) error {
	if pw == "" {
		s.mu.Lock()
		defer s.mu.Unlock()
		old := s.blob.LANPasswordHash
		if old == "" {
			return nil
		}
		s.blob.LANPasswordHash = ""
		if err := s.saveLocked(); err != nil {
			s.blob.LANPasswordHash = old
			return err
		}
		return nil
	}

	encoded, err := hashLANPassword(pw)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.blob.LANPasswordHash
	s.blob.LANPasswordHash = encoded
	if err := s.saveLocked(); err != nil {
		s.blob.LANPasswordHash = old
		return err
	}
	return nil
}

// VerifyLANPassword reports whether pw matches the stored Argon2id hash. An
// empty password or an unset hash never verifies.
func (s *Store) VerifyLANPassword(pw string) bool {
	if pw == "" {
		return false
	}
	s.mu.Lock()
	encoded := s.blob.LANPasswordHash
	s.mu.Unlock()
	if encoded == "" {
		return false
	}
	return verifyLANHash(encoded, pw)
}

// HasLANPassword reports whether a LAN password hash is stored.
func (s *Store) HasLANPassword() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blob.LANPasswordHash != ""
}

// hashLANPassword encodes as "argon2id$<b64 salt>$<b64 hash>".
func hashLANPassword(pw string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("secrets: generate LAN password salt: %w", err)
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	encoded := lanHashScheme + "$" +
		base64.StdEncoding.EncodeToString(salt) + "$" +
		base64.StdEncoding.EncodeToString(key)
	zero(key)
	return encoded, nil
}

func verifyLANHash(encoded, pw string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 3 || parts[0] != lanHashScheme {
		return false
	}
	salt, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil || len(salt) != argonSaltLen {
		return false
	}
	want, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil || len(want) != argonKeyLen {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	defer zero(got)
	return subtle.ConstantTimeCompare(got, want) == 1
}
