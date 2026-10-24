package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// pluginConfig is the slice of the host-injected environment this plugin needs.
type pluginConfig struct {
	ID         string
	APILevel   int
	Port       string
	Token      string
	DataDir    string
	EgressFile string
}

func loadConfig() pluginConfig {
	return pluginConfig{
		ID:         env("AIWORKGATE_PLUGIN_ID", "minimax"),
		APILevel:   2,
		Port:       env("AIWORKGATE_PLUGIN_PORT", "0"),
		Token:      env("AIWORKGATE_PLUGIN_TOKEN", ""),
		DataDir:    env("AIWORKGATE_PLUGIN_DATA_DIR", defaultDataDir()),
		EgressFile: env("AIWORKGATE_EGRESS_FILE", ""),
	}
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// defaultDataDir is used only when the host did not inject one, so a manual run
// still has somewhere to keep its sealed credentials.
func defaultDataDir() string {
	if base := os.Getenv("APPDATA"); base != "" {
		return base + string(os.PathSeparator) + "AIWorkGate" + string(os.PathSeparator) + "plugin-data" + string(os.PathSeparator) + "minimax"
	}
	return "minimax-data"
}

func main() {
	cfg := loadConfig()
	srv, err := newServer(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "minimax:", err)
		os.Exit(1)
	}

	addr := ":" + cfg.Port
	if cfg.Port == "" || cfg.Port == "0" {
		addr = ":0"
	}
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, "minimax: listen:", err)
		os.Exit(1)
	}
}

// manifestModels is the static catalog the manifest declares, mirroring
// MINIMAX_FALLBACK_MODELS. The plugin serves it from /v1/models so the host can
// list models without a live upstream call.
func manifestModels() []map[string]any {
	return []map[string]any{
		{"id": "MiniMax-M3.1-Flash-Preview", "object": "model", "owned_by": "minimax", "name": "M3.1-Flash-Preview"},
		{"id": "MiniMax-M3", "object": "model", "owned_by": "minimax", "name": "M3"},
		{"id": "MiniMax-M2.7-highspeed", "object": "model", "owned_by": "minimax", "name": "M2.7-highspeed"},
		{"id": "MiniMax-M2.7", "object": "model", "owned_by": "minimax", "name": "M2.7"},
	}
}

// sha256Sum returns the hex digest of secret, used for fingerprints only.
func sha256Sum(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// newUUID returns a random RFC-4122 v4 UUID for a login flow id.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	dst := make([]byte, 36)
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst)
}
