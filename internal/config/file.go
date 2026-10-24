package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"aiworkgate/internal/fsutil"
)

// DataDir returns the per-user AIWorkGate data directory,
// %APPDATA%\AIWorkGate. It fails when APPDATA is not set.
func DataDir() (string, error) {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return "", errors.New("config: APPDATA is not set, cannot locate the data directory")
	}
	return filepath.Join(appData, "AIWorkGate"), nil
}

// ConfigPath returns the configuration file path inside dir.
func ConfigPath(dir string) string {
	return filepath.Join(dir, FileName)
}

// EnsureDir creates dir (and parents) when missing.
func EnsureDir(dir string) error {
	if dir == "" {
		return errors.New("config: empty data directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: create %s: %w", dir, err)
	}
	return nil
}

// Load reads dir\config.json. A missing file yields Default(), which is
// written before returning. Unknown JSON fields are ignored. Read values are
// normalized (clamped) and then validated.
func Load(dir string) (Config, error) {
	path := ConfigPath(dir)
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			cfg := Default()
			if serr := Save(dir, cfg); serr != nil {
				return cfg, serr
			}
			return cfg, nil
		}
		return Config{}, fmt.Errorf("config: read %s: %w", path, err)
	}

	// Decode over the defaults so fields absent from an older or partial file
	// keep their default values.
	cfg := Default()
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("config: parse %s: %w", path, err)
	}
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("%w (%s)", err, path)
	}
	return cfg, nil
}

// Save normalizes cfg, validates it and atomically replaces dir\config.json
// with mode 0600.
func Save(dir string, cfg Config) error {
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := EnsureDir(dir); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("config: marshal: %w", err)
	}
	data = append(data, '\n')
	if err := fsutil.WriteFileAtomic(ConfigPath(dir), data, 0o600); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	return nil
}
