package plugins

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"aiworkgate/internal/model"
)

// PackageSignatureFile is the optional sidecar that authenticates a plugin
// package. It sits next to manifest.json and signs both the manifest bytes and
// the entry binary, so swapping either one invalidates it.
const PackageSignatureFile = "manifest.sig"

// packageSignature is the on-disk shape written by tools/plugin-sign.
type packageSignature struct {
	Alg            string `json:"alg"`
	KeyID          string `json:"key_id"`
	ManifestSHA256 string `json:"manifest_sha256"`
	EntrySHA256    string `json:"entry_sha256"`
	Signature      string `json:"signature"`
}

// signatureMessage is the exact byte string an Ed25519 key signs.
func signatureMessage(keyID, manifestSHA, entrySHA string) []byte {
	return []byte(keyID + "\n" + manifestSHA + "\n" + entrySHA)
}

// verifyPackageSignature enforces plugins.trusted_keys for one package rooted
// at dir. With no trusted keys configured it is a no-op: unsigned installs must
// keep working for the plugins that ship with the repository.
func verifyPackageSignature(dir string, manifest model.Manifest, trusted map[string]string) error {
	if len(trusted) == 0 {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(dir, PackageSignatureFile))
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("plugins: package is unsigned and plugins.trusted_keys is configured")
		}
		return fmt.Errorf("plugins: read %s: %w", PackageSignatureFile, err)
	}
	var sig packageSignature
	if err := json.Unmarshal(raw, &sig); err != nil {
		return fmt.Errorf("plugins: %s is not valid JSON: %w", PackageSignatureFile, err)
	}
	if sig.Alg != "ed25519" {
		return fmt.Errorf("plugins: unsupported signature algorithm %q", sig.Alg)
	}
	keyText, ok := trusted[sig.KeyID]
	if !ok {
		return fmt.Errorf("plugins: package is signed by untrusted key %q", sig.KeyID)
	}
	pub, err := base64.StdEncoding.DecodeString(keyText)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("plugins: trusted key %q is not an Ed25519 public key", sig.KeyID)
	}
	manifestRaw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return fmt.Errorf("plugins: read manifest.json: %w", err)
	}
	if err := compareDigest(manifestRaw, sig.ManifestSHA256); err != nil {
		return fmt.Errorf("plugins: manifest.json: %w", err)
	}
	entryRaw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(manifest.Entry.Exe)))
	if err != nil {
		return fmt.Errorf("plugins: read entry %q: %w", manifest.Entry.Exe, err)
	}
	if err := compareDigest(entryRaw, sig.EntrySHA256); err != nil {
		return fmt.Errorf("plugins: entry %q: %w", manifest.Entry.Exe, err)
	}
	sigBytes, err := base64.StdEncoding.DecodeString(sig.Signature)
	if err != nil {
		return fmt.Errorf("plugins: signature is not base64: %w", err)
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), signatureMessage(sig.KeyID, sig.ManifestSHA256, sig.EntrySHA256), sigBytes) {
		return fmt.Errorf("plugins: signature verification failed for key %q", sig.KeyID)
	}
	return nil
}

func compareDigest(content []byte, want string) error {
	sum := sha256.Sum256(content)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Errorf("sha256 %s does not match the signed %s", got, want)
	}
	return nil
}
