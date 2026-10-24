package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigAutoRefresh(t *testing.T) {
	dir := t.TempDir()
	cfg, err := loadConfig(dir)
	if err != nil {
		t.Fatalf("loadConfig(empty) error = %v", err)
	}
	if cfg.AutoRefresh {
		t.Fatal("auto refresh must default to off")
	}
	body, err := json.Marshal(Config{OfficeAceHome: `D:\OA`, AutoRefresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, configFileName), body, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = loadConfig(dir)
	if err != nil {
		t.Fatalf("loadConfig error = %v", err)
	}
	if !cfg.AutoRefresh || cfg.OfficeAceHome != `D:\OA` {
		t.Fatalf("config = %+v", cfg)
	}
}
