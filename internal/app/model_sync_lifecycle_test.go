package app

import (
	"strings"
	"testing"
	"time"

	"aiworkgate/internal/model"
	"aiworkgate/internal/store"
)

func TestSanitizeSyncErrorDropsSecrets(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // substring that must be gone
	}{
		{
			name: "url query with token",
			in:   `Get "http://127.0.0.1:8787/v1/models?token=sk-secret": dial tcp`,
			want: "sk-secret",
		},
		{
			name: "bearer header",
			in:   "upstream rejected: Authorization: Bearer abc.def.ghi",
			want: "abc.def.ghi",
		},
		{
			name: "url userinfo",
			in:   "fetch https://user:pa55@upstream.local/v1/models failed",
			want: "pa55",
		},
	}
	for _, tc := range cases {
		got := sanitizeSyncError(tc.in)
		if strings.Contains(got, tc.want) {
			t.Fatalf("%s: %q still contains %q", tc.name, got, tc.want)
		}
		if strings.Contains(got, "\n") {
			t.Fatalf("%s: newline survived: %q", tc.name, got)
		}
	}
	// A long message is capped, not stored whole.
	long := strings.Repeat("x", 400)
	if got := sanitizeSyncError(long); len([]rune(got)) > 161 {
		t.Fatalf("length = %d, want <= 161", len([]rune(got)))
	}
	if got := sanitizeSyncError("  "); got != "" {
		t.Fatalf("blank error = %q, want empty", got)
	}
}

func TestModelSyncSummaryAggregatesPools(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	app := &App{store: st}

	for _, id := range []string{"p1", "p2"} {
		if err := st.UpsertPoolSettings(model.PoolSettings{PluginID: id}); err != nil {
			t.Fatalf("UpsertPoolSettings %s: %v", id, err)
		}
	}

	now := time.Now()
	// p1 succeeded once; p2 failed later with two attempts and a pending retry.
	app.recordModelSyncAttempt("p1", now.Add(-2*time.Hour), "")
	app.recordModelSyncAttempt("p2", now.Add(-time.Hour), `Get "http://u:pw@x/v1/models?key=k": refused`)
	retryAt := now.Add(time.Hour).Unix()
	if err := st.SetKV(modelSyncRetryKeyPrefix+"p2", nextRetry("", now.Format("2006-01-02"), now)); err != nil {
		t.Fatalf("SetKV retry: %v", err)
	}

	summary := app.modelSyncSummary()
	if got := summary["last_success_at"].(int64); got != now.Add(-2*time.Hour).Unix() {
		t.Fatalf("last_success_at = %d", got)
	}
	if got := summary["last_attempt_at"].(int64); got != now.Add(-time.Hour).Unix() {
		t.Fatalf("last_attempt_at = %d", got)
	}
	if got := summary["failure_count"].(int); got != 1 {
		t.Fatalf("failure_count = %d, want 1", got)
	}
	if got := summary["next_retry_at"].(int64); got <= now.Unix() || got > retryAt {
		t.Fatalf("next_retry_at = %d, want a fresh future retry", got)
	}
	lastError, _ := summary["last_error"].(string)
	if !strings.Contains(lastError, "refused") || strings.Contains(lastError, "pw") {
		t.Fatalf("last_error = %q, want the sanitized failure", lastError)
	}

	// A later success clears the error and keeps the newest timestamps.
	app.recordModelSyncAttempt("p2", now, "")
	summary = app.modelSyncSummary()
	if got := summary["last_error"].(string); got != "" {
		t.Fatalf("last_error after success = %q, want empty", got)
	}
	if got := summary["last_success_at"].(int64); got != now.Unix() {
		t.Fatalf("last_success_at after success = %d", got)
	}

	// The legacy schedule payload keeps its keys and carries the summary.
	status := app.modelSyncStatus("p1")
	for _, key := range []string{"model_sync_time", "auto_synced_on", "model_sync"} {
		if _, ok := status[key]; !ok {
			t.Fatalf("modelSyncStatus lost %q: %+v", key, status)
		}
	}
	if _, ok := status["model_sync"].(map[string]any); !ok {
		t.Fatalf("model_sync = %T, want object", status["model_sync"])
	}

	// No store means a zeroed summary, never a nil map.
	empty := (&App{}).modelSyncSummary()
	if empty["last_success_at"].(int64) != 0 || empty["running"].(bool) {
		t.Fatalf("empty summary = %+v", empty)
	}
}
