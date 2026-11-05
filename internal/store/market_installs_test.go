package store

import (
	"errors"
	"testing"
)

func TestMarketInstallUpsertAndList(t *testing.T) {
	s := openTestStore(t)

	first := MarketInstall{PluginID: "trae-bridge", Package: "@magpie-community/opencode-trae-auth", Source: "npm", Version: "0.2.1", SHA256: "abc", InstalledAt: 100}
	if err := s.UpsertMarketInstall(first); err != nil {
		t.Fatalf("UpsertMarketInstall: %v", err)
	}
	// A later install replaces the provenance (single row per plugin).
	second := MarketInstall{PluginID: "trae-bridge", Package: "@magpie-community/opencode-trae-auth", Source: "npm", Version: "0.2.30", SHA256: "def12"}
	if err := s.UpsertMarketInstall(second); err != nil {
		t.Fatalf("UpsertMarketInstall again: %v", err)
	}
	extra := MarketInstall{PluginID: "workbuddy-bridge", Package: "@magpie-community/opencode-workbuddy-auth", Source: "local", Version: "0.0.0-local"}
	if err := s.UpsertMarketInstall(extra); err != nil {
		t.Fatalf("UpsertMarketInstall extra: %v", err)
	}

	rows, err := s.ListMarketInstalls()
	if err != nil {
		t.Fatalf("ListMarketInstalls: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	// Ordered by plugin id: trae-bridge sorts before workbuddy-bridge.
	got := rows[0]
	if got.PluginID != "trae-bridge" || got.Version != "0.2.30" || got.SHA256 != "def12" {
		t.Fatalf("trae row = %+v, want the second install (replaced)", got)
	}
	if got.InstalledAt == 0 {
		t.Fatalf("installed_at = 0, want the defaulted now timestamp")
	}
	if rows[1].PluginID != "workbuddy-bridge" || rows[1].Version != "0.0.0-local" {
		t.Fatalf("rows[1] = %+v", rows[1])
	}
}

func TestMarketInstallRequiresPluginID(t *testing.T) {
	s := openTestStore(t)
	err := s.UpsertMarketInstall(MarketInstall{Package: "pkg", Version: "1.0.0"})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("error = %v, want ErrInvalidArgument", err)
	}
}

func TestMarketInstallEmpty(t *testing.T) {
	s := openTestStore(t)
	rows, err := s.ListMarketInstalls()
	if err != nil {
		t.Fatalf("ListMarketInstalls: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("fresh store must list no market installs, got %d", len(rows))
	}
}
