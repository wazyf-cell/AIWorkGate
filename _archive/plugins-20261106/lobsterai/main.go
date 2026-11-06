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
	Egress   string
}

func loadConfig() pluginConfig {
	return pluginConfig{
		ID:       env("AIWORKGATE_PLUGIN_ID", "lobsterai"),
		APILevel: envInt("AIWORKGATE_API_LEVEL", 2),
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
		return base + sep + "AIWorkGate" + sep + "plugin-data" + sep + "lobsterai"
	}
	return "lobsterai-data"
}

func main() {
	cfg := loadConfig()
	st, err := newStore(cfg.DataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lobsterai:", err)
		os.Exit(1)
	}
	srv := &server{cfg: cfg, store: st, client: newUpstreamClient()}
	srv.client.transport = srv.transportFor
	if err := srv.reloadEgress(); err != nil {
		fmt.Fprintln(os.Stderr, "lobsterai: egress:", err)
	}

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
		fmt.Fprintln(os.Stderr, "lobsterai: listen:", err)
		os.Exit(1)
	}
}

// fallbackModels is the static catalog served when the remote one is
// unreachable. Ported from LOBSTERAI_FALLBACK_MODELS (lobsterai-product.ts): ids,
// names and the 131072 context window are the reference's.
func fallbackModels() []lobsteraiModel {
	ids := []string{
		"deepseek-v4-flash", "deepseek-v4-pro",
		"MiniMax-M3", "MiniMax-M2.7",
		"qwen3.7-max", "qwen3.7-plus", "qwen3.6-plus", "qwen3.5-plus-2026-04-20",
		"kimi-k2.7-code", "kimi-k2.7-code-highspeed", "kimi-k2.6", "kimi-k2.5",
		"doubao-seed-2-1-pro-260628", "doubao-seed-2-1-turbo-260628",
		"doubao-seed-2-0-code-preview-260215",
		"glm-5.2", "glm-5.1", "glm-5v-turbo", "glm-5",
	}
	// The reference's fallback rows carry name == id and contextWindow 131072.
	vision := map[string]bool{"glm-5v-turbo": true}
	out := make([]lobsteraiModel, 0, len(ids))
	for _, id := range ids {
		out = append(out, lobsteraiModel{
			ID:            id,
			Name:          id,
			ContextLength: 131072,
			Tools:         true,
			Vision:        vision[id],
		})
	}
	return out
}

// timeNowUnix is the single clock source for the store and identity resets.
func timeNowUnix() int64 { return time.Now().Unix() }

// sha256Sum returns the hex digest of secret, used for fingerprints only.
func sha256Sum(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
