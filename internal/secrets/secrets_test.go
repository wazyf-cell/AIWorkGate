package secrets

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

var (
	clientKeyPattern = regexp.MustCompile(`^agk_[A-Za-z0-9_-]{32}$`)
	clientKeyIDRegex = regexp.MustCompile(`^ck_[0-9a-f]{8}$`)
	hexRegex         = regexp.MustCompile(`^[0-9a-f]+$`)
)

func openStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}

func TestOpenEmptyStore(t *testing.T) {
	s, dir := openStore(t)
	if _, ok := s.GetAAPIKey(); ok {
		t.Fatal("GetAAPIKey() on empty store = present")
	}
	if s.HasLANPassword() {
		t.Fatal("HasLANPassword() on empty store = true")
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); !os.IsNotExist(err) {
		t.Fatalf("Open() wrote %s before any change (err = %v)", FileName, err)
	}
}

func TestAAPIKeyDPAPIRoundtrip(t *testing.T) {
	s, dir := openStore(t)
	const key = "sk-a-gateway-key-0123456789"

	if err := s.SetAAPIKey(key); err != nil {
		t.Fatalf("SetAAPIKey() error = %v", err)
	}
	got, ok := s.GetAAPIKey()
	if !ok || got != key {
		t.Fatalf("GetAAPIKey() = %q, %v; want %q, true", got, ok, key)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	defer reopened.Close()
	got, ok = reopened.GetAAPIKey()
	if !ok || got != key {
		t.Fatalf("after reopen GetAAPIKey() = %q, %v; want %q, true", got, ok, key)
	}

	if err := reopened.SetAAPIKey(""); err != nil {
		t.Fatalf("SetAAPIKey(\"\") error = %v", err)
	}
	if _, ok := reopened.GetAAPIKey(); ok {
		t.Fatal("GetAAPIKey() after clear = present")
	}
}

func TestDPAPIBlobIsNotPlaintext(t *testing.T) {
	s, dir := openStore(t)
	if err := s.SetAAPIKey("sk-sealed-check"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if json.Valid(raw) {
		t.Fatalf("%s is plain JSON, expected a DPAPI blob", FileName)
	}
	if bytes.Contains(raw, []byte("a_api_key")) || bytes.Contains(raw, []byte("sk-sealed-check")) {
		t.Fatal("secrets.dat contains plaintext JSON markers or the raw key")
	}
}

func TestClientKeyFormat(t *testing.T) {
	assertKey := func(t *testing.T, plain, id, prefix, hash string) {
		t.Helper()
		if !clientKeyPattern.MatchString(plain) {
			t.Fatalf("plain key %q does not match agk_ + 32 URL-safe chars", plain)
		}
		if !clientKeyIDRegex.MatchString(id) {
			t.Fatalf("id %q does not match ck_ + 8 hex chars", id)
		}
		if prefix != plain[:8] || len(prefix) != ClientKeyPrefixLen {
			t.Fatalf("prefix %q is not the first %d chars of %q", prefix, ClientKeyPrefixLen, plain)
		}
		if len(hash) != ClientKeyHashHexLen || !hexRegex.MatchString(hash) {
			t.Fatalf("hash %q is not %d lowercase hex chars", hash, ClientKeyHashHexLen)
		}
		if hash != HashClientKey(plain) {
			t.Fatal("hash does not equal HashClientKey(plain)")
		}
	}

	t.Run("single", func(t *testing.T) {
		plain, id, prefix, hash := NewClientKey()
		assertKey(t, plain, id, prefix, hash)
	})
	t.Run("unique", func(t *testing.T) {
		seen := make(map[string]bool, 50)
		for i := 0; i < 50; i++ {
			plain, id, _, _ := NewClientKey()
			assertKey(t, plain, id, plain[:8], HashClientKey(plain))
			if seen[id] {
				t.Fatalf("duplicate client key id %q", id)
			}
			seen[id] = true
		}
	})
}

func TestClientKeyAddVerifyRemove(t *testing.T) {
	s, dir := openStore(t)

	plain1, id1, _, hash1 := NewClientKey()
	plain2, id2, _, hash2 := NewClientKey()
	if err := s.AddClientKey(id1, hash1); err != nil {
		t.Fatalf("AddClientKey() error = %v", err)
	}
	// Hashes are accepted case-insensitively and stored lowercase.
	if err := s.AddClientKey(id2, strings.ToUpper(hash2)); err != nil {
		t.Fatalf("AddClientKey(upper) error = %v", err)
	}

	t.Run("verify each key maps to its id", func(t *testing.T) {
		if got, ok := s.VerifyClientKey(plain1); !ok || got != id1 {
			t.Fatalf("VerifyClientKey(key1) = %q, %v; want %q, true", got, ok, id1)
		}
		if got, ok := s.VerifyClientKey(plain2); !ok || got != id2 {
			t.Fatalf("VerifyClientKey(key2) = %q, %v; want %q, true", got, ok, id2)
		}
	})

	t.Run("rejects unknown and empty keys", func(t *testing.T) {
		if id, ok := s.VerifyClientKey("agk_not-a-real-key"); ok {
			t.Fatalf("VerifyClientKey(unknown) = %q, true", id)
		}
		if id, ok := s.VerifyClientKey(""); ok {
			t.Fatalf("VerifyClientKey(\"\") = %q, true", id)
		}
	})

	t.Run("hashes survive reopen", func(t *testing.T) {
		reopened, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		if got, ok := reopened.VerifyClientKey(plain1); !ok || got != id1 {
			t.Fatalf("after reopen VerifyClientKey(key1) = %q, %v", got, ok)
		}
	})

	t.Run("hashes accessor returns a copy", func(t *testing.T) {
		hashes := s.ClientKeyHashes()
		if len(hashes) != 2 || hashes[id1] != hash1 {
			t.Fatalf("ClientKeyHashes() = %v", hashes)
		}
		hashes[id1] = "tampered"
		delete(hashes, id2)
		after := s.ClientKeyHashes()
		if after[id1] != hash1 || len(after) != 2 {
			t.Fatalf("ClientKeyHashes() aliases internal state: %v", after)
		}
	})

	t.Run("rejects malformed hashes", func(t *testing.T) {
		cases := []struct{ name, id, hash string }{
			{name: "empty id", id: "", hash: hash1},
			{name: "short hash", id: "ck_x", hash: "abc"},
			{name: "non-hex hash", id: "ck_x", hash: strings.Repeat("z", ClientKeyHashHexLen)},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if err := s.AddClientKey(tc.id, tc.hash); err == nil {
					t.Fatal("AddClientKey() = nil, want error")
				}
			})
		}
	})

	t.Run("remove", func(t *testing.T) {
		if err := s.RemoveClientKey(id1); err != nil {
			t.Fatalf("RemoveClientKey() error = %v", err)
		}
		if _, ok := s.VerifyClientKey(plain1); ok {
			t.Fatal("removed key still verifies")
		}
		if _, ok := s.VerifyClientKey(plain2); !ok {
			t.Fatal("removing key1 broke key2")
		}
		if err := s.RemoveClientKey("ck_unknown"); err != nil {
			t.Fatalf("RemoveClientKey(unknown) error = %v", err)
		}
	})
}

func TestPluginTokenStable(t *testing.T) {
	s, dir := openStore(t)

	tok, err := s.EnsurePluginToken("mock")
	if err != nil {
		t.Fatalf("EnsurePluginToken() error = %v", err)
	}
	if len(tok) != 64 || !hexRegex.MatchString(tok) {
		t.Fatalf("token %q is not 64 lowercase hex chars", tok)
	}
	again, err := s.EnsurePluginToken("mock")
	if err != nil {
		t.Fatal(err)
	}
	if again != tok {
		t.Fatalf("EnsurePluginToken() is not stable: %q then %q", tok, again)
	}
	other, err := s.EnsurePluginToken("other")
	if err != nil {
		t.Fatal(err)
	}
	if other == tok {
		t.Fatal("distinct plugin ids received the same token")
	}
	if got, ok := s.PluginToken("mock"); !ok || got != tok {
		t.Fatalf("PluginToken(mock) = %q, %v", got, ok)
	}
	if _, ok := s.PluginToken("missing"); ok {
		t.Fatal("PluginToken(missing) = present")
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got, ok := reopened.PluginToken("mock"); !ok || got != tok {
		t.Fatalf("token did not persist: %q, %v", got, ok)
	}

	if _, err := s.EnsurePluginToken(""); err == nil {
		t.Fatal("EnsurePluginToken(\"\") = nil error")
	}
}

func TestProxyPassword(t *testing.T) {
	s, dir := openStore(t)

	if err := s.SetProxyPassword("px_1", "pass-one"); err != nil {
		t.Fatalf("SetProxyPassword() error = %v", err)
	}
	if got, ok := s.GetProxyPassword("px_1"); !ok || got != "pass-one" {
		t.Fatalf("GetProxyPassword() = %q, %v", got, ok)
	}
	if err := s.SetProxyPassword("px_1", "pass-two"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetProxyPassword("px_1"); got != "pass-two" {
		t.Fatalf("GetProxyPassword() after overwrite = %q", got)
	}
	if err := s.SetProxyPassword("px_2", "keep-me"); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got, ok := reopened.GetProxyPassword("px_2"); !ok || got != "keep-me" {
		t.Fatalf("proxy password did not persist: %q, %v", got, ok)
	}

	if err := s.RemoveProxyPassword("px_2"); err != nil {
		t.Fatalf("RemoveProxyPassword() error = %v", err)
	}
	if _, ok := s.GetProxyPassword("px_2"); ok {
		t.Fatal("removed proxy password still present")
	}
	// Empty password also removes.
	if err := s.SetProxyPassword("px_1", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.GetProxyPassword("px_1"); ok {
		t.Fatal("empty password did not clear the entry")
	}
	if err := s.RemoveProxyPassword("px_missing"); err != nil {
		t.Fatalf("RemoveProxyPassword(missing) error = %v", err)
	}
	if err := s.SetProxyPassword("", "x"); err == nil {
		t.Fatal("SetProxyPassword(\"\") = nil error")
	}
}

func TestLANPassword(t *testing.T) {
	s, dir := openStore(t)

	if s.VerifyLANPassword("anything") {
		t.Fatal("VerifyLANPassword() on empty store = true")
	}
	if err := s.SetLANPassword("correct horse battery staple"); err != nil {
		t.Fatalf("SetLANPassword() error = %v", err)
	}
	if !s.HasLANPassword() {
		t.Fatal("HasLANPassword() = false after set")
	}

	tests := []struct {
		name string
		pw   string
		want bool
	}{
		{name: "right", pw: "correct horse battery staple", want: true},
		{name: "wrong", pw: "correct horse battery stapl3", want: false},
		{name: "empty", pw: "", want: false},
		{name: "case differs", pw: "Correct horse battery staple", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := s.VerifyLANPassword(tt.pw); got != tt.want {
				t.Fatalf("VerifyLANPassword(%q) = %v, want %v", tt.pw, got, tt.want)
			}
		})
	}

	encoded := s.blob.LANPasswordHash
	parts := strings.Split(encoded, "$")
	if len(parts) != 3 || parts[0] != "argon2id" {
		t.Fatalf("LAN hash %q is not argon2id$salt$hash", encoded)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if !reopened.VerifyLANPassword("correct horse battery staple") {
		t.Fatal("LAN password did not persist")
	}

	if err := reopened.SetLANPassword(""); err != nil {
		t.Fatalf("SetLANPassword(\"\") error = %v", err)
	}
	if reopened.HasLANPassword() {
		t.Fatal("empty password did not clear the hash")
	}
	if reopened.VerifyLANPassword("") {
		t.Fatal("VerifyLANPassword(\"\") = true after clear")
	}
	if err := reopened.SetLANPassword(""); err != nil {
		t.Fatalf("second clear error = %v", err)
	}
}

func TestExtra(t *testing.T) {
	s, dir := openStore(t)
	if _, ok := s.GetExtra("k"); ok {
		t.Fatal("GetExtra() on empty store = present")
	}
	if err := s.SetExtra("k", "v"); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.GetExtra("k"); !ok || got != "v" {
		t.Fatalf("GetExtra() = %q, %v", got, ok)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got, ok := reopened.GetExtra("k"); !ok || got != "v" {
		t.Fatalf("extra did not persist: %q, %v", got, ok)
	}
	if err := s.SetExtra("k", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.GetExtra("k"); ok {
		t.Fatal("empty value did not remove the extra key")
	}
	if err := s.SetExtra("", "v"); err == nil {
		t.Fatal("SetExtra(\"\") = nil error")
	}
}

func TestNoPlaintextSecretsOnDisk(t *testing.T) {
	s, dir := openStore(t)

	const (
		aKey    = "sk-live-AKEY-0123456789abcdef-DO-NOT-LEAK"
		proxyPW = "proxy-pass-9f8e7d6c5b4a-DO-NOT-LEAK"
		lanPW   = "lan-pass-ZYXWVU-1234567890-DO-NOT-LEAK"
	)
	clientPlain, clientID, _, clientHash := NewClientKey()

	if err := s.SetAAPIKey(aKey); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProxyPassword("px_1", proxyPW); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLANPassword(lanPW); err != nil {
		t.Fatal(err)
	}
	if err := s.AddClientKey(clientID, clientHash); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsurePluginToken("mock"); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatal("secrets.dat is empty")
	}
	for _, secret := range []struct{ name, value string }{
		{"A API key", aKey},
		{"proxy password", proxyPW},
		{"LAN password", lanPW},
		{"client key", clientPlain},
	} {
		if bytes.Contains(raw, []byte(secret.value)) {
			t.Fatalf("secrets.dat contains the raw %s", secret.name)
		}
		// Also try the secret without the synthetic DO-NOT-LEAK suffix.
		core := strings.Split(secret.value, "-DO-NOT-LEAK")[0]
		if bytes.Contains(raw, []byte(core)) {
			t.Fatalf("secrets.dat contains part of the raw %s (%q)", secret.name, core)
		}
	}
	for _, marker := range []string{`"a_api_key"`, `"proxy_passwords"`, `"lan_password_hash"`, "agk_"} {
		if bytes.Contains(raw, []byte(marker)) {
			t.Fatalf("secrets.dat contains plaintext marker %q", marker)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != FileName {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected only %s, found %v", FileName, names)
	}
}

func TestOpenCorruptFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("not a dpapi blob"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("Open() on corrupt file = nil error")
	}
}

func TestCloseDropsPlaintext(t *testing.T) {
	s, _ := openStore(t)
	if err := s.SetAAPIKey("sk-close-me"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, ok := s.GetAAPIKey(); ok {
		t.Fatal("GetAAPIKey() after Close() = present")
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save() after Close() error = %v", err)
	}
	if _, err := s.EnsurePluginToken("mock"); err != nil {
		t.Fatalf("EnsurePluginToken() after Close() error = %v", err)
	}
}

func TestConcurrentAccess(t *testing.T) {
	s, dir := openStore(t)

	const workers = 8
	var wg sync.WaitGroup
	tokens := make([]string, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tok, err := s.EnsurePluginToken("shared")
			if err != nil {
				errs[i] = err
				return
			}
			tokens[i] = tok
			if err := s.SetProxyPassword("px_1", "pw"); err != nil {
				errs[i] = err
			}
			_, _ = s.GetProxyPassword("px_1")
			_ = s.ClientKeyHashes()
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d error = %v", i, err)
		}
	}
	for i := 1; i < workers; i++ {
		if tokens[i] != tokens[0] {
			t.Fatalf("EnsurePluginToken raced: %q != %q", tokens[i], tokens[0])
		}
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen after concurrent writes: %v", err)
	}
	defer reopened.Close()
	if got, ok := reopened.PluginToken("shared"); !ok || got != tokens[0] {
		t.Fatalf("token on disk = %q, %v; want %q", got, ok, tokens[0])
	}
}

func TestOpenEmptyDirPath(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("Open(\"\") = nil error")
	}
}

func TestRemovePluginToken(t *testing.T) {
	s, _ := openStore(t)
	if _, err := s.EnsurePluginToken("mock"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemovePluginToken("mock"); err != nil {
		t.Fatalf("RemovePluginToken() error = %v", err)
	}
	if _, ok := s.PluginToken("mock"); ok {
		t.Fatal("token still present after removal")
	}
	// Removing an absent token is a no-op.
	if err := s.RemovePluginToken("mock"); err != nil {
		t.Fatalf("second RemovePluginToken() error = %v", err)
	}
	if _, err := s.EnsurePluginToken("mock"); err != nil {
		t.Fatal(err)
	}
}
