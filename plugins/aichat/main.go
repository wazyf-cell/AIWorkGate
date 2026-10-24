// Command aichat-plugin is the AIWorkGate forwarding plugin for the local
// "AI Work 助手" GUI (AIWorkAssistant.exe).
//
// Design boundary (see docs/决策台账-2026-10-21-第二轮.md, Q18/Q19):
//
//   - The assistant is an independent GUI process: it is started by HKCU Run,
//     owns its listening port (stored in its own sqlite kv) and does not check
//     any bearer token. It is therefore NOT a supervised child process; this
//     plugin only forwards traffic and never starts or stops it.
//   - The upstream base_url and api_key live in this plugin's own data
//     directory (accounts.dat, DPAPI-protected). The host never passes the key
//     through the environment: that would only relocate the hardcoding.
//
// The host injects AIWORKGATE_PLUGIN_PORT and AIWORKGATE_PLUGIN_TOKEN; the
// plugin binds 127.0.0.1 on that port and never picks one itself.
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
	// pluginID must match manifest.json "id" (Q19: single-char ids are
	// rejected by internal/plugins/manifest.go's ^[a-z0-9][a-z0-9-]{1,31}$).
	pluginID = "aichat"
	// pluginVersion must match manifest.json "version".
	pluginVersion = "0.1.0"
	// apiLevel mirrors manifest.json "api_level". The host accepts 1-2
	// (internal/model/model.go: MinAPILevel=1, MaxAPILevel=2) and injects the
	// declared level as AIWORKGATE_API_LEVEL.
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
		RulesFile:  strings.TrimSpace(os.Getenv("AIWORKGATE_RULES_FILE")),
		EgressFile: strings.TrimSpace(os.Getenv("AIWORKGATE_EGRESS_FILE")),
		APILevel:   envIntOr("AIWORKGATE_API_LEVEL", apiLevel),
		StartedAt:  time.Now(),
	})
	if err != nil {
		log.Fatalf("start plugin: %v", err)
	}
	if app.cfg.Token == "" {
		app.log("warn", "AIWORKGATE_PLUGIN_TOKEN is empty; refusing every non-health request", nil)
	}

	addr := "127.0.0.1:" + strconv.Itoa(port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           app.handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	app.log("info", "plugin listening", map[string]any{
		"addr": addr, "id": app.cfg.ID, "version": pluginVersion,
		"api_level": app.cfg.APILevel, "data_dir": app.cfg.DataDir,
		"upstream_configured": app.store.configured(),
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
