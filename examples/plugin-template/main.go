// Command hello-plugin is the AIWorkGate plugin template.
//
// It speaks the AIWorkGate plugin contract (api_level 1) over loopback HTTP:
// the host starts this process, injects AIWORKGATE_PLUGIN_PORT and
// AIWORKGATE_PLUGIN_TOKEN, then polls GET /health before routing traffic.
//
// The plugin never chooses its own port and never reads the host's data
// directory: everything it needs arrives through the environment.
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
	pluginID = "hello"
	// pluginVersion must match manifest.json "version".
	pluginVersion = "0.1.0"
	// apiLevel must equal the host's supported level (internal/model.APIVersion).
	apiLevel = 1
)

func main() {
	// 1. The host always injects the port. There is no fixed fallback: a
	// plugin that guesses a port can collide with the system reserved range.
	rawPort := strings.TrimSpace(os.Getenv("AIWORKGATE_PLUGIN_PORT"))
	if rawPort == "" {
		log.Fatal("AIWORKGATE_PLUGIN_PORT is required; start the plugin through the host or set it yourself")
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port < 1 || port > 65535 {
		log.Fatalf("invalid AIWORKGATE_PLUGIN_PORT %q", rawPort)
	}

	app := newServer(serverConfig{
		ID:         envOrDefault("AIWORKGATE_PLUGIN_ID", pluginID),
		Token:      os.Getenv("AIWORKGATE_PLUGIN_TOKEN"),
		DataDir:    envOrDefault("AIWORKGATE_PLUGIN_DATA_DIR", "plugin-data"),
		RulesFile:  os.Getenv("AIWORKGATE_RULES_FILE"),
		EgressFile: os.Getenv("AIWORKGATE_EGRESS_FILE"),
		StartedAt:  time.Now(),
	})

	// 2. Bind loopback only. The contract forbids listening on 0.0.0.0.
	addr := "127.0.0.1:" + strconv.Itoa(port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           app.handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	app.log("info", "plugin listening", map[string]any{
		"addr": addr, "id": app.cfg.ID, "version": pluginVersion,
		"api_level": apiLevel, "data_dir": app.cfg.DataDir,
	})

	// 3. Stop on Ctrl+C / SIGTERM. The Windows host stops a child with
	// `taskkill /T` and, after 5s, `taskkill /F /T`; it never closes stdin
	// (stdin is the NUL device), so stdin EOF must NOT be used as a stop
	// signal or the plugin would exit immediately after launch.
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

func envOrDefault(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}
