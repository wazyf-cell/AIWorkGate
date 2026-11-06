// Command cline-plugin forwards the host's OpenAI-compatible traffic to Cline's
// gateway (https://api.cline.bot/api/v1/chat/completions).
//
// Design boundary:
//
//   - Cline speaks OpenAI natively, so there is no protocol conversion. What the
//     wire needs is the envelope: the Authorization value must be
//     `workos:<accessToken>`, and every call carries the four Cline client
//     headers. Both live in protocol.go and are pinned by tests.
//   - Login is a WorkOS device-code flow (browser round trip) whose tokens are then
//     exchanged for a Cline session via /api/v1/auth/register. Renewal goes through
//     /api/v1/auth/refresh with the WorkOS refresh token.
//   - The token lives in this plugin's own data directory (accounts.dat,
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
	pluginID = "cline"
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
		ID:         envOr("AIWORKGATE_PLUGIN_ID", pluginID),
		Token:      os.Getenv("AIWORKGATE_PLUGIN_TOKEN"),
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
		"configured": app.store.configured(),
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
