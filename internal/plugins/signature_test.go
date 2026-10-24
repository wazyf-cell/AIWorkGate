package plugins

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"aiworkgate/internal/config"
	"aiworkgate/internal/model"
)

// signedPackage writes an unsigned plugin source tree, signs it with a fresh
// key, and returns the directory, the public key (base64) and the private key.
func signedPackage(t *testing.T, id string) (string, string, ed25519.PrivateKey) {
	t.Helper()
	m := baseManifest()
	m.ID = id
	m.Startup.Mode = model.StartupOnDemand
	m.Entry.Exe = id + ".exe"
	dir := testTempDir(t)
	writeManifestFile(t, dir, m)
	writeTestFile(t, filepath.Join(dir, m.Entry.Exe), dummyExe)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	writeSignature(t, dir, priv, "test-key")
	return dir, base64.StdEncoding.EncodeToString(pub), priv
}

func writeSignature(t *testing.T, dir string, priv ed25519.PrivateKey, keyID string) {
	t.Helper()
	manifestRaw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m model.Manifest
	if err := json.Unmarshal(manifestRaw, &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	entryRaw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(m.Entry.Exe)))
	if err != nil {
		t.Fatalf("read entry: %v", err)
	}
	manifestSHA := sha256Hex(manifestRaw)
	entrySHA := sha256Hex(entryRaw)
	sig := packageSignature{
		Alg:            "ed25519",
		KeyID:          keyID,
		ManifestSHA256: manifestSHA,
		EntrySHA256:    entrySHA,
		Signature:      base64.StdEncoding.EncodeToString(ed25519.Sign(priv, signatureMessage(keyID, manifestSHA, entrySHA))),
	}
	raw, err := json.Marshal(sig)
	if err != nil {
		t.Fatalf("encode signature: %v", err)
	}
	writeTestFile(t, filepath.Join(dir, PackageSignatureFile), raw)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func trustedConfig(keyID, pub string) config.Config {
	c := testConfig()
	c.Plugins.TrustedKeys = map[string]string{keyID: pub}
	return c
}

func TestTrustedKeysRequireASignedPackage(t *testing.T) {
	mgr := newTestManagerConfig(t, trustedConfig("test-key", base64.StdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize))))

	unsigned := writePluginSource(t, "unsigned", model.StartupOnDemand, dummyExe)
	err := mgr.InstallDir(unsigned)
	if err == nil {
		t.Fatal("an unsigned package installed while plugins.trusted_keys is configured")
	}
	if _, ok := mgr.Get("unsigned"); ok {
		t.Fatal("rejected package was installed anyway")
	}
}

func TestSignedPackageInstalls(t *testing.T) {
	src, pub, _ := signedPackage(t, "signed")
	mgr := newTestManagerConfig(t, trustedConfig("test-key", pub))
	if err := mgr.InstallDir(src); err != nil {
		t.Fatalf("signed package rejected: %v", err)
	}
	if _, ok := mgr.Get("signed"); !ok {
		t.Fatal("signed package was not installed")
	}
}

func TestSignedPackageRejectsTampering(t *testing.T) {
	cases := map[string]func(t *testing.T, dir string){
		"manifest changed after signing": func(t *testing.T, dir string) {
			m := baseManifest()
			m.ID = "tampered"
			m.Name = "changed after signing"
			m.Startup.Mode = model.StartupOnDemand
			m.Entry.Exe = "tampered.exe"
			data, err := json.Marshal(m)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			writeTestFile(t, filepath.Join(dir, "manifest.json"), data)
		},
		"entry replaced after signing": func(t *testing.T, dir string) {
			writeTestFile(t, filepath.Join(dir, "tampered.exe"), []byte("different binary"))
		},
		"signature flipped": func(t *testing.T, dir string) {
			raw, err := os.ReadFile(filepath.Join(dir, PackageSignatureFile))
			if err != nil {
				t.Fatalf("read signature: %v", err)
			}
			var sig packageSignature
			if err := json.Unmarshal(raw, &sig); err != nil {
				t.Fatalf("parse signature: %v", err)
			}
			decoded, _ := base64.StdEncoding.DecodeString(sig.Signature)
			decoded[0] ^= 0xff
			sig.Signature = base64.StdEncoding.EncodeToString(decoded)
			data, _ := json.Marshal(sig)
			writeTestFile(t, filepath.Join(dir, PackageSignatureFile), data)
		},
		"unknown key id": func(t *testing.T, dir string) {
			writeSignature(t, dir, ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)), "other-key")
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			dir, pub, _ := signedPackage(t, "tampered")
			mutate(t, dir)
			mgr := newTestManagerConfig(t, trustedConfig("test-key", pub))
			if err := mgr.InstallDir(dir); err == nil {
				t.Fatal("tampered package installed")
			}
			if _, ok := mgr.Get("tampered"); ok {
				t.Fatal("tampered package reached the plugin list")
			}
		})
	}
}

func TestNoTrustedKeysAcceptsUnsignedPackages(t *testing.T) {
	src := writePluginSource(t, "plain", model.StartupOnDemand, dummyExe)
	mgr := newTestManager(t)
	if err := mgr.InstallDir(src); err != nil {
		t.Fatalf("unsigned install must keep working without trusted keys: %v", err)
	}
}
