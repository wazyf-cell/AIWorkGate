package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
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
}

func loadConfig() pluginConfig {
	return pluginConfig{
		ID:       env("AIWORKGATE_PLUGIN_ID", "raccoon"),
		APILevel: envInt("AIWORKGATE_API_LEVEL", 2),
		Port:     env("AIWORKGATE_PLUGIN_PORT", "0"),
		Token:    env("AIWORKGATE_PLUGIN_TOKEN", ""),
		DataDir:  env("AIWORKGATE_PLUGIN_DATA_DIR", defaultDataDir()),
	}
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

// defaultDataDir is used only when the host did not inject one.
func defaultDataDir() string {
	if base := os.Getenv("APPDATA"); base != "" {
		sep := string(os.PathSeparator)
		return base + sep + "AIWorkGate" + sep + "plugin-data" + sep + "raccoon"
	}
	return "raccoon-data"
}

func main() {
	cfg := loadConfig()
	st, err := newStore(cfg.DataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "raccoon:", err)
		os.Exit(1)
	}
	srv := &server{
		cfg:        cfg,
		store:      st,
		client:     newUpstreamClient(),
		logins:     newLoginManager(),
		egressPath: strings.TrimSpace(os.Getenv("AIWORKGATE_EGRESS_FILE")),
	}
	ef, eerr := loadEgress(srv.egressPath)
	if eerr != nil {
		// A broken egress file must not stop the plugin; strict mode will refuse
		// the accounts it cannot resolve instead of silently going direct.
		fmt.Fprintln(os.Stderr, "raccoon: egress:", eerr)
	}
	srv.egress = ef

	addr := "127.0.0.1:" + cfg.Port
	if cfg.Port == "" || cfg.Port == "0" {
		addr = "127.0.0.1:0"
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
		fmt.Fprintln(os.Stderr, "raccoon: listen:", err)
		os.Exit(1)
	}
}

// fallbackModels is the static catalog served when the remote one is
// unreachable. Ported from RACCOON_FALLBACK_MODELS (raccoon-product.ts): the ids,
// display names, context windows and per-model maxTokens are the reference's.
func fallbackModels() []raccoonModel {
	return []raccoonModel{
		{ID: "sn-sensenova-6-8-flash", Name: "SenseNova-6.8-Flash · 免费", ContextLength: 256000, Tools: true, Vision: true, Thinking: raccoonEfforts},
		{ID: "sn-sensenova-6-8-flash-lite", Name: "SenseNova-6.8-Flash-Lite · 免费", ContextLength: 256000, Tools: true, Vision: true, Thinking: raccoonEfforts},
		{ID: "sn-glm-5-3", Name: "GLM-5-3 · x0.75", ContextLength: 1000000, Tools: true, Thinking: raccoonEfforts},
	}
}

// sha256Sum returns the hex digest of secret, used for fingerprints only.
func sha256Sum(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
