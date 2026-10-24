// Command plugin-sign signs an AIWorkGate plugin package for
// plugins.trusted_keys, and generates the Ed25519 key it uses.
//
// Usage:
//
//	plugin-sign -gen   -key dev.key -key-id dev
//	plugin-sign        -key dev.key -key-id dev -dir plugins/hello
//
// The signature covers manifest.json and the entry binary, so editing either
// after signing makes the install fail once the host has the matching public
// key configured.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

const signatureFile = "manifest.sig"

type signature struct {
	Alg            string `json:"alg"`
	KeyID          string `json:"key_id"`
	ManifestSHA256 string `json:"manifest_sha256"`
	EntrySHA256    string `json:"entry_sha256"`
	Signature      string `json:"signature"`
}

func main() {
	var (
		gen   = flag.Bool("gen", false, "generate a new Ed25519 key at -key")
		key   = flag.String("key", "", "key file (base64 32-byte seed)")
		keyID = flag.String("key-id", "", "key id referenced by plugins.trusted_keys")
		dir   = flag.String("dir", ".", "plugin package directory")
		entry = flag.String("entry", "", "entry binary (default: manifest entry.exe)")
		force = flag.Bool("force", false, "overwrite an existing key file")
		quiet = flag.Bool("quiet", false, "print only the signature path")
	)
	flag.Parse()

	if *key == "" || *keyID == "" {
		fail("-key and -key-id are required")
	}

	if *gen {
		generate(*key, *keyID, *force)
		return
	}

	priv, err := loadKey(*key)
	if err != nil {
		fail("%v", err)
	}
	manifestPath := filepath.Join(*dir, "manifest.json")
	manifestRaw, err := os.ReadFile(manifestPath)
	if err != nil {
		fail("read manifest: %v", err)
	}
	var manifest struct {
		Entry struct {
			Exe string `json:"exe"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		fail("manifest.json is not valid JSON: %v", err)
	}
	entryRel := manifest.Entry.Exe
	if *entry != "" {
		entryRel = *entry
	}
	if entryRel == "" {
		fail("manifest has no entry.exe and -entry was not given")
	}
	entryRaw, err := os.ReadFile(filepath.Join(*dir, filepath.FromSlash(entryRel)))
	if err != nil {
		fail("read entry: %v", err)
	}

	manifestSHA := digest(manifestRaw)
	entrySHA := digest(entryRaw)
	message := []byte(*keyID + "\n" + manifestSHA + "\n" + entrySHA)
	sig := signature{
		Alg:            "ed25519",
		KeyID:          *keyID,
		ManifestSHA256: manifestSHA,
		EntrySHA256:    entrySHA,
		Signature:      base64.StdEncoding.EncodeToString(ed25519.Sign(priv, message)),
	}
	data, err := json.MarshalIndent(sig, "", "  ")
	if err != nil {
		fail("encode signature: %v", err)
	}
	out := filepath.Join(*dir, signatureFile)
	if err := os.WriteFile(out, append(data, '\n'), 0o644); err != nil {
		fail("write %s: %v", out, err)
	}
	if !*quiet {
		fmt.Printf("signed %s (key %s)\n  manifest %s\n  entry    %s\n", *dir, *keyID, manifestSHA, entrySHA)
	}
	fmt.Println(out)
}

func generate(path, keyID string, force bool) {
	if !force {
		if _, err := os.Stat(path); err == nil {
			fail("%s already exists; pass -force to overwrite", path)
		}
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		fail("generate seed: %v", err)
	}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(seed)), 0o600); err != nil {
		fail("write key: %v", err)
	}
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	fmt.Printf("generated %s\n", path)
	fmt.Printf("add this to config.json -> plugins.trusted_keys:\n  %q: %q\n",
		keyID, base64.StdEncoding.EncodeToString(pub))
}

func loadKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read key: %w", err)
	}
	seed, err := base64.StdEncoding.DecodeString(string(trimSpace(raw)))
	if err != nil {
		return nil, fmt.Errorf("key is not base64: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("key must decode to %d bytes, got %d", ed25519.SeedSize, len(seed))
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func trimSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && (b[start] == ' ' || b[start] == '\n' || b[start] == '\r' || b[start] == '\t') {
		start++
	}
	for end > start && (b[end-1] == ' ' || b[end-1] == '\n' || b[end-1] == '\r' || b[end-1] == '\t') {
		end--
	}
	return b[start:end]
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "plugin-sign: "+format+"\n", args...)
	os.Exit(1)
}
