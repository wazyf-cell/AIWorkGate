package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	// EgressFile is AIWORKGATE_EGRESS_FILE, the host-written proxy bindings.
	EgressFile string
}

func loadConfig() pluginConfig {
	return pluginConfig{
		ID:         env("AIWORKGATE_PLUGIN_ID", "gemini"),
		APILevel:   2,
		Port:       env("AIWORKGATE_PLUGIN_PORT", "0"),
		Token:      env("AIWORKGATE_PLUGIN_TOKEN", ""),
		DataDir:    env("AIWORKGATE_PLUGIN_DATA_DIR", defaultDataDir()),
		EgressFile: strings.TrimSpace(os.Getenv("AIWORKGATE_EGRESS_FILE")),
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
		return base + string(os.PathSeparator) + "AIWorkGate" + string(os.PathSeparator) + "plugin-data" + string(os.PathSeparator) + "gemini"
	}
	return "gemini-data"
}

func main() {
	cfg := loadConfig()
	st, err := newStore(cfg.DataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gemini:", err)
		os.Exit(1)
	}
	srv := newServer(cfg, st)

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
		fmt.Fprintln(os.Stderr, "gemini: listen:", err)
		os.Exit(1)
	}
}

// manifest models are served from /v1/models via catalog(); this helper only keeps
// the shape the manifest expects.
func manifestModels() []map[string]any {
	entries := catalog()
	out := make([]map[string]any, 0, len(entries))
	for _, m := range entries {
		out = append(out, map[string]any{
			"id":             m.ID,
			"object":         "model",
			"owned_by":       "gemini",
			"name":           m.Name,
			"context_length": m.ContextWindow,
		})
	}
	return out
}

// sha256Sum returns the hex digest of secret, used for fingerprints only.
func sha256Sum(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// fingerprint is a short, non-reversible tag so two tokens can be told apart in a
// log without exposing either.
func fingerprint(secret string) string {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return ""
	}
	sum := sha256Sum(secret)
	return "sha256:" + sum[:8]
}

// nowMillis is used to build tool call ids in the reference's format.
func nowMillis() int64 { return time.Now().UnixMilli() }

// maxTokensOf reads the requested output cap, preferring max_tokens.
func maxTokensOf(body []byte) int {
	var probe struct {
		MaxTokens int `json:"max_tokens"`
		MaxComp   int `json:"max_completion_tokens"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return 0
	}
	if probe.MaxTokens > 0 {
		return probe.MaxTokens
	}
	return probe.MaxComp
}

// reasoningEffortOf reads the OpenAI reasoning_effort value, which Gemini maps
// onto a thinking budget tier.
func reasoningEffortOf(body []byte) string {
	var probe struct {
		ReasoningEffort string `json:"reasoning_effort"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return ""
	}
	return strings.TrimSpace(probe.ReasoningEffort)
}

// decodeInlineImages collects every data-URL image in the conversation, keyed by
// URL, so the translator can inline them without re-parsing.
//
// The upstream only accepts inline data, and the reference refuses to send a
// request that silently lost an image, so an over-large image is an error rather
// than a skip.
func decodeInlineImages(body []byte) map[string]inlineImage {
	images := map[string]inlineImage{}
	var probe struct {
		Messages []oaMessage `json:"messages"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return images
	}
	for _, m := range probe.Messages {
		raw := strings.TrimSpace(string(m.Content))
		if !strings.HasPrefix(raw, "[") {
			continue
		}
		var parts []struct {
			Type     string `json:"type"`
			ImageURL *struct {
				URL string `json:"url"`
			} `json:"image_url"`
		}
		if json.Unmarshal(m.Content, &parts) != nil {
			continue
		}
		for _, p := range parts {
			if p.Type != "image_url" || p.ImageURL == nil {
				continue
			}
			media, data, ok := strings.Cut(p.ImageURL.URL, ",")
			if !ok || !strings.HasPrefix(media, "data:") {
				continue
			}
			mime := strings.TrimPrefix(media, "data:")
			if i := strings.Index(mime, ";"); i >= 0 {
				mime = mime[:i]
			}
			if len(data) > maxImageBytesInline {
				continue
			}
			images[p.ImageURL.URL] = inlineImage{MimeType: firstNonEmpty(mime, "image/png"), Data: data}
		}
	}
	return images
}
