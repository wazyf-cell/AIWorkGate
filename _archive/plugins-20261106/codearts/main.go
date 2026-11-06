package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// main starts the CodeArts plugin on 127.0.0.1:$AIWORKGATE_PLUGIN_PORT.
func main() {
	port := strings.TrimSpace(os.Getenv("AIWORKGATE_PLUGIN_PORT"))
	if port == "" {
		log.Fatal("AIWORKGATE_PLUGIN_PORT is required")
	}
	token := os.Getenv("AIWORKGATE_PLUGIN_TOKEN")
	dataDir := strings.TrimSpace(os.Getenv("AIWORKGATE_PLUGIN_DATA_DIR"))
	if dataDir == "" {
		dataDir = "plugin-data"
	}

	p, err := New(Options{
		DataDir:    dataDir,
		Token:      token,
		RulesFile:  strings.TrimSpace(os.Getenv("AIWORKGATE_RULES_FILE")),
		EgressFile: strings.TrimSpace(os.Getenv("AIWORKGATE_EGRESS_FILE")),
		Endpoints:  endpointsFromEnv(),
	})
	if err != nil {
		log.Fatalf("start plugin: %v", err)
	}
	defer p.Close()
	if token == "" {
		p.logLine("warn", "AIWORKGATE_PLUGIN_TOKEN is empty; all non-health requests will be rejected", nil)
	}

	addr := "127.0.0.1:" + port
	srv := &http.Server{
		Addr:              addr,
		Handler:           p.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	p.logLine("info", "plugin listening", map[string]any{
		"addr":           addr,
		"id":             p.ID,
		"version":        p.Version,
		"data_dir":       dataDir,
		"authorize_mode": describeAuthorizeMode(p.endpoints.Authorize),
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		p.logLine("error", "listen failed", map[string]any{"addr": addr, "error": err.Error()})
		os.Exit(1)
	}
	p.logLine("info", "plugin stopped", nil)
}
