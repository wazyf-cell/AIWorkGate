// Command opencode-plugin forwards the host's OpenAI-compatible traffic to the
// OpenCode free inference tier (https://opencode.ai/inference/v1).
//
// Design boundary:
//
//   - The upstream's free tier is usable with no login at all: the official CLI
//     sends the literal key `public` when it has no key, and the plugin does the
//     same. A stored sk- key is only needed for the paid models.
//   - The gate is a request SHAPE, not a credential: stream must be true and the
//     payload must declare the official read/shell tool definitions, or the
//     upstream answers 403 FreeTierError. That rewrite lives in protocol.go and
//     is pinned by tests.
//   - The key lives in this plugin's own data directory (accounts.dat,
//     DPAPI-protected). The host never passes it through the environment.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// pluginID must match manifest.json "id".
	pluginID = "opencode"
	// apiLevel mirrors manifest.json "api_level".
	apiLevel = 2
)

func main() {
	rawPort := strings.TrimSpace(os.Getenv("AIWORKGATE_PLUGIN_PORT"))
	if rawPort == "" {
		log.Fatal("AIWORKGATE_PLUGIN_PORT is required; start the plugin through the host or set it yourself")
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port < 1 || port > 65535 {
		log.Fatalf("invalid AIWORKGATE_PLUGIN_PORT %q", rawPort)
	}

	dataDir := strings.TrimSpace(os.Getenv("AIWORKGATE_PLUGIN_DATA_DIR"))
	if dataDir == "" {
		dataDir = "plugin-data"
	}

	app, err := newServer(serverConfig{
		ID:        envOr("AIWORKGATE_PLUGIN_ID", pluginID),
		Token:     os.Getenv("AIWORKGATE_PLUGIN_TOKEN"),
		DataDir:    dataDir,
		APILevel:   envIntOr("AIWORKGATE_API_LEVEL", apiLevel),
		StartedAt:  time.Now(),
		EgressFile: strings.TrimSpace(os.Getenv("AIWORKGATE_EGRESS_FILE")),
	})
	if err != nil {
		log.Fatalf("start plugin: %v", err)
	}
	if app.cfg.Token == "" {
		app.log("warn", "AIWORKGATE_PLUGIN_TOKEN is empty; every non-health request will be rejected", nil)
	}

	addr := "127.0.0.1:" + strconv.Itoa(port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	app.log("info", "plugin listening", map[string]any{
		"addr": addr, "id": app.cfg.ID, "version": pluginVersion,
		"api_level": app.cfg.APILevel, "data_dir": app.cfg.DataDir,
		"anonymous": app.store.current().anonymous(),
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		app.log("info", "shutdown requested", nil)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		app.log("error", "listen failed", map[string]any{"addr": addr, "error": err.Error()})
		os.Exit(1)
	}
	app.log("info", "plugin stopped", nil)
}

func envOr(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

func envIntOr(name string, fallback int) int {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
