package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// configFileName is the plugin-owned overrides file under AIWORKGATE_PLUGIN_DATA_DIR.
const configFileName = "config.json"

// defaultOfficeAceHome is the install directory observed on this machine
// (Electron app root; storage.sqlite lives in <root>\data).
const defaultOfficeAceHome = `E:\Program\OfficeAce\program\OfficeAce`

// Config is the plugin data dir config.json. It never contains credentials.
type Config struct {
	OfficeAceHome string `json:"officeace_home,omitempty"`
	// AutoRefresh lets the plugin rotate OfficeAce's STS token automatically.
	// It is off by default: refreshing may rotate the refresh_token and thereby
	// invalidate the session stored by the OfficeAce app itself.
	AutoRefresh bool `json:"auto_refresh,omitempty"`
}

// Discovery is the resolved OfficeAce install location.
type Discovery struct {
	Home         string   `json:"home"`
	DBPath       string   `json:"db_path"`
	Found        bool     `json:"found"`
	Source       string   `json:"source"`
	Tried        []string `json:"tried,omitempty"`
	UserClawHome string   `json:"user_claw_home,omitempty"`
}

var percentVar = regexp.MustCompile(`%([^%]+)%`)

func loadConfig(dir string) (Config, error) {
	var cfg Config
	path := filepath.Join(dir, configFileName)
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.OfficeAceHome = strings.TrimSpace(cfg.OfficeAceHome)
	return cfg, nil
}

func saveConfig(dir string, cfg Config) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, configFileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

// expandWindowsVars expands %VAR% references and strips surrounding quotes.
func expandWindowsVars(raw string) string {
	s := strings.TrimSpace(raw)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			s = strings.TrimSpace(s[1 : len(s)-1])
		}
	}
	s = percentVar.ReplaceAllStringFunc(s, func(m string) string {
		name := strings.Trim(m, "%")
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		return m
	})
	if strings.HasPrefix(s, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			s = filepath.Join(home, strings.TrimLeft(s[1:], `\/`))
		}
	}
	return s
}

// discoverOfficeAce resolves the install root in the documented order:
// OFFICEACE_HOME env, plugin config.json, known default, LOCALAPPDATA, PROGRAMFILES.
func discoverOfficeAce(configured string) Discovery {
	disc := Discovery{UserClawHome: userClawHome()}
	seen := map[string]bool{}

	try := func(raw, source string) {
		if raw == "" || disc.Found {
			return
		}
		home := expandWindowsVars(raw)
		if home == "" {
			return
		}
		if abs, err := filepath.Abs(home); err == nil {
			home = abs
		}
		home = filepath.Clean(home)

		// Accept both the Electron root and its parent (…\OfficeAce\program\OfficeAce).
		candidates := []string{home}
		if parent := filepath.Join(home, "program", "OfficeAce"); parent != home {
			candidates = append(candidates, parent)
		}
		for _, candidate := range candidates {
			key := strings.ToLower(filepath.Clean(candidate))
			if seen[key] {
				continue
			}
			seen[key] = true
			db := filepath.Join(candidate, "data", "storage.sqlite")
			disc.Tried = append(disc.Tried, db)
			if st, err := os.Stat(db); err == nil && !st.IsDir() {
				disc.Home = candidate
				disc.DBPath = db
				disc.Found = true
				disc.Source = source
				return
			}
		}
		if disc.Home == "" {
			disc.Home = home
			disc.Source = source
		}
	}

	try(os.Getenv("OFFICEACE_HOME"), "env")
	try(configured, "config")
	try(defaultOfficeAceHome, "default")
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		try(filepath.Join(local, "Programs", "OfficeAce"), "localappdata")
	}
	if pf := os.Getenv("PROGRAMFILES"); pf != "" {
		try(filepath.Join(pf, "OfficeAce"), "programfiles")
	}
	if !disc.Found {
		disc.Source = "not_found"
	}
	return disc
}

// discoverIsolated resolves only the supplied path (used by tests so the real
// machine install is never touched).
func discoverIsolated(raw, userClaw string) Discovery {
	disc := Discovery{UserClawHome: userClaw, Source: "isolated"}
	if raw == "" {
		disc.Source = "not_found"
		return disc
	}
	home := expandWindowsVars(raw)
	if abs, err := filepath.Abs(home); err == nil {
		home = abs
	}
	home = filepath.Clean(home)
	for _, candidate := range []string{home, filepath.Join(home, "program", "OfficeAce")} {
		db := filepath.Join(candidate, "data", "storage.sqlite")
		disc.Tried = append(disc.Tried, db)
		if st, err := os.Stat(db); err == nil && !st.IsDir() {
			disc.Home = candidate
			disc.DBPath = db
			disc.Found = true
			return disc
		}
	}
	disc.Home = home
	disc.Source = "not_found"
	return disc
}

func userClawHome() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".office-claw")
}

// statusMessage explains an unsuccessful discovery to the host/UI.
func (d Discovery) statusMessage() string {
	if d.Found {
		return ""
	}
	if len(d.Tried) == 0 {
		return "未找到 OfficeAce 安装目录：请设置 OFFICEACE_HOME 或在插件 config.json 中填写 officeace_home"
	}
	return "未找到 OfficeAce 的 data\\storage.sqlite：请确认已安装并至少启动登录一次 OfficeAce，或通过 /accounts/login/{sid}/submit 指定 officeace_home"
}
