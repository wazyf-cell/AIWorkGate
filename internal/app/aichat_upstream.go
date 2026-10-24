package app

// One-shot credential handover to the aichat plugin (decision Q18, gap R1 in
// docs/_lane-aichat-evidence.md §6.1).
//
// The assistant's api_key lives in secrets.dat (DPAPI, a_api_key). Q18 settled
// who owns it after the migration: the plugin keeps it in its own sealed store
// (DATA_DIR\accounts.dat); the host only hands it over once at startup through
// the plugin's POST /admin/upstream endpoint.
//
// Rejected by Q18, so not implemented here:
//   - passing the key through the plugin environment (that is the same
//     hardcoding with a different address);
//   - letting the plugin read %APPDATA%\AIWorkAssistant\conf\vault.stronghold
//     (an unverified, unauthorised read of another product's credential vault).
//
// The push is best effort: a failure is a warn line and never blocks startup,
// and the key itself is never logged — only its fingerprint.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"aiworkgate/internal/model"
	"aiworkgate/internal/safego"
)

const (
	// aichatPluginID is the plugin that holds A's credentials (Q19 renamed the
	// plugin from the illegal single-letter id "a" to "aichat").
	aichatPluginID = "aichat"

	// aichatUpstreamTimeout bounds the whole push. A cold on-demand plugin is
	// allowed its full 15s supervisor boot window plus the control call; the
	// push runs in the background so this bound never delays host startup.
	aichatUpstreamTimeout = 30 * time.Second
)

// aichatUpstreamPusher is the slice of *plugins.Manager the handover needs. The
// seam keeps app tests from spawning a real plugin process.
type aichatUpstreamPusher interface {
	Get(id string) (model.PluginStatus, bool)
	SetUpstream(ctx context.Context, id, baseURL, apiKey string) error
}

// aichatUpstreamSource resolves the pusher; tests replace it.
var aichatUpstreamSource = func(a *App) aichatUpstreamPusher {
	if a == nil || a.pluginMgr == nil {
		return nil
	}
	return a.pluginMgr
}

// startAIChatUpstreamPush runs the handover in the background with a recover
// boundary, so a misbehaving plugin or a slow cold start cannot fail or stall
// the host (Q18: host startup must not depend on the migration succeeding).
func (a *App) startAIChatUpstreamPush() {
	if a == nil {
		return
	}
	safego.Go(a.logger, "aichatUpstreamPush", func() {
		ctx, cancel := context.WithTimeout(context.Background(), aichatUpstreamTimeout)
		defer cancel()
		// The error is already logged inside; this wrapper only keeps startup
		// independent of it.
		_ = a.pushAUpstreamToAIChat(ctx)
	})
}

// pushAUpstreamToAIChat performs the migration once. It returns an error only
// when a push that should have happened failed; every skip condition is a
// logged no-op, so the caller can treat the return value as informational.
//
// A missing key never results in a push: the plugin stores whatever pair it is
// given, so an empty api_key would overwrite a credential the plugin already
// holds (and the plugin answers /v1/models 503 until it is configured again).
func (a *App) pushAUpstreamToAIChat(ctx context.Context) error {
	if a == nil {
		return errors.New("app: aichat upstream push: host is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	key, ok := a.aichatAPIKey()
	if !ok {
		a.logger.Info("app", "aichat upstream push skipped: A api key is not configured", nil)
		return nil
	}

	baseURL := a.aUpstreamBaseURL()
	if baseURL == "" {
		a.logger.Warn("app", "aichat upstream push skipped: A base URL is not configured", nil)
		return errors.New("app: aichat upstream push: A base URL is not configured")
	}

	mgr := aichatUpstreamSource(a)
	if mgr == nil {
		a.logger.Warn("app", "aichat upstream push skipped: plugin manager is not available", nil)
		return errors.New("app: aichat upstream push: plugin manager is not available")
	}

	status, installed := mgr.Get(aichatPluginID)
	if !installed {
		a.logger.Info("app", "aichat upstream push skipped: plugin is not installed", map[string]any{
			"plugin": aichatPluginID,
		})
		return nil
	}
	if !status.Enabled {
		a.logger.Info("app", "aichat upstream push skipped: plugin is disabled", map[string]any{
			"plugin": aichatPluginID,
		})
		return nil
	}

	fp := secretFingerprint(key)
	if err := mgr.SetUpstream(ctx, aichatPluginID, baseURL, key); err != nil {
		a.logger.Warn("app", "cannot hand A credentials to the aichat plugin", map[string]any{
			"plugin":          aichatPluginID,
			"base_url":        baseURL,
			"key_fingerprint": fp,
			// A plugin error message is untrusted text; make sure a plugin that
			// echoes the credential back cannot put it in the host log.
			"error": redactSecret(err.Error(), key),
		})
		return err
	}

	a.logger.Info("app", "A credentials handed to the aichat plugin", map[string]any{
		"plugin":          aichatPluginID,
		"base_url":        baseURL,
		"key_fingerprint": fp,
	})
	return nil
}

// aichatAPIKey returns A's stored gateway key, if any.
func (a *App) aichatAPIKey() (string, bool) {
	if a == nil || a.secrets == nil {
		return "", false
	}
	key, ok := a.secrets.GetAAPIKey()
	key = strings.TrimSpace(key)
	return key, ok && key != ""
}

// aUpstreamBaseURL is the base URL the host currently dials for A: the live URL
// when port discovery already ran, else the startup snapshot. It does not probe
// A, so the handover never waits on the assistant's health at host startup.
func (a *App) aUpstreamBaseURL() string {
	if a == nil {
		return ""
	}
	a.aMu.Lock()
	defer a.aMu.Unlock()
	if live := strings.TrimRight(strings.TrimSpace(a.aURL), "/"); live != "" {
		return live
	}
	return strings.TrimRight(strings.TrimSpace(a.aBaseURL), "/")
}

// secretFingerprint is the log-safe tag of a credential, in the same shape the
// aichat plugin prints (sha256 of the first four bytes), so the host log and
// the plugin log can be compared without either exposing the key.
func secretFingerprint(secret string) string {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(secret))
	return fmt.Sprintf("sha256:%x", sum[:4])
}

// redactSecret removes an exact credential occurrence from untrusted text.
func redactSecret(text, secret string) string {
	if secret == "" || text == "" {
		return text
	}
	return strings.ReplaceAll(text, secret, "<redacted>")
}
