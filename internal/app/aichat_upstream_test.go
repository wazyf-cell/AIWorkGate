package app

// Tests for the Q18 one-time credential handover to the aichat plugin. The wire
// shape of SetUpstream is pinned in internal/plugins/client_test.go; here the
// push decision is pinned: which values are read, when a push is skipped, what a
// failure does (warn, never a startup failure) and that no host log line ever
// contains the key.

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aiworkgate/internal/logging"
	"aiworkgate/internal/model"
	"aiworkgate/internal/secrets"
)

// assistantTestKey is a fake credential. The real A key is never read by a test.
const assistantTestKey = "sk-SMOKE-T3-ASSISTANT-KEY"

// fakeAIChatPlugin stands in for *plugins.Manager and records what was pushed.
type fakeAIChatPlugin struct {
	installed bool
	enabled   bool
	// failStatus, when non-zero, makes every push fail with failMessage.
	failStatus  int
	failMessage string

	mu     sync.Mutex
	calls  int
	bases  []string
	keys   []string
	pusher func(baseURL, apiKey string) error
}

func (f *fakeAIChatPlugin) Get(id string) (model.PluginStatus, bool) {
	if id != aichatPluginID {
		return model.PluginStatus{}, false
	}
	return model.PluginStatus{Enabled: f.enabled}, f.installed
}

func (f *fakeAIChatPlugin) SetUpstream(_ context.Context, id, baseURL, apiKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.bases = append(f.bases, baseURL)
	f.keys = append(f.keys, apiKey)
	if id != aichatPluginID {
		return errors.New("wrong plugin id")
	}
	if f.pusher != nil {
		return f.pusher(baseURL, apiKey)
	}
	if f.failStatus != 0 {
		return errors.New(f.failMessage)
	}
	return nil
}
func (f *fakeAIChatPlugin) snapshot() (int, []string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, append([]string(nil), f.bases...), append([]string(nil), f.keys...)
}

// newAIChatPushApp builds an App with a real logger and a real DPAPI secret store
// in one temporary directory, A's base URL resolved, and the given stand-in.
func newAIChatPushApp(t *testing.T, plugin aichatUpstreamPusher, apiKey string) (*App, string) {
	t.Helper()
	dir := t.TempDir()
	sec, err := secrets.Open(dir)
	if err != nil {
		t.Fatalf("secrets.Open: %v", err)
	}
	t.Cleanup(func() { _ = sec.Close() })
	if apiKey != "" {
		if err := sec.SetAAPIKey(apiKey); err != nil {
			t.Fatalf("SetAAPIKey: %v", err)
		}
	}
	logger, err := logging.NewLogger(dir)
	if err != nil {
		t.Fatalf("logging.NewLogger: %v", err)
	}
	t.Cleanup(func() { _ = logger.Close() })

	previous := aichatUpstreamSource
	aichatUpstreamSource = func(*App) aichatUpstreamPusher { return plugin }
	t.Cleanup(func() { aichatUpstreamSource = previous })

	const aURL = "http://127.0.0.1:17864"
	return &App{secrets: sec, logger: logger, aBaseURL: aURL, aURL: aURL}, filepath.Join(dir, "logs")
}

// hostLogTail reads every line the host logger has written so far.
func hostLogTail(t *testing.T, logsDir string) string {
	t.Helper()
	entries, err := os.ReadDir(logsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read logs dir: %v", err)
	}
	var out strings.Builder
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(logsDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		out.Write(data)
	}
	return out.String()
}

// TestAIChatUpstreamPushSendsBaseURLAndKey: the host hands A's resolved base URL
// and exact key to the plugin once and logs only the fingerprint.
func TestAIChatUpstreamPushSendsBaseURLAndKey(t *testing.T) {
	plugin := &fakeAIChatPlugin{installed: true, enabled: true}
	app, logsDir := newAIChatPushApp(t, plugin, assistantTestKey)

	if err := app.pushAUpstreamToAIChat(context.Background()); err != nil {
		t.Fatalf("pushAUpstreamToAIChat: %v", err)
	}
	calls, bases, keys := plugin.snapshot()
	if calls != 1 || bases[0] != "http://127.0.0.1:17864" || keys[0] != assistantTestKey {
		t.Fatalf("pushed calls=%d base=%q key_present=%v", calls, bases, keys[0] != "")
	}

	logText := hostLogTail(t, logsDir)
	if strings.Contains(logText, assistantTestKey) {
		t.Fatalf("host log leaks the plaintext key:\n%s", logText)
	}
	fp := secretFingerprint(assistantTestKey)
	if strings.Count(logText, fp) != 1 {
		t.Fatalf("host log mentions the fingerprint %d times, want 1:\n%s", strings.Count(logText, fp), logText)
	}
	if !strings.Contains(logText, "http://127.0.0.1:17864") {
		t.Fatalf("host log lacks the pushed base URL:\n%s", logText)
	}
}

// TestAIChatUpstreamPushFailureOnlyWarns: a failing plugin returns an error, is
// logged at warn level and never panics or aborts the host.
func TestAIChatUpstreamPushFailureOnlyWarns(t *testing.T) {
	plugin := &fakeAIChatPlugin{installed: true, enabled: true,
		failStatus: http.StatusInternalServerError, failMessage: "sealing is unavailable"}
	app, logsDir := newAIChatPushApp(t, plugin, assistantTestKey)

	if err := app.pushAUpstreamToAIChat(context.Background()); err == nil {
		t.Fatal("push returned no error for a failing plugin")
	}
	logText := hostLogTail(t, logsDir)
	if !strings.Contains(logText, `"level":"warn"`) || !strings.Contains(logText, "cannot hand A credentials") {
		t.Fatalf("host log lacks the warn handover failure:\n%s", logText)
	}
	if strings.Contains(logText, assistantTestKey) {
		t.Fatalf("host log leaks the plaintext key:\n%s", logText)
	}
	if !strings.Contains(logText, secretFingerprint(assistantTestKey)) {
		t.Fatalf("host log lacks the key fingerprint:\n%s", logText)
	}
}

// TestAIChatUpstreamPushRedactsEchoedKey: a plugin that echoes the credential in
// its error message must not get it into the host log.
func TestAIChatUpstreamPushRedactsEchoedKey(t *testing.T) {
	plugin := &fakeAIChatPlugin{installed: true, enabled: true, failStatus: http.StatusBadRequest,
		failMessage: "cannot store key " + assistantTestKey + ": file is read-only"}
	app, logsDir := newAIChatPushApp(t, plugin, assistantTestKey)

	if err := app.pushAUpstreamToAIChat(context.Background()); err == nil {
		t.Fatal("push returned no error for a 400 answer")
	}
	logText := hostLogTail(t, logsDir)
	if strings.Contains(logText, assistantTestKey) {
		t.Fatalf("host log leaks the echoed key:\n%s", logText)
	}
	// The JSON encoder HTML-escapes < >, so the marker reads \u003credacted\u003e.
	if !strings.Contains(logText, "redacted") || !strings.Contains(logText, "file is read-only") {
		t.Fatalf("host log lost the marker or the useful error text:\n%s", logText)
	}
}

// TestAIChatUpstreamPushIsIdempotent: two pushes of the same pair both succeed,
// so a host restart never has to know whether the migration already landed.
func TestAIChatUpstreamPushIsIdempotent(t *testing.T) {
	plugin := &fakeAIChatPlugin{installed: true, enabled: true}
	app, _ := newAIChatPushApp(t, plugin, assistantTestKey)

	for i := 0; i < 2; i++ {
		if err := app.pushAUpstreamToAIChat(context.Background()); err != nil {
			t.Fatalf("push %d: %v", i+1, err)
		}
	}
	calls, bases, keys := plugin.snapshot()
	if calls != 2 || bases[0] != bases[1] || keys[0] != keys[1] {
		t.Fatalf("calls = %d, bases = %v; want two identical pushes", calls, bases)
	}
}

// TestAIChatUpstreamPushSkippedWithoutAKey pins the most dangerous case: with no
// A key nothing is pushed, because an empty api_key would overwrite the
// credential the plugin already holds.
func TestAIChatUpstreamPushSkippedWithoutAKey(t *testing.T) {
	plugin := &fakeAIChatPlugin{installed: true, enabled: true}
	app, logsDir := newAIChatPushApp(t, plugin, "") // no A key configured

	if err := app.pushAUpstreamToAIChat(context.Background()); err != nil {
		t.Fatalf("a skipped push must not be an error: %v", err)
	}
	if calls, bases, _ := plugin.snapshot(); calls != 0 || len(bases) != 0 {
		t.Fatalf("plugin was called %d times with %v; must not push without a key", calls, bases)
	}
	if logText := hostLogTail(t, logsDir); !strings.Contains(logText, "api key is not configured") {
		t.Fatalf("skip reason not logged:\n%s", logText)
	}
}

// TestAIChatUpstreamPushSkipsAbsentOrDisabledPlugin: an uninstalled or disabled
// aichat plugin is a no-op, not a startup error.
func TestAIChatUpstreamPushSkipsAbsentOrDisabledPlugin(t *testing.T) {
	cases := map[string]*fakeAIChatPlugin{
		"not installed": {installed: false, enabled: true},
		"disabled":      {installed: true, enabled: false},
	}
	for name, plugin := range cases {
		t.Run(name, func(t *testing.T) {
			app, _ := newAIChatPushApp(t, plugin, assistantTestKey)
			if err := app.pushAUpstreamToAIChat(context.Background()); err != nil {
				t.Fatalf("a skipped push must not be an error: %v", err)
			}
			if calls, _, _ := plugin.snapshot(); calls != 0 {
				t.Fatalf("plugin was called %d times, want 0", calls)
			}
		})
	}
}

// TestStartAIChatUpstreamPushNeverBlocksOrPanics: the entry point runs in the
// background with a recover boundary, so a panicking pusher cannot take the host
// down and the call itself returns at once.
func TestStartAIChatUpstreamPushNeverBlocksOrPanics(t *testing.T) {
	plugin := &fakeAIChatPlugin{installed: true, enabled: true}
	plugin.pusher = func(string, string) error { panic("pusher exploded") }
	app, logsDir := newAIChatPushApp(t, plugin, assistantTestKey)
	app.startAIChatUpstreamPush() // must not wait for the push

	found := false
	for i := 0; i < 100 && !found; i++ {
		found = strings.Contains(hostLogTail(t, logsDir), "aichatUpstreamPush")
		if !found {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !found {
		t.Fatal("the recovered panic was not logged")
	}
	logText := hostLogTail(t, logsDir)
	if !strings.Contains(logText, `"level":"error"`) || strings.Contains(logText, assistantTestKey) {
		t.Fatalf("host log lacks the panic error or leaks the key:\n%s", logText)
	}
}

// TestSecretFingerprintIsShortAndStable pins the log-safe tag; TestRedact is
// covered in the same function because both guard the same invariant.
func TestSecretFingerprintIsShortAndStable(t *testing.T) {
	fp := secretFingerprint(assistantTestKey)
	if fp != secretFingerprint("  "+assistantTestKey+"  ") {
		t.Fatal("fingerprint must ignore surrounding whitespace")
	}
	if fp == secretFingerprint("sk-SMOKE-OTHER") {
		t.Fatal("different keys must have different fingerprints")
	}
	if !strings.HasPrefix(fp, "sha256:") || len(fp) != len("sha256:")+8 {
		t.Fatalf("fingerprint = %q, want sha256:<8 hex>", fp)
	}
	if strings.Contains(fp, "SMOKE") || secretFingerprint("") != "" {
		t.Fatalf("fingerprint %q is not log-safe, or an empty key is not empty", fp)
	}

	// redactSecret keeps an echoed credential out of the host log.
	got := redactSecret("plugin says the key "+assistantTestKey+" is invalid", assistantTestKey)
	if strings.Contains(got, assistantTestKey) || !strings.Contains(got, "<redacted>") {
		t.Fatalf("redacted text = %q", got)
	}
	if got := redactSecret("nothing to hide", ""); got != "nothing to hide" {
		t.Fatalf("an empty secret changed the text: %q", got)
	}
}
