package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"aiworkgate/internal/logging"
	"aiworkgate/internal/model"
	"aiworkgate/internal/store"
	"aiworkgate/internal/webui"
)

func insertTestProxyRow(t *testing.T, st *store.Store, account, result, host string, at time.Time) int64 {
	t.Helper()
	id, err := st.InsertProxyLog(model.ProxyLog{
		TS: at.Format(time.RFC3339), At: at.Unix(),
		PluginID: "trae", AccountID: account, ProxyID: "px_1", ProxyLabel: "出口A",
		TargetHost: host, TargetPort: 443, Result: result, Status: 200,
		DurationMS: 15, BytesUp: 321, BytesDown: 654,
		Error: map[string]string{model.ProxyResultFailed: "dial tcp: refused"}[result],
	})
	if err != nil {
		t.Fatalf("InsertProxyLog: %v", err)
	}
	return id
}

func TestProxyLogsPageFiltersAndBounds(t *testing.T) {
	app, st := openAppWithStore(t)
	now := time.Now()
	insertTestProxyRow(t, st, "acc_1", model.ProxyResultOK, "api.one.com", now)
	insertTestProxyRow(t, st, "acc_2", model.ProxyResultOK, "api.two.com", now.Add(-time.Minute))
	insertTestProxyRow(t, st, "acc_2", model.ProxyResultFailed, "api.two.com", now.Add(-2*time.Minute))

	page, err := app.proxyLogs(map[string][]string{})
	if err != nil {
		t.Fatalf("proxyLogs: %v", err)
	}
	body := page.(map[string]any)
	if body["total"] != 3 || len(body["items"].([]model.ProxyLog)) != 3 {
		t.Fatalf("page = %+v", body)
	}

	failed, err := app.proxyLogs(map[string][]string{"result": {"failed"}})
	if err != nil {
		t.Fatalf("result filter: %v", err)
	}
	rows := failed.(map[string]any)["items"].([]model.ProxyLog)
	if len(rows) != 1 || rows[0].Error == "" {
		t.Fatalf("failed rows = %+v", rows)
	}

	byAccount, err := app.proxyLogs(map[string][]string{"account": {"acc_1"}})
	if err != nil {
		t.Fatalf("account filter: %v", err)
	}
	if total := byAccount.(map[string]any)["total"]; total != 1 {
		t.Fatalf("account total = %v, want 1", total)
	}

	search, _ := app.proxyLogs(map[string][]string{"q": {"ONE.COM"}})
	if total := search.(map[string]any)["total"]; total != 1 {
		t.Fatalf("query total = %v, want 1", total)
	}

	second, err := app.proxyLogs(map[string][]string{"page": {"2"}, "page_size": {"1"}})
	if err != nil {
		t.Fatalf("paging: %v", err)
	}
	body = second.(map[string]any)
	if len(body["items"].([]model.ProxyLog)) != 1 || body["page"] != 2 {
		t.Fatalf("page 2 = %+v", body)
	}

	if _, err := app.proxyLogs(map[string][]string{"result": {"weird"}}); !errors.Is(err, webui.ErrInvalidRequest) {
		t.Fatalf("bad result error = %v, want ErrInvalidRequest", err)
	}
}

func TestTimelineMergesProxyAndConnectivity(t *testing.T) {
	app, st := openAppWithStore(t)
	app.dataDir = t.TempDir()
	app.timeline = logging.NewTimeline(st, app.dataDir, nil)

	now := time.Now()
	insertTestProxyRow(t, st, "acc_1", model.ProxyResultOK, "api.one.com", now)
	if _, err := st.InsertConnectivityProbe(store.ConnectivityProbe{
		Kind: "model", Target: "插件模型", TargetID: "mock/m1", OK: false,
		ErrorCode: "unreachable", Detail: "connect: refused", CreatedAt: now.Unix(),
	}); err != nil {
		t.Fatalf("insert probe: %v", err)
	}

	page, err := app.timeline.Query(logging.TimelineFilter{
		FromDay: now.Format("2006-01-02"), ToDay: now.Format("2006-01-02"),
	}, "", 100)
	if err != nil {
		t.Fatalf("timeline query: %v", err)
	}
	types := map[string]string{}
	for _, item := range page.Items {
		types[item.Type] = item.Summary
	}
	if _, ok := types[logging.TimelineProxy]; !ok {
		t.Fatalf("proxy source missing from timeline: %+v", page.Items)
	}
	if _, ok := types[logging.TimelineConnectivity]; !ok {
		t.Fatalf("connectivity source missing from timeline: %+v", page.Items)
	}
	if !strings.Contains(types[logging.TimelineProxy], "api.one.com") {
		t.Fatalf("proxy summary = %q", types[logging.TimelineProxy])
	}

	// One type must be selectable on its own.
	only, err := app.timeline.Query(logging.TimelineFilter{
		FromDay: now.Format("2006-01-02"), ToDay: now.Format("2006-01-02"),
		Types: []string{logging.TimelineProxy},
	}, "", 100)
	if err != nil {
		t.Fatalf("proxy-only query: %v", err)
	}
	if len(only.Items) != 1 || only.Items[0].Type != logging.TimelineProxy {
		t.Fatalf("proxy-only items = %+v", only.Items)
	}
	if only.SourceCounts[logging.TimelineProxy] != 1 {
		t.Fatalf("source counts = %+v", only.SourceCounts)
	}
}

func TestLogRawForEachSource(t *testing.T) {
	app, st := openAppWithStore(t)
	app.dataDir = t.TempDir()
	now := time.Now()
	ts := now.Format(time.RFC3339)

	if err := st.InsertAPIRequestLog(model.APIRequestLog{
		TS: ts, ReqID: "req-raw", Source: "plugin:mock", Model: "m1", Status: 200,
		BodyReq: `{"secret":"must-not-leak"}`, BodyResp: `{"ok":true}`,
	}); err != nil {
		t.Fatalf("insert api log: %v", err)
	}
	auditID, err := st.AppendAudit(model.AuditEntry{TS: ts, Category: "account",
		Action: "delete", Target: "mock/acc_1", Result: "fail"})
	if err != nil {
		t.Fatalf("append audit: %v", err)
	}
	proxyID := insertTestProxyRow(t, st, "acc_1", model.ProxyResultOK, "api.one.com", now)

	t.Run("api row keeps bodies out", func(t *testing.T) {
		raw, err := app.logRaw(map[string][]string{"file": {"api"}, "id": {"req-raw"}})
		if err != nil {
			t.Fatalf("logRaw(api): %v", err)
		}
		if !strings.Contains(raw.Line, "req-raw") {
			t.Fatalf("line = %q", raw.Line)
		}
		if strings.Contains(raw.Line, "must-not-leak") {
			t.Fatalf("raw line leaked the request body: %q", raw.Line)
		}
	})

	t.Run("audit and proxy rows return their own JSON", func(t *testing.T) {
		raw, err := app.logRaw(map[string][]string{"file": {"audit"}, "id": {strconv.FormatInt(auditID, 10)}})
		if err != nil {
			t.Fatalf("logRaw(audit): %v", err)
		}
		var entry model.AuditEntry
		if err := json.Unmarshal([]byte(raw.Line), &entry); err != nil {
			t.Fatalf("audit line is not JSON: %v (%s)", err, raw.Line)
		}
		if entry.Target != "mock/acc_1" || entry.ID != auditID {
			t.Fatalf("audit entry = %+v", entry)
		}

		raw, err = app.logRaw(map[string][]string{"file": {"proxy"}, "id": {strconv.FormatInt(proxyID, 10)}})
		if err != nil {
			t.Fatalf("logRaw(proxy): %v", err)
		}
		if !strings.Contains(raw.Line, "api.one.com") || !strings.Contains(raw.Line, "acc_1") {
			t.Fatalf("proxy line = %q", raw.Line)
		}
	})

	t.Run("host line comes from the day file", func(t *testing.T) {
		dir := filepath.Join(app.dataDir, "logs")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		line := `{"ts":"` + ts + `","level":"info","module":"app","msg":"托盘左键点击：显示窗口"}`
		path := filepath.Join(dir, "host-"+now.Format("2006-01-02")+".jsonl")
		if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
			t.Fatalf("write host log: %v", err)
		}
		raw, err := app.logRaw(map[string][]string{"file": {"host"}, "id": {"app"}, "ts": {ts}})
		if err != nil {
			t.Fatalf("logRaw(host): %v", err)
		}
		if raw.Line != line {
			t.Fatalf("host line = %q, want the literal JSONL line", raw.Line)
		}
	})

	t.Run("plugin line comes from the plugin log", func(t *testing.T) {
		dir := filepath.Join(app.dataDir, "logs", "plugins")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		line := ts + " plugin says hello"
		path := filepath.Join(dir, "mock-"+now.Format("2006-01-02")+".log")
		if err := os.WriteFile(path, []byte("older line\n"+line+"\n"), 0o600); err != nil {
			t.Fatalf("write plugin log: %v", err)
		}
		raw, err := app.logRaw(map[string][]string{"file": {"plugin"}, "id": {"mock"}, "ts": {ts}})
		if err != nil {
			t.Fatalf("logRaw(plugin): %v", err)
		}
		if raw.Line != line {
			t.Fatalf("plugin line = %q, want %q", raw.Line, line)
		}
	})

	t.Run("missing rows and bad input are reported plainly", func(t *testing.T) {
		if _, err := app.logRaw(map[string][]string{"file": {"api"}, "id": {"nope"}}); !errors.Is(err, webui.ErrLogNotFound) {
			t.Fatalf("missing api row = %v, want ErrLogNotFound", err)
		}
		if _, err := app.logRaw(map[string][]string{"file": {"nope"}}); !errors.Is(err, webui.ErrInvalidRequest) {
			t.Fatalf("unknown file = %v, want ErrInvalidRequest", err)
		}
		if _, err := app.logRaw(map[string][]string{"file": {"proxy"}, "id": {"abc"}}); !errors.Is(err, webui.ErrInvalidRequest) {
			t.Fatalf("bad proxy id = %v, want ErrInvalidRequest", err)
		}
		if _, err := app.logRaw(map[string][]string{"file": {"host"}, "id": {"app"}}); !errors.Is(err, webui.ErrInvalidRequest) {
			t.Fatalf("host without ts = %v, want ErrInvalidRequest", err)
		}
	})
}
