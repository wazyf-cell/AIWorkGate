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
}

func loadConfig() pluginConfig {
	return pluginConfig{
		ID:       env("AIWORKGATE_PLUGIN_ID", "loomy"),
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
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

// defaultDataDir is used only when the host did not inject one, so a manual run
// still has somewhere to keep its sealed credentials.
func defaultDataDir() string {
	if base := os.Getenv("APPDATA"); base != "" {
		sep := string(os.PathSeparator)
		return base + sep + "AIWorkGate" + sep + "plugin-data" + sep + "loomy"
	}
	return "loomy-data"
}

func main() {
	cfg := loadConfig()
	st, err := newStore(cfg.DataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loomy:", err)
		os.Exit(1)
	}
	srv := &server{cfg: cfg, store: st, client: newUpstreamClient(), signing: signingFromEnv(), logins: newLoginManager()}

	// Bind loopback only: the contract forbids listening on 0.0.0.0.
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
		fmt.Fprintln(os.Stderr, "loomy: listen:", err)
		os.Exit(1)
	}
}

// fallbackModels is the static catalog served when the remote one is
// unreachable. Ported from LOOMY_FALLBACK_MODELS (loomy-product.ts): the ids,
// names and context windows are the reference's, not invented.
//
// The reference's fallback deliberately drops image support ("兜底表不声明图片
// 能力：宁可少报"), which is why vision is absent here.
func fallbackModels() []loomyModel {
	return []loomyModel{
		{ID: "deepseek-v4-flash-0731", Name: "DeepSeek V4 Flash 0731 · x3.0", ContextLength: 1048576, Tools: true, Thinking: loomyEfforts},
		{ID: "MiniMax-M3", Name: "MiniMax M3 · x4.0", ContextLength: 1048576, Tools: true, Thinking: loomyEfforts},
		{ID: "Kimi-k2.6", Name: "Kimi k2.6 · x6.5", ContextLength: 262144, Tools: true, Thinking: loomyEfforts},
		{ID: "qwen-3.8-max", Name: "Qwen 3.8 Max · x12.0", ContextLength: 1000000, Tools: true, Thinking: loomyEfforts},
		{ID: "GLM-5.3-Flash", Name: "GLM 5.3 Flash · x0.8", ContextLength: 1048576, Tools: true, Thinking: loomyEfforts},
		{ID: "qwen3.8-flash", Name: "qwen 3.8 flash · x0.8", ContextLength: 1000000, Tools: true, Thinking: loomyEfforts},
		{ID: "spark-x", Name: "Spark X2.5 · x0.1", ContextLength: 1048576, Tools: true, Thinking: loomyEfforts},
		{ID: "mimo-v2.5", Name: "MiMo V2.5 · x3.3", ContextLength: 1048576, Tools: true, Thinking: loomyEfforts},
	}
}

// loomyEfforts is LOOMY_EFFORTS from loomy-product.ts.
var loomyEfforts = []string{"none", "low", "medium", "high", "xhigh"}

// sha256Sum returns the hex digest of secret, used for fingerprints only.
func sha256Sum(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
