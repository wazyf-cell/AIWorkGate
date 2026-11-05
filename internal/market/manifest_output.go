package market

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"aiworkgate/internal/model"
	"aiworkgate/internal/opchost"
)

// manifestSeed is the context needed to decide package-manifest vs generated
// bridge manifest.
type manifestSeed struct {
	Spec     Spec
	PluginID string
	Version  string
	Opts     Options
}

// ensureManifest returns the manifest the prepared tree will carry and whether
// it was generated (vs shipped by the package). A shipped manifest is validated
// minimally; a generated one goes through opchost.GenerateBridgeManifest.
func (in *Installer) ensureManifest(payloadRoot string, seed manifestSeed) (model.Manifest, bool, error) {
	path := filepath.Join(payloadRoot, "manifest.json")
	if isRegularFile(path) {
		mf, err := loadManifestFile(path)
		if err != nil {
			return model.Manifest{}, false, err
		}
		if err := validateShippedManifest(mf); err != nil {
			return model.Manifest{}, false, err
		}
		return mf, false, nil
	}
	if strings.TrimSpace(seed.Opts.FinalPluginDir) == "" {
		return model.Manifest{}, false, errors.New("market: package ships no manifest.json; final plugin dir is required to generate the bridge manifest")
	}
	if strings.TrimSpace(seed.Opts.BridgeExecutable) == "" {
		return model.Manifest{}, false, errors.New("market: package ships no manifest.json; the bridge executable is required to generate the bridge manifest")
	}
	mf, err := opchost.GenerateBridgeManifest(opchost.Options{
		ID:             seed.PluginID,
		Name:           seed.Spec.Name,
		Version:        seed.Version,
		NetworkDomains: seed.Opts.NetworkDomains,
		StartupMode:    seed.Opts.StartupMode,
		IdleTimeoutS:   seed.Opts.IdleTimeoutS,
		PluginDir:      seed.Opts.FinalPluginDir,
	})
	if err != nil {
		return model.Manifest{}, false, err
	}
	return mf, true, nil
}

// validateShippedManifest applies the host's load gates to a package-shipped
// manifest without importing internal/plugins: id syntax, api_level range and a
// non-empty safe entry.exe.
func validateShippedManifest(mf model.Manifest) error {
	if !pluginIDPattern.MatchString(mf.ID) {
		return fmt.Errorf("market: package manifest: id %q must match %s", mf.ID, pluginIDPattern.String())
	}
	if mf.APILevel < 1 || mf.APILevel > 2 {
		return fmt.Errorf("market: package manifest %q: api_level %d is not supported (need 1-2)", mf.ID, mf.APILevel)
	}
	if strings.TrimSpace(mf.Entry.Exe) == "" {
		return fmt.Errorf("market: package manifest %q: entry.exe is required", mf.ID)
	}
	return nil
}

// deployBridgeExecutable copies the shared bridge binary into the prepared root
// under the generated manifest's entry.exe name, the same existence the host
// checks after staging (internal/plugins/install.go:170-173).
func deployBridgeExecutable(opts Options, payloadRoot string, mf model.Manifest) error {
	src := strings.TrimSpace(opts.BridgeExecutable)
	if src == "" {
		return errors.New("market: bridge executable path is required to prepare a bridge plugin")
	}
	if !isRegularFile(src) {
		return fmt.Errorf("market: bridge executable %q is missing", src)
	}
	entry := filepath.Join(payloadRoot, filepath.FromSlash(mf.Entry.Exe))
	if err := os.MkdirAll(filepath.Dir(entry), 0o755); err != nil {
		return fmt.Errorf("market: deploy bridge: %w", err)
	}
	if err := copyFile(src, entry); err != nil {
		return err
	}
	if !isRegularFile(entry) {
		return fmt.Errorf("market: bridge executable %q was not deployed", entry)
	}
	return nil
}

func writeManifestFile(payloadRoot string, mf model.Manifest) error {
	data, err := json.MarshalIndent(mf, "", "  ")
	if err != nil {
		return fmt.Errorf("market: encode manifest: %w", err)
	}
	data = append(data, '\n')
	return os.WriteFile(filepath.Join(payloadRoot, "manifest.json"), data, 0o644)
}

// findPayloadRoot is the tar analogue of the host's findManifestRoot
// (internal/plugins/manifest.go:296-321), relaxed to also accept a root without
// a manifest yet: the base, or the single top-level directory when no manifest
// is at the base.
func findPayloadRoot(base string) (string, error) {
	if isRegularFile(filepath.Join(base, "manifest.json")) {
		return base, nil
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return "", fmt.Errorf("market: read %q: %w", base, err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(base, e.Name()))
		}
	}
	if len(dirs) == 1 && isRegularFile(filepath.Join(dirs[0], "manifest.json")) {
		return dirs[0], nil
	}
	if len(dirs) == 1 {
		return dirs[0], nil
	}
	return base, nil
}

func isRegularFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

func loadManifestFile(p string) (model.Manifest, error) {
	var mf model.Manifest
	data, err := os.ReadFile(p)
	if err != nil {
		return mf, fmt.Errorf("market: read manifest %q: %w", p, err)
	}
	if len(data) > maxManifestBytes {
		return mf, fmt.Errorf("market: manifest %q exceeds %d bytes", p, maxManifestBytes)
	}
	if err := json.Unmarshal(data, &mf); err != nil {
		return mf, fmt.Errorf("market: parse manifest %q: %w", p, err)
	}
	return mf, nil
}
