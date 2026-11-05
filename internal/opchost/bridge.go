package opchost

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"aiworkgate/internal/model"
)

const (
	// DefaultBridgeExeName is the in-directory name of the shared bridge binary.
	// It is what ResolveBridgeEntry falls back to when no bridge path is
	// configured, i.e. "look in the plugin directory".
	DefaultBridgeExeName = "magpie-bridge.exe"

	// argPluginDir is the only entry.args element the manifest carries: the
	// directory the bridge loads its .mjs plugin from. Port, token and data
	// directory are injected by the supervisor as AIWORKGATE_PLUGIN_* variables
	// (see the package doc), never baked into the manifest.
	argPluginDir = "--plugin-dir"
)

// bridgePluginIDPattern mirrors pluginIDPattern in
// internal/plugins/manifest.go: a bridge is an ordinary plugin, so its id obeys
// the same regexp. It is duplicated here on purpose: this package must not
// import internal/plugins (see the package doc), and the contract is stable.
var bridgePluginIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,31}$`)

// DefaultBridgeCapabilities are the capabilities a bridge declares when the
// caller names none. accounts + checkin are the two endpoints the P1 scope
// promises (D-19 check-in delegation); extend the list explicitly once the
// bridge also serves refresh/quota/login/identity.
var DefaultBridgeCapabilities = []string{model.CapAccounts, model.CapCheckin}

// Options describes one bridge plugin manifest.
//
// PluginDir is required and must contain the bridge executable (see the package
// doc for the shared-exe deployment). BridgeExe is optional and defaults to
// DefaultBridgeExeName inside PluginDir.
type Options struct {
	// ID is the plugin id (must match ^[a-z0-9][a-z0-9-]{1,31}$).
	ID string
	// Name, Version and Description are display-only, like every manifest.
	Name        string
	Version     string
	Description string
	// APILevel defaults to model.MinAPILevel when zero.
	APILevel int
	// Capabilities defaults to DefaultBridgeCapabilities when nil.
	Capabilities []string
	// NetworkDomains is the manifest permissions.network_domains list, usually
	// exported by the bridge's /admin/manifest SITES table.
	NetworkDomains []string
	// StartupMode is "", "always" or "on_demand"; "" lets the host normalize to
	// its documented default (always).
	StartupMode string
	// IdleTimeoutS is manifest startup.idle_timeout_s; only meaningful for
	// on_demand.
	IdleTimeoutS int
	// PluginDir is the magpie plugin directory the bridge loads and the
	// directory the supervisor launches from (cmd.Dir).
	PluginDir string
	// BridgeExe is the bridge executable. Absolute paths are accepted when they
	// live inside PluginDir; a relative path is relative to PluginDir; empty
	// means DefaultBridgeExeName.
	BridgeExe string
}

// BridgeArgs returns manifest.entry.args for a bridge plugin: only
// ["--plugin-dir", pluginDir]. The port, token and data directory are deliberately
// absent because the supervisor injects them as AIWORKGATE_PLUGIN_* environment
// variables on every launch (internal/plugins/env.go), and a restart would
// invalidate anything baked in here.
//
// It is exported so tests and callers can assert the exact argument vector.
func BridgeArgs(pluginDir string) []string {
	return []string{argPluginDir, pluginDir}
}

// ResolveBridgeEntry computes manifest.entry.exe for a bridge plugin and
// validates it against the same rules the host applies to a normal plugin
// (internal/plugins/manifest.go validateEntryPath): relative to the plugin
// directory, no drive/stream separator, no ".." escape.
//
// An absolute bridgeExe is converted to its path relative to pluginDir and
// rejected when it points outside it. That is not a limitation of this package
// but of the plugin contract: the supervisor resolves entry.exe as
// filepath.Join(pluginDir, entry.exe), so the shared bridge must be deployed
// into the plugin directory first (hard link or copy), see the package doc.
func ResolveBridgeEntry(pluginDir, bridgeExe string) (string, error) {
	pluginDir = strings.TrimSpace(pluginDir)
	bridgeExe = strings.TrimSpace(bridgeExe)
	if bridgeExe == "" {
		bridgeExe = DefaultBridgeExeName
	}

	var entry string
	if isAbsLike(bridgeExe) {
		if pluginDir == "" || !isAbsLike(pluginDir) {
			return "", fmt.Errorf("opchost: bridge exe %q is absolute, so the plugin dir %q must be absolute too", bridgeExe, pluginDir)
		}
		rel, err := filepath.Rel(pluginDir, bridgeExe)
		if err != nil {
			return "", fmt.Errorf("opchost: bridge exe %q is not inside plugin dir %q: %w", bridgeExe, pluginDir, err)
		}
		entry = filepath.ToSlash(rel)
	} else {
		entry = filepath.ToSlash(filepath.Clean(bridgeExe))
	}
	if err := validateBridgeEntry(entry); err != nil {
		return "", fmt.Errorf("opchost: bridge exe %q: %w (entry.exe must stay inside the plugin directory)", bridgeExe, err)
	}
	return entry, nil
}

// isAbsLike reports whether p is absolute on this platform or uses a spelling
// that only makes sense as an absolute path (a leading slash or a drive letter).
// It mirrors the pieces of filepath that validateEntryPath depends on.
func isAbsLike(p string) bool {
	if p == "" {
		return false
	}
	if filepath.IsAbs(p) {
		return true
	}
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) {
		return true
	}
	return len(p) >= 2 && p[1] == ':'
}

// validateBridgeEntry mirrors internal/plugins/manifest.go validateEntryPath.
func validateBridgeEntry(entry string) error {
	if entry == "" {
		return errors.New("must not be empty")
	}
	if strings.ContainsRune(entry, 0) {
		return errors.New("contains a NUL byte")
	}
	normalized := strings.ReplaceAll(entry, "\\", "/")
	if strings.HasPrefix(normalized, "/") || filepath.IsAbs(entry) || filepath.VolumeName(entry) != "" {
		return fmt.Errorf("%q must be relative to the plugin directory", entry)
	}
	if strings.Contains(normalized, ":") {
		return fmt.Errorf("%q must not contain a drive or stream separator", entry)
	}
	clean := filepath.ToSlash(filepath.Clean(entry))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("%q escapes the plugin directory", entry)
	}
	return nil
}

// GenerateBridgeManifest builds the manifest of one bridge plugin. The result is
// a plain model.Manifest, so the caller can marshal it to manifest.json inside
// <dataDir>\plugins\<id> and let the existing supervisor launch it unchanged.
//
// Nothing is written to disk and no process is started: this is pure manifest
// generation. The bridge binary is not required to exist yet (the host stats it
// when it loads the plugin).
func GenerateBridgeManifest(opts Options) (model.Manifest, error) {
	var mf model.Manifest

	id := strings.TrimSpace(opts.ID)
	if !bridgePluginIDPattern.MatchString(id) {
		return mf, fmt.Errorf("opchost: manifest: id %q must match %s", opts.ID, bridgePluginIDPattern.String())
	}
	apiLevel := opts.APILevel
	if apiLevel == 0 {
		apiLevel = model.MinAPILevel
	}
	if apiLevel < model.MinAPILevel || apiLevel > model.MaxAPILevel {
		return mf, fmt.Errorf("opchost: manifest %q: api_level %d is not supported (need %d-%d)",
			id, apiLevel, model.MinAPILevel, model.MaxAPILevel)
	}
	pluginDir := strings.TrimSpace(opts.PluginDir)
	if pluginDir == "" {
		return mf, fmt.Errorf("opchost: manifest %q: plugin dir is required", id)
	}
	entry, err := ResolveBridgeEntry(pluginDir, opts.BridgeExe)
	if err != nil {
		return mf, fmt.Errorf("opchost: manifest %q: %w", id, err)
	}
	startupMode := strings.TrimSpace(opts.StartupMode)
	switch startupMode {
	case "", model.StartupAlways, model.StartupOnDemand:
	default:
		return mf, fmt.Errorf("opchost: manifest %q: startup mode %q must be %q or %q",
			id, startupMode, model.StartupAlways, model.StartupOnDemand)
	}
	if opts.IdleTimeoutS < 0 {
		return mf, fmt.Errorf("opchost: manifest %q: idle timeout must not be negative", id)
	}
	capabilities := opts.Capabilities
	if capabilities == nil {
		capabilities = append([]string(nil), DefaultBridgeCapabilities...)
	}

	mf = model.Manifest{
		ID:           id,
		Name:         strings.TrimSpace(opts.Name),
		Version:      strings.TrimSpace(opts.Version),
		APILevel:     apiLevel,
		Description:  strings.TrimSpace(opts.Description),
		Capabilities: capabilities,
		Permissions:  model.Permissions{NetworkDomains: opts.NetworkDomains},
		Startup:      model.StartupConfig{Mode: startupMode, IdleTimeoutS: opts.IdleTimeoutS},
		Entry:        model.Entry{Exe: entry, Args: BridgeArgs(pluginDir)},
	}
	return mf, nil
}
