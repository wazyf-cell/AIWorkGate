package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aiworkgate/internal/secrets"
	"aiworkgate/internal/store"
)

// openKeyTestApp opens the store and secret store in one data directory.
func openKeyTestApp(t *testing.T) (*App, *store.Store, *secrets.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sec, err := secrets.Open(dir)
	if err != nil {
		t.Fatalf("secrets.Open: %v", err)
	}
	return &App{store: st, secrets: sec}, st, sec
}

// TestKeyAdapterVerifyFailsClosed covers the audited fail-open: a hash with no
// enabled store row (deleted row, disabled row, or ListClientKeys error) must
// not authenticate, because its scope/quota would not be enforceable.
func TestKeyAdapterVerifyFailsClosed(t *testing.T) {
	app, st, sec := openKeyTestApp(t)
	adapter := &keyAdapter{app: app}

	plain, id, prefix, hash := secrets.NewClientKey()
	if err := sec.AddClientKey(id, hash); err != nil {
		t.Fatalf("AddClientKey: %v", err)
	}

	// Hash exists, store row does not.
	if _, _, ok := adapter.Verify(plain); ok {
		t.Fatal("orphaned hash verified; it must fail closed")
	}

	// Disabled row fails closed too.
	if err := st.UpsertClientKey(id, "k", prefix, hash, false, 0); err != nil {
		t.Fatalf("UpsertClientKey: %v", err)
	}
	if _, _, ok := adapter.Verify(plain); ok {
		t.Fatal("disabled key verified")
	}

	// Enabled row resolves id and name.
	if err := st.UpsertClientKey(id, "laptop", prefix, hash, true, 0); err != nil {
		t.Fatalf("UpsertClientKey enabled: %v", err)
	}
	gotID, gotName, ok := adapter.Verify(plain)
	if !ok || gotID != id || gotName != "laptop" {
		t.Fatalf("Verify = %q/%q/%v, want %q/laptop/true", gotID, gotName, ok, id)
	}

	// A failing ListClientKeys must also fail closed.
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, _, ok := adapter.Verify(plain); ok {
		t.Fatal("verification succeeded while the store was closed")
	}
}

// TestDeleteKeyRemovesSecretFirst proves the revocation order: even when the
// store delete fails, the secret hash is already gone, so no working key is
// left without its scope/quota row.
func TestDeleteKeyRemovesSecretFirst(t *testing.T) {
	app, st, sec := openKeyTestApp(t)

	plain, id, prefix, hash := secrets.NewClientKey()
	if err := sec.AddClientKey(id, hash); err != nil {
		t.Fatalf("AddClientKey: %v", err)
	}
	if err := st.UpsertClientKey(id, "k", prefix, hash, true, 0); err != nil {
		t.Fatalf("UpsertClientKey: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := app.deleteKey(id); err == nil {
		t.Fatal("deleteKey succeeded although the store was closed")
	}
	if _, ok := sec.VerifyClientKey(plain); ok {
		t.Fatal("secret hash survived a failed row delete; the key still works")
	}
}

// customTestApp stores one enabled custom model with an upstream key.
func customTestApp(t *testing.T, baseURL string) (*App, string) {
	t.Helper()
	app, st, sec := openKeyTestApp(t)
	row, err := st.UpsertCustomModel(store.CustomModel{
		Name: "probe", BaseURL: baseURL, UpstreamModel: "probe-model", Enabled: true,
	})
	if err != nil {
		t.Fatalf("UpsertCustomModel: %v", err)
	}
	const key = "sk-probe-secret"
	if err := sec.SetExtra(store.CustomModelSecretKey(row.ID), key); err != nil {
		t.Fatalf("SetExtra: %v", err)
	}
	return app, row.ID
}

// TestCustomModelProbeRejectsRedirects pins the SSRF fix: the probe must not
// follow a 3xx (which would replay the stored credential elsewhere), and it
// must not echo a response body that could contain credentials.
func TestCustomModelProbeRejectsRedirects(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		_, _ = w.Write([]byte(`{"leak":"sk-probe-secret"}`))
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL+"/steal")
		w.WriteHeader(http.StatusFound)
		_, _ = w.Write([]byte(`moved sk-probe-secret`))
	}))
	defer redirect.Close()

	app, id := customTestApp(t, redirect.URL)
	_, err := app.testCustomModel(id)
	if err == nil {
		t.Fatal("redirecting upstream reported success")
	}
	if hits := targetHits.Load(); hits != 0 {
		t.Fatalf("redirect target was contacted %d times", hits)
	}
	msg := err.Error()
	if !strings.Contains(msg, "302") {
		t.Fatalf("error = %q, want the redirect status", msg)
	}
	for _, leak := range []string{"sk-probe-secret", target.URL, "moved"} {
		if strings.Contains(msg, leak) {
			t.Fatalf("error leaked %q: %q", leak, msg)
		}
	}
}

func TestCustomModelProbeDoesNotEchoErrorBody(t *testing.T) {
	const body = `{"error":{"message":"bad key sk-probe-secret from http://10.0.0.1/secret"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	app, id := customTestApp(t, srv.URL)
	_, err := app.testCustomModel(id)
	if err == nil {
		t.Fatal("500 upstream reported success")
	}
	msg := err.Error()
	if !strings.Contains(msg, "500") {
		t.Fatalf("error = %q, want the upstream status", msg)
	}
	for _, leak := range []string{"sk-probe-secret", "bad key", "10.0.0.1"} {
		if strings.Contains(msg, leak) {
			t.Fatalf("error echoed the response body (%q): %q", leak, msg)
		}
	}
}

func TestCustomModelProbeAcceptsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	app, id := customTestApp(t, srv.URL)
	latency, err := app.testCustomModel(id)
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	if latency < 0 || time.Duration(latency)*time.Millisecond > customModelTestTimeout {
		t.Fatalf("latency = %dms", latency)
	}
}
