//go:build !windows

// Non-Windows secret sealing. Windows uses DPAPI (per-user, no key file); the
// Linux/macOS fallback uses AES-256-GCM with a 0600 key file beside
// secrets.dat. That protects against a copied data directory or a casual read,
// not against a compromised account/root — deploy it on a disk with OS
// encryption when the threat model includes offline disk access.

package secrets

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const keyFileName = "secrets.key"

var secretMagic = []byte("AWG1")

func protect(dir string, plain []byte) ([]byte, error) {
	if len(plain) == 0 {
		return nil, fmt.Errorf("secrets: refusing to encrypt an empty blob")
	}
	key, err := loadOrCreateKey(dir)
	if err != nil {
		return nil, err
	}
	defer zero(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secrets: aes: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secrets: nonce: %w", err)
	}
	out := append(append([]byte{}, secretMagic...), nonce...)
	return gcm.Seal(out, nonce, plain, nil), nil
}

func unprotect(dir string, sealed []byte) ([]byte, error) {
	if len(sealed) == 0 {
		return nil, fmt.Errorf("secrets: empty sealed blob")
	}
	if !bytes.HasPrefix(sealed, secretMagic) {
		return nil, errors.New("secrets: not an AIWorkGate secret blob (a Windows DPAPI store cannot be read here)")
	}
	key, err := loadOrCreateKey(dir)
	if err != nil {
		return nil, err
	}
	defer zero(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secrets: aes: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: gcm: %w", err)
	}
	body := sealed[len(secretMagic):]
	if len(body) < gcm.NonceSize() {
		return nil, errors.New("secrets: sealed blob is truncated")
	}
	nonce, ciphertext := body[:gcm.NonceSize()], body[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("secrets: decrypt: %w", err)
	}
	return plain, nil
}

func loadOrCreateKey(dir string) ([]byte, error) {
	if err := os.MkdirAll(dir, DirPerm); err != nil {
		return nil, fmt.Errorf("secrets: create %s: %w", dir, err)
	}
	path := filepath.Join(dir, keyFileName)
	if raw, err := os.ReadFile(path); err == nil {
		if len(raw) != 32 {
			return nil, fmt.Errorf("secrets: %s must hold a 32-byte key", path)
		}
		return raw, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("secrets: read %s: %w", path, err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("secrets: generate key: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, FilePerm)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return loadOrCreateKey(dir)
		}
		return nil, fmt.Errorf("secrets: create %s: %w", path, err)
	}
	defer file.Close()
	if _, err := file.Write(key); err != nil {
		return nil, fmt.Errorf("secrets: write %s: %w", path, err)
	}
	return key, nil
}
