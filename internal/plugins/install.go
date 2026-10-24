package plugins

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"aiworkgate/internal/model"
)

const (
	// MaxZipBytes caps the uncompressed size of a dragged-in archive so a
	// malicious or corrupt zip cannot fill the disk. It is exported because the
	// admin API must also reject an oversized upload before it stages the file;
	// both sides read this one value so the limits can never drift apart.
	MaxZipBytes = 1 << 30

	// maxZipBytes is the in-package spelling used by the extractor below.
	maxZipBytes = MaxZipBytes

	// maxZipEntries bounds the number of entries a plugin archive may contain;
	// the per-entry byte cap alone still lets an archive with many tiny entries
	// create a lot of files.
	maxZipEntries = 4096
)

// InstallDir validates srcDir (manifest at the root or inside a single
// top-level directory) and installs it as plugins\<id>. The plugin data
// directory is created and the settings row is (re)written with the manifest
// defaults; a previously installed copy is stopped before the swap and a
// plugin configured to start always is launched afterwards.
//
// This is the developer-facing directory path (no HTTP route reaches it): it
// keeps its long-standing "install and enable" contract. The user-facing upload
// path is InstallZipOpts, which defaults a fresh install to stopped.
func (m *Manager) InstallDir(srcDir string) error {
	root, manifest, err := inspectInstallSource(srcDir)
	if err != nil {
		return err
	}
	return m.installRoot(root, manifest, false, InstallOpts{Enabled: true})
}

// InstallOpts carries the choices an install request is allowed to make.
// The zero value is the safe default: a fresh install lands stopped and the
// user starts it explicitly.
type InstallOpts struct {
	// Enabled starts a freshly installed plugin (subject to the existing
	// startup-mode decision). An update always keeps the stored enabled value.
	Enabled bool
}

// InstallZip extracts zipPath into a staging directory and installs it with
// the same validation and atomic swap as InstallDir.
func (m *Manager) InstallZip(zipPath string) error {
	return m.InstallZipOpts(zipPath, InstallOpts{})
}

// InstallZipOpts is InstallZip with explicit install options.
func (m *Manager) InstallZipOpts(zipPath string, opts InstallOpts) error {
	zipPath = strings.TrimSpace(zipPath)
	if zipPath == "" {
		return errors.New("plugins: install zip: empty path")
	}
	if m.dataDir == "" {
		return errors.New("plugins: install zip: data directory is not configured")
	}
	if err := os.MkdirAll(m.pluginsDir(), 0o755); err != nil {
		return fmt.Errorf("plugins: install zip: %w", err)
	}
	staging, err := os.MkdirTemp(m.pluginsDir(), ".unzip-")
	if err != nil {
		return fmt.Errorf("plugins: install zip: create staging dir: %w", err)
	}
	defer os.RemoveAll(staging)

	if err := extractZip(zipPath, staging); err != nil {
		return err
	}
	root, err := findManifestRoot(staging)
	if err != nil {
		return err
	}
	manifest, err := loadManifestDir(root)
	if err != nil {
		return err
	}
	return m.installRoot(root, manifest, false, opts)
}

// UpdateDir replaces the files of an already installed plugin while leaving
// plugin-data\<id> untouched. Settings are preserved; a running plugin is
// restarted.
func (m *Manager) UpdateDir(srcDir string) error {
	root, manifest, err := inspectInstallSource(srcDir)
	if err != nil {
		return err
	}
	return m.installRoot(root, manifest, true, InstallOpts{})
}

// inspectInstallSource locates and validates the plugin payload under base.
func inspectInstallSource(base string) (string, model.Manifest, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		return "", model.Manifest{}, errors.New("plugins: install: empty source path")
	}
	info, err := os.Stat(base)
	if err != nil {
		return "", model.Manifest{}, fmt.Errorf("plugins: install: %w", err)
	}
	if !info.IsDir() {
		return "", model.Manifest{}, fmt.Errorf("plugins: install: %q is not a directory", base)
	}
	root, err := findManifestRoot(base)
	if err != nil {
		return "", model.Manifest{}, err
	}
	manifest, err := loadManifestDir(root)
	if err != nil {
		return "", model.Manifest{}, err
	}
	return root, manifest, nil
}

// installRoot copies root into a staging directory and atomically swaps it in
// as plugins\<id>. opts only affects a fresh install; an update always keeps
// the stored settings.
func (m *Manager) installRoot(root string, manifest model.Manifest, update bool, opts InstallOpts) error {
	if m.dataDir == "" {
		return errors.New("plugins: install: data directory is not configured")
	}
	id := manifest.ID
	if !validPluginID(id) {
		return fmt.Errorf("plugins: install: invalid plugin id %q", id)
	}
	// Authenticate before touching the installed copy: a rejected package must
	// not stop or replace a plugin that is already running.
	if err := verifyPackageSignature(root, manifest, m.currentConfig().Plugins.TrustedKeys); err != nil {
		return err
	}

	wasRunning := m.isRunning(id)
	if err := m.Stop(id); err != nil && !errors.Is(err, ErrNotInstalled) {
		return err
	}

	pluginsDir := m.pluginsDir()
	if err := os.MkdirAll(pluginsDir, 0o755); err != nil {
		return fmt.Errorf("plugins: install: create %q: %w", pluginsDir, err)
	}
	staging, err := os.MkdirTemp(pluginsDir, ".staging-"+id+"-")
	if err != nil {
		return fmt.Errorf("plugins: install: create staging dir: %w", err)
	}
	defer os.RemoveAll(staging)

	if err := copyTree(root, staging); err != nil {
		return err
	}
	// Re-check the entry after copying: the contract requires it to exist in
	// the installed tree.
	exe := filepath.Join(staging, filepath.FromSlash(manifest.Entry.Exe))
	if !isRegularFile(exe) {
		return fmt.Errorf("plugins: install %q: entry.exe %q is missing or not a regular file after copy", id, manifest.Entry.Exe)
	}

	dest := m.installDir(id)
	backup := ""
	if _, statErr := os.Stat(dest); statErr == nil {
		backup = fmt.Sprintf("%s.old-%d", dest, time.Now().UnixNano())
		if err := os.Rename(dest, backup); err != nil {
			return fmt.Errorf("plugins: install %q: move previous copy: %w", id, err)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("plugins: install %q: %w", id, statErr)
	}
	if err := os.Rename(staging, dest); err != nil {
		if backup != "" {
			_ = os.Rename(backup, dest)
		}
		return fmt.Errorf("plugins: install %q: activate: %w", id, err)
	}
	if backup != "" {
		if err := os.RemoveAll(backup); err != nil {
			m.logWarn("cannot remove previous plugin copy", map[string]any{"plugin": id, "error": err.Error()})
		}
	}
	if err := os.MkdirAll(m.pluginDataDir(id), 0o755); err != nil {
		return fmt.Errorf("plugins: install %q: create data dir: %w", id, err)
	}

	settings := m.installSettings(manifest, update, opts)
	if err := m.persistSettings(id, settings); err != nil {
		return err
	}
	if err := m.refreshPlugin(id, dest, manifest); err != nil {
		return err
	}

	start := settings.enabled && (settings.mode == model.StartupAlways || wasRunning)
	if start {
		if err := m.Start(id); err != nil {
			m.logWarn("installed plugin could not be started", map[string]any{"plugin": id, "error": err.Error()})
		}
	}
	m.logInfo("plugin installed", map[string]any{
		"plugin": id, "version": manifest.Version, "update": update, "dir": dest,
	})
	return nil
}

// installSettings builds the settings written by an install. Updates keep the
// previous editable values; a fresh install applies the manifest defaults and
// lands disabled unless the caller explicitly asked for enabled (opts.Enabled).
// installed_at stays anchored to the first install.
func (m *Manager) installSettings(manifest model.Manifest, update bool, opts InstallOpts) pluginSettings {
	byID := m.settingsMap()
	if update {
		return m.deriveSettings(manifest.ID, manifest, byID)
	}
	cfg := m.currentConfig()
	s := pluginSettings{
		// A fresh install is inert by default: the archive came from outside and
		// enabling it starts an unsandboxed process as the gateway user, so that
		// is a deliberate one-click decision, not an install side effect.
		enabled:     opts.Enabled,
		mode:        modeOrDefault(manifest.Startup.Mode, cfg),
		checkin:     strings.TrimSpace(manifest.Checkin.DefaultTime),
		strict:      manifest.Egress.Strict,
		concurrency: clampConcurrency(manifest.Concurrency.Suggested, cfg.Plugins.Concurrency),
		installedAt: m.now().Unix(),
	}
	if s.checkin == "" || !timeOfDayPattern.MatchString(s.checkin) {
		s.checkin = cfg.Checkin.DefaultTime
	}
	if prev, ok := byID[manifest.ID]; ok && prev.InstalledAt > 0 {
		s.installedAt = prev.InstalledAt
	}
	return s
}

// copyTree copies the regular files and directories under src into dst.
// Symlinks and other special files are rejected rather than followed.
func copyTree(src, dst string) error {
	src = filepath.Clean(src)
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("plugins: copy %q: %w", p, err)
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("plugins: copy: symlink %q is not allowed", p)
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return fmt.Errorf("plugins: copy %q: %w", p, err)
		}
		target := dst
		if rel != "." {
			target = filepath.Join(dst, rel)
		}
		if d.IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("plugins: copy: create %q: %w", target, err)
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("plugins: copy: unsupported file %q", p)
		}
		return copyFile(p, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("plugins: copy: open %q: %w", src, err)
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("plugins: copy: stat %q: %w", src, err)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("plugins: copy: create %q: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("plugins: copy: write %q: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("plugins: copy: close %q: %w", dst, err)
	}
	return nil
}

// secureZipPath normalises an archive entry name and rejects absolute paths,
// drive letters, stream separators and any component that escapes the target.
func secureZipPath(name string) (string, error) {
	n := strings.ReplaceAll(name, "\\", "/")
	if n == "" {
		return "", nil
	}
	if strings.ContainsRune(n, 0) {
		return "", fmt.Errorf("plugins: zip entry %q contains a NUL byte", name)
	}
	if strings.HasPrefix(n, "/") || path.IsAbs(n) || filepath.VolumeName(name) != "" || strings.Contains(n, ":") {
		return "", fmt.Errorf("plugins: zip entry %q must be a relative path", name)
	}
	clean := path.Clean(n)
	if clean == "." {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("plugins: zip entry %q escapes the installation directory", name)
	}
	return clean, nil
}

// extractZip writes the archive contents below dst after validating every
// entry. Symlinks, device files and oversized archives are rejected.
func extractZip(zipPath, dst string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("plugins: open zip %q: %w", zipPath, err)
	}
	defer zr.Close()

	root := filepath.Clean(dst)
	var total uint64
	entries := 0
	for _, f := range zr.File {
		entries++
		if entries > maxZipEntries {
			return fmt.Errorf("plugins: zip %q has more than %d entries", zipPath, maxZipEntries)
		}
		rel, err := secureZipPath(f.Name)
		if err != nil {
			return err
		}
		if rel == "" {
			continue
		}
		mode := f.Mode()
		if mode&os.ModeSymlink != 0 {
			return fmt.Errorf("plugins: zip entry %q is a symlink", f.Name)
		}
		target := filepath.Join(root, filepath.FromSlash(rel))
		if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
			return fmt.Errorf("plugins: zip entry %q escapes the installation directory", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("plugins: unzip: create %q: %w", target, err)
			}
			continue
		}
		if !mode.IsRegular() {
			return fmt.Errorf("plugins: zip entry %q is not a regular file", f.Name)
		}
		next, ok := addZipEntrySize(total, f.UncompressedSize64)
		if !ok {
			return fmt.Errorf("plugins: zip %q expands beyond %d bytes", zipPath, maxZipBytes)
		}
		total = next
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("plugins: unzip: create %q: %w", filepath.Dir(target), err)
		}
		if err := extractZipFile(f, target); err != nil {
			return err
		}
	}
	return nil
}

// addZipEntrySize adds one attacker-controlled declared size to total without
// overflowing. A Zip64 header may declare up to 2^64-1; casting that to int64
// (or adding first) would make the running total negative and bypass the cap.
func addZipEntrySize(total, size uint64) (uint64, bool) {
	if size > maxZipBytes || total > maxZipBytes-size {
		return total, false
	}
	return total + size, true
}

func extractZipFile(f *zip.File, target string) error {
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("plugins: unzip %q: %w", f.Name, err)
	}
	defer rc.Close()
	perm := f.Mode().Perm()
	if perm == 0 {
		perm = 0o644
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("plugins: unzip: create %q: %w", target, err)
	}
	// LimitReader guards against a zip whose header understates its size.
	if _, err := io.Copy(out, io.LimitReader(rc, maxZipBytes)); err != nil {
		_ = out.Close()
		return fmt.Errorf("plugins: unzip: write %q: %w", target, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("plugins: unzip: close %q: %w", target, err)
	}
	return nil
}
