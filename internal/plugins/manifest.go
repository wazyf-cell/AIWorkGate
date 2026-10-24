// Package plugins implements the AIWorkGate plugin runtime described by the
// plugin contract v0.2: manifest validation, install/update/uninstall,
// process supervision, health watching and the typed client for the plugin
// HTTP contract.
package plugins

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"aiworkgate/internal/config"
	"aiworkgate/internal/model"
)

// On-disk layout names below <dataDir>.
const (
	pluginsDirName    = "plugins"
	pluginDataDirName = "plugin-data"
	runtimeDirName    = "runtime"

	// ManifestFileName is the descriptor every plugin ships at its root.
	ManifestFileName = "manifest.json"

	manifestMaxBytes = 1 << 20

	// DefaultIdleTimeoutS is the idle timeout for on-demand plugins when the
	// manifest does not set startup.idle_timeout_s.
	DefaultIdleTimeoutS = 600
)

var (
	pluginIDPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,31}$`)
	modelIDPattern   = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)
	timeOfDayPattern = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
)

// ErrNotInstalled is returned for operations on an unknown plugin id.
var ErrNotInstalled = errors.New("plugins: not installed")

// ErrDisabled is returned when a disabled plugin is asked to run.
var ErrDisabled = errors.New("plugins: plugin is disabled")

// ErrNotRunning is returned when a plugin endpoint is requested while no
// plugin process is running.
var ErrNotRunning = errors.New("plugins: plugin is not running")

// validPluginID reports whether id matches the contract's plugin id pattern.
func validPluginID(id string) bool { return pluginIDPattern.MatchString(id) }

// LoadManifestFile reads and parses one manifest file. It does not validate
// the contents; call ValidateManifest for that.
func LoadManifestFile(path string) (model.Manifest, error) {
	var m model.Manifest
	info, err := os.Stat(path)
	if err != nil {
		return m, fmt.Errorf("plugins: read manifest %q: %w", path, err)
	}
	if info.IsDir() {
		return m, fmt.Errorf("plugins: read manifest %q: is a directory", path)
	}
	if info.Size() > manifestMaxBytes {
		return m, fmt.Errorf("plugins: read manifest %q: file is larger than %d bytes", path, manifestMaxBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return m, fmt.Errorf("plugins: read manifest %q: %w", path, err)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("plugins: parse manifest %q: %w", path, err)
	}
	return m, nil
}

// KnownManifestKeys is the top-level manifest vocabulary this host decodes.
// Anything else is ignored by json.Unmarshal, which makes a misspelling behave
// exactly like an omitted field - hence UnknownManifestKeys.
var KnownManifestKeys = []string{
	"id", "name", "version", "api_level", "description", "author", "homepage",
	"os", "capabilities", "permissions", "models", "checkin", "quota",
	"concurrency", "rules", "egress", "startup", "entry", "login",
}

// knownNestedManifestKeys lists the named fields of each object-valued block.
// A block absent from this map has no fields the host reads (os, capabilities,
// ... are arrays of strings), so it is not inspected.
var knownNestedManifestKeys = map[string][]string{
	"permissions": {"read_paths", "launch_apps", "network_domains", "use_proxy", "os_identity", "require_admin"},
	"checkin":     {"default_time", "supports_history"},
	"quota":       {"poll_minutes"},
	"concurrency": {"suggested"},
	"rules":       {"packs"},
	"egress":      {"strict"},
	"startup":     {"mode", "idle_timeout_s"},
	"entry":       {"exe", "args"},
	"login":       {"methods", "reads_local_state", "requires_app", "auto_refresh", "notes"},
	"models": {"id", "name", "context_length", "context_window", "tools", "vision",
		"vendor", "multiplier", "thinking", "free", "display", "supports_image"},
}

// UnknownManifestKeys lists keys the host would silently ignore: top-level ones
// that are not in KnownManifestKeys and, one level down, unknown fields of the
// object-valued blocks in knownNestedManifestKeys. Nested keys are reported as
// "block.key"; a models entry uses "models[<index>].<key>" so a repeated typo
// stays attributable to one entry. Blocks that are absent or not a JSON object
// (models: not an array; entries: not an object) are skipped.
//
// The result is sorted, because decoding a JSON object into a map loses the
// document order. Malformed JSON yields nil: the caller already reports the
// parse error, and an unknown-key warning must never be the reason a plugin
// fails to load.
func UnknownManifestKeys(data []byte) []string {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil
	}
	known := make(map[string]struct{}, len(KnownManifestKeys))
	for _, key := range KnownManifestKeys {
		known[key] = struct{}{}
	}
	var out []string
	collect := func(prefix string, raw json.RawMessage, fields []string) {
		var block map[string]json.RawMessage
		if !isJSONKind(raw, '{') || json.Unmarshal(raw, &block) != nil {
			return
		}
		out = append(out, unknownNestedKeys(prefix, block, fields)...)
	}
	for key, raw := range top {
		fields, nested := knownNestedManifestKeys[key]
		if _, ok := known[key]; !ok {
			out = append(out, key)
			continue
		}
		if !nested {
			continue
		}
		if key != "models" {
			collect(key, raw, fields)
			continue
		}
		var entries []json.RawMessage
		if !isJSONKind(raw, '[') || json.Unmarshal(raw, &entries) != nil {
			continue
		}
		for i, entry := range entries {
			collect(fmt.Sprintf("models[%d]", i), entry, fields)
		}
	}
	sort.Strings(out)
	return out
}

func unknownNestedKeys(prefix string, block map[string]json.RawMessage, fields []string) []string {
	known := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		known[field] = struct{}{}
	}
	var out []string
	for key := range block {
		if _, ok := known[key]; !ok {
			out = append(out, prefix+"."+key)
		}
	}
	return out
}

// isJSONKind reports whether raw (after leading whitespace) starts with open,
// which keeps a block of the wrong JSON type out of the nested scan.
func isJSONKind(raw json.RawMessage, open byte) bool {
	trimmed := strings.TrimLeft(string(raw), " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == open
}

// ValidateManifest checks a manifest against contract v0.2 section 5. Empty
// optional values are accepted (startup mode, check-in time and concurrency
// have documented defaults); the entry executable is only required to be a
// safe relative path, existence is checked after the files are copied.
func ValidateManifest(m model.Manifest) error {
	if !validPluginID(m.ID) {
		return fmt.Errorf("plugins: manifest: id %q must match %s", m.ID, pluginIDPattern.String())
	}
	if m.APILevel < model.MinAPILevel || m.APILevel > model.MaxAPILevel {
		return fmt.Errorf("plugins: manifest %q: api_level %d is not supported (need %d-%d)",
			m.ID, m.APILevel, model.MinAPILevel, model.MaxAPILevel)
	}
	if err := validateEntryPath(m.Entry.Exe); err != nil {
		return fmt.Errorf("plugins: manifest %q: entry.exe: %w", m.ID, err)
	}
	for _, mi := range m.Models {
		if !modelIDPattern.MatchString(mi.ID) {
			return fmt.Errorf("plugins: manifest %q: model id %q must match %s", m.ID, mi.ID, modelIDPattern.String())
		}
	}
	if t := strings.TrimSpace(m.Checkin.DefaultTime); t != "" && !timeOfDayPattern.MatchString(t) {
		return fmt.Errorf("plugins: manifest %q: checkin.default_time %q must be HH:MM", m.ID, t)
	}
	switch strings.TrimSpace(m.Startup.Mode) {
	case "", model.StartupAlways, model.StartupOnDemand:
	default:
		return fmt.Errorf("plugins: manifest %q: startup.mode %q must be %q or %q", m.ID, m.Startup.Mode, model.StartupAlways, model.StartupOnDemand)
	}
	if m.Startup.IdleTimeoutS < 0 {
		return fmt.Errorf("plugins: manifest %q: startup.idle_timeout_s must not be negative", m.ID)
	}
	if m.Concurrency.Suggested < 0 || m.Concurrency.Suggested > config.MaxConcurr {
		return fmt.Errorf("plugins: manifest %q: concurrency.suggested %d out of range 0-%d", m.ID, m.Concurrency.Suggested, config.MaxConcurr)
	}
	if m.Quota.PollMinutes != nil && *m.Quota.PollMinutes < 0 {
		return fmt.Errorf("plugins: manifest %q: quota.poll_minutes must not be negative", m.ID)
	}
	if len(m.OS) > 0 {
		found := false
		for _, o := range m.OS {
			if strings.EqualFold(strings.TrimSpace(o), "windows") {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("plugins: manifest %q: os %v does not include windows", m.ID, m.OS)
		}
	}
	return nil
}

// validateEntryPath rejects absolute paths, drive-qualified paths and any
// component that would escape the plugin directory.
func validateEntryPath(exe string) error {
	exe = strings.TrimSpace(exe)
	if exe == "" {
		return errors.New("must not be empty")
	}
	if strings.ContainsRune(exe, 0) {
		return errors.New("contains a NUL byte")
	}
	normalized := strings.ReplaceAll(exe, "\\", "/")
	if strings.HasPrefix(normalized, "/") || filepath.IsAbs(exe) || filepath.VolumeName(exe) != "" {
		return fmt.Errorf("%q must be relative to the plugin directory", exe)
	}
	if strings.Contains(normalized, ":") {
		return fmt.Errorf("%q must not contain a drive or stream separator", exe)
	}
	clean := filepath.ToSlash(filepath.Clean(exe))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("%q escapes the plugin directory", exe)
	}
	return nil
}

// normalizeManifest trims the values the runtime compares and resolves the
// documented defaults, without rewriting optional fields.
func normalizeManifest(m model.Manifest) model.Manifest {
	m.ID = strings.TrimSpace(m.ID)
	m.Name = strings.TrimSpace(m.Name)
	m.Version = strings.TrimSpace(m.Version)
	m.Entry.Exe = strings.TrimSpace(m.Entry.Exe)
	m.Entry.Exe = filepath.ToSlash(m.Entry.Exe)
	m.Startup.Mode = strings.TrimSpace(m.Startup.Mode)
	if m.Startup.Mode == "" {
		m.Startup.Mode = model.StartupAlways
	}
	m.Checkin.DefaultTime = strings.TrimSpace(m.Checkin.DefaultTime)
	return m
}

// loadManifestDir loads and validates <dir>\manifest.json.
func loadManifestDir(dir string) (model.Manifest, error) {
	mf, err := LoadManifestFile(filepath.Join(dir, ManifestFileName))
	if err != nil {
		return mf, err
	}
	if err := ValidateManifest(mf); err != nil {
		return mf, err
	}
	if mf.Entry.Exe != "" {
		exe := filepath.Join(dir, filepath.FromSlash(mf.Entry.Exe))
		info, statErr := os.Stat(exe)
		switch {
		case errors.Is(statErr, os.ErrNotExist):
			return mf, fmt.Errorf("plugins: manifest %q: entry.exe %q does not exist", mf.ID, mf.Entry.Exe)
		case statErr != nil:
			return mf, fmt.Errorf("plugins: manifest %q: entry.exe %q: %w", mf.ID, mf.Entry.Exe, statErr)
		case info.IsDir():
			return mf, fmt.Errorf("plugins: manifest %q: entry.exe %q is a directory", mf.ID, mf.Entry.Exe)
		}
	}
	return normalizeManifest(mf), nil
}

// findManifestRoot locates the plugin root inside base: either base itself
// (manifest at the archive root) or a single top-level directory that holds
// the manifest. Archive junk such as __MACOSX is ignored.
func findManifestRoot(base string) (string, error) {
	if isRegularFile(filepath.Join(base, ManifestFileName)) {
		return base, nil
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return "", fmt.Errorf("plugins: read %q: %w", base, err)
	}
	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if e.Name() == "__MACOSX" || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dirs = append(dirs, filepath.Join(base, e.Name()))
	}
	if len(dirs) == 1 && isRegularFile(filepath.Join(dirs[0], ManifestFileName)) {
		return dirs[0], nil
	}
	return "", fmt.Errorf("plugins: %s not found at the root or inside a single top-level directory", ManifestFileName)
}

func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
