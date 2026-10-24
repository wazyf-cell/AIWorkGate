package logging

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func fixedNow() time.Time {
	return time.Date(2026, 10, 21, 12, 0, 0, 0, time.Local)
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	text := strings.TrimSuffix(string(b), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func TestLoggerWritesJSONL(t *testing.T) {
	dir := testTempDir(t)
	l, err := NewLogger(dir)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	l.now = fixedNow

	l.Info("gateway", "started", map[string]any{"port": 7865})
	l.Warn("plugins", "slow start", map[string]any{"plugin": "mock"})
	l.Error("egress", "dial failed", map[string]any{"err": "timeout"})
	// Reserved keys cannot be overwritten by fields.
	if err := l.Log(LevelInfo, "kv", "ping", map[string]any{"level": "bogus", "msg": "bogus"}); err != nil {
		t.Fatalf("Log: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	path := filepath.Join(dir, "logs", "host-2026-10-21.jsonl")
	lines := readLines(t, path)
	if len(lines) != 4 {
		t.Fatalf("lines = %d, want 4", len(lines))
	}

	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if first["level"] != "info" || first["module"] != "gateway" || first["msg"] != "started" {
		t.Fatalf("entry = %+v", first)
	}
	if port, ok := first["port"].(float64); !ok || port != 7865 {
		t.Fatalf("port field = %v", first["port"])
	}
	ts, _ := first["ts"].(string)
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil || !parsed.Equal(fixedNow()) {
		t.Fatalf("ts = %q (%v)", ts, err)
	}

	var fourth map[string]any
	if err := json.Unmarshal([]byte(lines[3]), &fourth); err != nil {
		t.Fatalf("unmarshal 4: %v", err)
	}
	if fourth["level"] != "info" || fourth["msg"] != "ping" {
		t.Fatalf("reserved fields overridden: %+v", fourth)
	}
}

func TestLoggerRotatesAtMaxSize(t *testing.T) {
	dir := testTempDir(t)
	l, err := NewLogger(dir)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	l.now = fixedNow
	l.file.maxSize = 400

	for i := 0; i < 20; i++ {
		l.Info("rotation", "a fairly long line so the file grows quickly", map[string]any{"i": i})
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	base := filepath.Join(dir, "logs", "host-2026-10-21.jsonl")
	st, err := os.Stat(base + ".1")
	if err != nil {
		t.Fatalf("rotated file missing: %v", err)
	}
	if st.Size() < 400 {
		t.Fatalf("rotated size = %d, want >= 400", st.Size())
	}
	cur, err := os.Stat(base)
	if err != nil {
		t.Fatalf("current file missing: %v", err)
	}
	if cur.Size() == 0 {
		t.Fatalf("current file empty after rotation")
	}
}

func TestAPILoggerKeepsPreviewWhenDebugOff(t *testing.T) {
	dataDir := testTempDir(t)
	st := openTestStore(t)
	a, err := NewAPILogger(st, dataDir, false)
	if err != nil {
		t.Fatalf("NewAPILogger: %v", err)
	}
	a.now = fixedNow
	if a.Debug() {
		t.Fatalf("debug should be off")
	}

	long := strings.Repeat("p", BodyPreviewLimit+500)
	if err := a.Write(apiRec(`{"model":"m","api_key":"sk-preview"}`, long)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rows, n, err := st.QueryAPIRequestLogs(storeFilter())
	if err != nil || n != 1 {
		t.Fatalf("rows=%d err=%v", n, err)
	}
	// D97: with Debug off only a redacted 1KB preview is kept.
	if len(rows[0].BodyReq) > BodyPreviewLimit || len(rows[0].BodyResp) > BodyPreviewLimit {
		t.Fatalf("preview too long: %d / %d", len(rows[0].BodyReq), len(rows[0].BodyResp))
	}
	if strings.Contains(rows[0].BodyReq, "sk-preview") {
		t.Fatalf("preview leaked the API key: %q", rows[0].BodyReq)
	}

	lines := readLines(t, filepath.Join(dataDir, "logs", "api-2026-10-21.jsonl"))
	if len(lines) != 1 {
		t.Fatalf("api jsonl lines = %d, want 1", len(lines))
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := entry["body_req"]; !ok {
		t.Fatalf("body_req preview missing from jsonl: %+v", entry)
	}
	if entry["req_id"] != "req-1" || entry["source"] != "plugin:mock" {
		t.Fatalf("entry = %+v", entry)
	}
}

func TestAPILoggerDebugRedactsAndTruncates(t *testing.T) {
	dataDir := testTempDir(t)
	st := openTestStore(t)
	a, err := NewAPILogger(st, dataDir, true)
	if err != nil {
		t.Fatalf("NewAPILogger: %v", err)
	}
	a.now = fixedNow

	body := `{"model":"m","api_key":"sk-live-123","Authorization":"Bearer abc.def","nested":{"cookie":"sid=1"},"max_tokens":100}`
	if err := a.Write(apiRec(body, strings.Repeat("r", 9000))); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// A second row with a body far beyond the request cap.
	long := strings.Repeat("x", 6000)
	if err := a.Write(apiRec(long, "")); err != nil {
		t.Fatalf("Write long: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rows, n, err := st.QueryAPIRequestLogs(storeFilter())
	if err != nil || n != 2 {
		t.Fatalf("rows=%d err=%v", n, err)
	}
	// Newest first: the long row was written last.
	got := rows[0].BodyReq
	if len(got) > BodyReqLimit {
		t.Fatalf("request body len = %d, want <= %d", len(got), BodyReqLimit)
	}
	if rows[1].BodyReq == "" || rows[1].BodyResp == "" {
		t.Fatalf("bodies missing with debug on")
	}
	if len(rows[1].BodyResp) > BodyRespLimit {
		t.Fatalf("response body len = %d, want <= %d", len(rows[1].BodyResp), BodyRespLimit)
	}
	redacted := rows[1].BodyReq
	for _, secret := range []string{"sk-live-123", "abc.def", "sid=1"} {
		if strings.Contains(redacted, secret) {
			t.Fatalf("secret %q leaked in %q", secret, redacted)
		}
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(redacted), &parsed); err != nil {
		t.Fatalf("redacted body is not JSON: %v (%q)", err, redacted)
	}
	if parsed["api_key"] != "***" || parsed["Authorization"] != "***" {
		t.Fatalf("keys not redacted: %+v", parsed)
	}
	nested, _ := parsed["nested"].(map[string]any)
	if nested == nil || nested["cookie"] != "***" {
		t.Fatalf("nested cookie not redacted: %+v", parsed)
	}
	if parsed["max_tokens"] != float64(100) {
		t.Fatalf("max_tokens was redacted: %+v", parsed)
	}
}

func TestRedactBody(t *testing.T) {
	in := `{"Authorization":"Bearer top.secret","API_KEY":"abc123","cookie":"sid=xyz","password":"p@ss","note":"Bearer eyJhbGci.xyz"}`
	out := RedactBody(in)
	for _, leak := range []string{"top.secret", "abc123", "sid=xyz", "p@ss", "eyJhbGci"} {
		if strings.Contains(out, leak) {
			t.Fatalf("leak %q in %q", leak, out)
		}
	}
	if strings.Count(out, "***") < 4 {
		t.Fatalf("expected redaction markers in %q", out)
	}

	plain := "Authorization: Bearer tok_12345\nCookie: sid=abc; Path=/\napi_key=key_zzz"
	redacted := RedactBody(plain)
	for _, leak := range []string{"tok_12345", "sid=abc", "key_zzz"} {
		if strings.Contains(redacted, leak) {
			t.Fatalf("plain leak %q in %q", leak, redacted)
		}
	}

	// A Bearer token outside a sensitive key is still collapsed.
	if got := RedactBody("note: Bearer abc.def-123"); got != "note: Bearer ***" {
		t.Fatalf("bearer collapse = %q", got)
	}

	// Non-sensitive token counters survive.
	keep := RedactBody(`{"max_tokens":512,"total_tokens":20,"prompt_tokens":3}`)
	for _, want := range []string{"512", "20", "3"} {
		if !strings.Contains(keep, want) {
			t.Fatalf("lost %q in %q", want, keep)
		}
	}

	if RedactBody("") != "" {
		t.Fatalf("empty body should stay empty")
	}
	// Invalid JSON still gets key/value redaction.
	broken := RedactBody(`{"token": "abc"`)
	if strings.Contains(broken, "abc") {
		t.Fatalf("broken json leaked: %q", broken)
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"", 10, ""},
		{"abc", 0, ""},
		{"abc", 10, "abc"},
		{"abcdef", 3, "abc"},
	}
	for _, c := range cases {
		if got := Truncate(c.in, c.n); got != c.want {
			t.Fatalf("Truncate(%q,%d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
	// Multi-byte boundary: "中文" is 6 bytes, cutting at 4 must yield "中".
	got := Truncate("中文测试", 4)
	if got != "中" {
		t.Fatalf("Truncate utf8 = %q, want 中", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("invalid utf8 result")
	}
}
