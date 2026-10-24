package main

import (
	"context"
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
	ID       string
	APILevel int
	Port     string
	Token    string
	DataDir  string
	Egress   string
}

func loadConfig() pluginConfig {
	return pluginConfig{
		ID:       env("AIWORKGATE_PLUGIN_ID", "zcode"),
		APILevel: 2,
		Port:     env("AIWORKGATE_PLUGIN_PORT", "0"),
		Token:    env("AIWORKGATE_PLUGIN_TOKEN", ""),
		DataDir:  env("AIWORKGATE_PLUGIN_DATA_DIR", defaultDataDir()),
		Egress:   strings.TrimSpace(os.Getenv("AIWORKGATE_EGRESS_FILE")),
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
		return base + string(os.PathSeparator) + "AIWorkGate" + string(os.PathSeparator) + "plugin-data" + string(os.PathSeparator) + "zcode"
	}
	return "zcode-data"
}

func main() {
	cfg := loadConfig()
	st, err := newStore(cfg.DataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zcode:", err)
		os.Exit(1)
	}
	srv := &server{cfg: cfg, store: st, client: newUpstreamClient(), now: time.Now}
	srv.client.transport = srv.transportFor
	if err := srv.reloadEgress(); err != nil {
		fmt.Fprintln(os.Stderr, "zcode: egress:", err)
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
		fmt.Fprintln(os.Stderr, "zcode: listen:", err)
		os.Exit(1)
	}
}

// manifestModels is the static catalog served when the live one is unavailable
// or the stored credential is a desktop JWT (the zcode-plan channel lists
// models only with a captcha the plugin cannot mint). The ids are the ones
// GET https://api.z.ai/api/anthropic/v1/models actually returns.
func manifestModels() []map[string]any {
	ids := []struct{ id, name string }{
		{"glm-5.3", "GLM-5.3"},
		{"glm-5.3-flash", "GLM-5.3-Flash"},
		{"glm-5.2", "GLM-5.2"},
		{"glm-5.1", "GLM-5.1"},
		{"glm-5", "GLM-5"},
		{"glm-4.7", "GLM-4.7"},
		{"glm-4.6", "GLM-4.6"},
	}
	out := make([]map[string]any, 0, len(ids))
	for _, m := range ids {
		out = append(out, map[string]any{"id": m.id, "object": "model", "owned_by": "zcode", "name": m.name})
	}
	return out
}

// sha256Sum returns the hex digest of secret, used for fingerprints only.
func sha256Sum(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
