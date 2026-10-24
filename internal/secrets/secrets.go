// Package secrets stores AIWorkGate's sensitive material in
// %APPDATA%\AIWorkGate\secrets.dat using DPAPI user-scope encryption.
//
// Only ciphertext is ever written to disk: the plaintext JSON blob exists in
// memory and, briefly, in a temporary file that is atomically renamed into
// place. Client keys are compared as SHA-256 hashes; the LAN password is kept
// as an Argon2id hash; the A key, plugin tokens and proxy passwords are only
// recoverable through CryptUnprotectData for the current Windows user.
package secrets

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"aiworkgate/internal/fsutil"
)

const (
	// FileName is the on-disk name of the encrypted secret store.
	FileName = "secrets.dat"
	// FilePerm and DirPerm are applied to the store and its directory.
	FilePerm os.FileMode = 0o600
	DirPerm  os.FileMode = 0o700
)

// Blob is the plaintext document that gets DPAPI-sealed in secrets.dat.
type Blob struct {
	AAPIKey         string            `json:"a_api_key,omitempty"`
	ClientKeyHashes map[string]string `json:"client_key_hashes,omitempty"`
	PluginTokens    map[string]string `json:"plugin_tokens,omitempty"`
	ProxyPasswords  map[string]string `json:"proxy_passwords,omitempty"`
	LANPasswordHash string            `json:"lan_password_hash,omitempty"`
	Extra           map[string]string `json:"extra,omitempty"`
}

// Store is the thread-safe handle to secrets.dat.
type Store struct {
	mu   sync.Mutex
	path string
	blob Blob
}

// Open loads (or initializes) dir\secrets.dat. A missing file starts an empty
// store; a file that cannot be decrypted for this user is an error.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("secrets: empty data directory")
	}
	if err := os.MkdirAll(dir, DirPerm); err != nil {
		return nil, fmt.Errorf("secrets: create %s: %w", dir, err)
	}
	s := &Store{path: filepath.Join(dir, FileName)}

	sealed, err := os.ReadFile(s.path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("secrets: read %s: %w", s.path, err)
		}
		s.initMaps()
		return s, nil
	}

	plain, err := unprotect(sealed)
	if err != nil {
		return nil, fmt.Errorf("secrets: decrypt %s: %w", s.path, err)
	}
	defer zero(plain)
	if len(plain) > 0 {
		if err := json.Unmarshal(plain, &s.blob); err != nil {
			return nil, fmt.Errorf("secrets: parse %s: %w", s.path, err)
		}
	}
	s.initMaps()
	return s, nil
}

// Close drops the in-memory plaintext. It never writes; call Save first if
// there are unsaved changes.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blob = Blob{}
	s.initMaps()
	return nil
}

// Save seals the current blob and atomically replaces secrets.dat.
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *Store) initMaps() {
	if s.blob.ClientKeyHashes == nil {
		s.blob.ClientKeyHashes = make(map[string]string)
	}
	if s.blob.PluginTokens == nil {
		s.blob.PluginTokens = make(map[string]string)
	}
	if s.blob.ProxyPasswords == nil {
		s.blob.ProxyPasswords = make(map[string]string)
	}
	if s.blob.Extra == nil {
		s.blob.Extra = make(map[string]string)
	}
}

func (s *Store) saveLocked() error {
	s.initMaps()
	plain, err := json.Marshal(s.blob)
	if err != nil {
		return fmt.Errorf("secrets: marshal: %w", err)
	}
	defer zero(plain)
	sealed, err := protect(plain)
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, sealed, FilePerm)
}

// SetAAPIKey stores the AI Work 助手 gateway key. An empty key clears it.
func (s *Store) SetAAPIKey(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.blob.AAPIKey
	if old == key {
		return nil
	}
	s.blob.AAPIKey = key
	if err := s.saveLocked(); err != nil {
		s.blob.AAPIKey = old
		return err
	}
	return nil
}

// GetAAPIKey returns the stored A gateway key and whether one is present.
func (s *Store) GetAAPIKey() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blob.AAPIKey, s.blob.AAPIKey != ""
}

// EnsurePluginToken returns the per-plugin bearer token, creating and
// persisting a new 32-byte random token on first use.
func (s *Store) EnsurePluginToken(pluginID string) (string, error) {
	if pluginID == "" {
		return "", errors.New("secrets: empty plugin id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initMaps()
	if tok := s.blob.PluginTokens[pluginID]; tok != "" {
		return tok, nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("secrets: generate plugin token: %w", err)
	}
	token := hexString(buf)
	zero(buf)
	s.blob.PluginTokens[pluginID] = token
	if err := s.saveLocked(); err != nil {
		delete(s.blob.PluginTokens, pluginID)
		return "", err
	}
	return token, nil
}

// PluginToken returns the existing token for pluginID, if any.
func (s *Store) PluginToken(pluginID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tok, ok := s.blob.PluginTokens[pluginID]
	return tok, ok && tok != ""
}

// RemovePluginToken forgets the token of an uninstalled plugin. Removing an
// absent token is not an error.
func (s *Store) RemovePluginToken(pluginID string) error {
	if pluginID == "" {
		return errors.New("secrets: empty plugin id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.blob.PluginTokens == nil {
		return nil
	}
	old, had := s.blob.PluginTokens[pluginID]
	if !had {
		return nil
	}
	delete(s.blob.PluginTokens, pluginID)
	if err := s.saveLocked(); err != nil {
		s.blob.PluginTokens[pluginID] = old
		return err
	}
	return nil
}

// SetProxyPassword stores a proxy password; an empty password removes it.
func (s *Store) SetProxyPassword(proxyID, password string) error {
	if proxyID == "" {
		return errors.New("secrets: empty proxy id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initMaps()
	if password == "" {
		return s.removeProxyPasswordLocked(proxyID)
	}
	old, had := s.blob.ProxyPasswords[proxyID]
	s.blob.ProxyPasswords[proxyID] = password
	if err := s.saveLocked(); err != nil {
		if had {
			s.blob.ProxyPasswords[proxyID] = old
		} else {
			delete(s.blob.ProxyPasswords, proxyID)
		}
		return err
	}
	return nil
}

// GetProxyPassword returns the stored password for proxyID.
func (s *Store) GetProxyPassword(proxyID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pw, ok := s.blob.ProxyPasswords[proxyID]
	return pw, ok && pw != ""
}

// RemoveProxyPassword deletes the stored password for proxyID.
func (s *Store) RemoveProxyPassword(proxyID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.removeProxyPasswordLocked(proxyID)
}

func (s *Store) removeProxyPasswordLocked(proxyID string) error {
	old, had := s.blob.ProxyPasswords[proxyID]
	if !had {
		return nil
	}
	delete(s.blob.ProxyPasswords, proxyID)
	if err := s.saveLocked(); err != nil {
		s.blob.ProxyPasswords[proxyID] = old
		return err
	}
	return nil
}

// SetExtra stores a free-form secret value; an empty value removes the key.
func (s *Store) SetExtra(key, value string) error {
	if key == "" {
		return errors.New("secrets: empty extra key")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initMaps()
	if value == "" {
		if _, had := s.blob.Extra[key]; !had {
			return nil
		}
		delete(s.blob.Extra, key)
	} else {
		s.blob.Extra[key] = value
	}
	return s.saveLocked()
}

// GetExtra returns a free-form secret value previously stored with SetExtra.
func (s *Store) GetExtra(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.blob.Extra[key]
	return v, ok && v != ""
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := fsutil.WriteFileAtomic(path, data, perm); err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	return nil
}
