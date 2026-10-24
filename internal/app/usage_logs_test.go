package app

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"aiworkgate/internal/config"
	"aiworkgate/internal/logging"
	"aiworkgate/internal/model"
	"aiworkgate/internal/usage"
	"aiworkgate/internal/webui"
)

func newUsageTestApp(t *testing.T) (*App, *model.APIRequestLog) {
	t.Helper()
	app, st := openAppWithStore(t)
	app.dataDir = t.TempDir()
	app.cfg = config.Default()
	app.usageSvc = usage.NewService(st)
	app.usageSvc.Now = func() time.Time {
		return time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local)
	}
	app.timeline = logging.NewTimeline(st, app.dataDir, nil)
	app.timeline.Now = app.usageSvc.Now

	rec := model.APIRequestLog{
		TS:    time.Date(2026, 10, 20, 10, 0, 0, 0, time.Local).Format(time.RFC3339),
		ReqID: "req-body", Source: "plugin:mock", Account: "acc_1", Model: "m1",
		Status: 200, BodyReq: `{"model":"m"}`, BodyResp: `{"ok":true}`,
	}
	if err := st.InsertAPIRequestLog(rec); err != nil {
		t.Fatalf("insert api log: %v", err)
	}
	return app, &rec
}

func TestLogBodyRespectsDebugSwitch(t *testing.T) {
	app, _ := newUsageTestApp(t)

	// Debug off: 403-worthy error and no body access.
	if _, err := app.logBody("req-body", "req"); !errors.Is(err, webui.ErrDebugDisabled) {
		t.Fatalf("debug off error = %v, want ErrDebugDisabled", err)
	}

	app.cfg.Logging.DebugBody = true
	body, err := app.logBody("req-body", "req")
	if err != nil {
		t.Fatalf("logBody(req): %v", err)
	}
	if body.Body != `{"model":"m"}` || body.Truncated {
		t.Fatalf("req body = %+v", body)
	}
	body, err = app.logBody("req-body", "resp")
	if err != nil || body.Body != `{"ok":true}` {
		t.Fatalf("resp body = %+v err=%v", body, err)
	}
	if _, err := app.logBody("missing", "req"); !errors.Is(err, webui.ErrLogNotFound) {
		t.Fatalf("missing req error = %v", err)
	}
}

func TestLogBodyFlagsTruncation(t *testing.T) {
	app, _ := newUsageTestApp(t)
	app.cfg.Logging.DebugBody = true
	long := strings.Repeat("x", logging.BodyReqLimit+10)
	if err := app.store.InsertAPIRequestLog(model.APIRequestLog{
		TS: "2026-10-20T10:01:00Z", ReqID: "req-long", BodyReq: long,
	}); err != nil {
		t.Fatalf("insert long body: %v", err)
	}
	body, err := app.logBody("req-long", "req")
	if err != nil {
		t.Fatalf("logBody: %v", err)
	}
	if !body.Truncated || len(body.Body) != len(long) {
		t.Fatalf("truncated=%v len=%d", body.Truncated, len(body.Body))
	}
}

func TestUsageHandlersParseAndRejectQueries(t *testing.T) {
	app, _ := newUsageTestApp(t)
	if _, err := app.store.DB().Exec(`INSERT INTO usage_daily
		(day, plugin_id, account_id, model, requests, errors, prompt_tokens, completion_tokens,
		 credits, latency_ms_sum, ttfb_ms_sum, ttfb_count)
		VALUES ('2026-10-20', 'plugin:mock', 'acc_1', 'm1', 4, 1, 100, 50, 2.5, 0, 0, 0)`); err != nil {
		t.Fatalf("insert usage: %v", err)
	}

	payload, err := app.usageSummary(map[string][]string{
		"from": {"2026-10-20"}, "to": {"2026-10-20"}, "plugin": {"mock", "other"},
	})
	if err != nil {
		t.Fatalf("usageSummary: %v", err)
	}
	summary, ok := payload.(usage.SummaryResponse)
	if !ok {
		t.Fatalf("payload type = %T", payload)
	}
	if summary.KPIs.Requests != 4 || summary.KPIs.Credits != 2.5 || len(summary.Series) != 1 {
		t.Fatalf("summary = %+v", summary)
	}

	if _, err := app.usageSummary(map[string][]string{"from": {"not-a-day"}}); !errors.Is(err, webui.ErrInvalidRequest) {
		t.Fatalf("bad from error = %v, want ErrInvalidRequest", err)
	}

	page, err := app.logTimeline(map[string][]string{
		"from": {"2026-10-20"}, "to": {"2026-10-20"}, "type": {"api"},
	})
	if err != nil {
		t.Fatalf("logTimeline: %v", err)
	}
	timeline, ok := page.(logging.TimelinePage)
	if !ok || len(timeline.Items) != 1 {
		t.Fatalf("timeline = %#v", page)
	}
	if _, err := app.logTimeline(map[string][]string{"to": {"2026-13-01"}}); !errors.Is(err, webui.ErrInvalidRequest) {
		t.Fatalf("bad timeline day error = %v", err)
	}
}

func TestTimelineCursorWalksEveryAPIEventOnce(t *testing.T) {
	app, _ := newUsageTestApp(t)
	for i := 0; i < 6; i++ {
		rec := model.APIRequestLog{
			TS:    time.Date(2026, 10, 20, 9, i, 0, 0, time.Local).Format(time.RFC3339),
			ReqID: "req-walk-" + string(rune('a'+i)), Source: "plugin:mock",
		}
		if err := app.store.InsertAPIRequestLog(rec); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		query := map[string][]string{
			"from": {"2026-10-20"}, "to": {"2026-10-20"}, "type": {"api"}, "page_size": {"2"},
		}
		if cursor != "" {
			query["cursor"] = []string{cursor}
		}
		payload, err := app.logTimeline(query)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		page, ok := payload.(logging.TimelinePage)
		if !ok {
			t.Fatalf("payload type = %T", payload)
		}
		if len(page.Items) == 0 || len(page.Items) > 2 {
			t.Fatalf("page %d returned %d items", pages, len(page.Items))
		}
		for _, item := range page.Items {
			if seen[item.ReqID] {
				t.Fatalf("event %q returned twice", item.ReqID)
			}
			seen[item.ReqID] = true
		}
		pages++
		if pages > 10 {
			t.Fatal("cursor did not terminate")
		}
		if !page.HasMore {
			break
		}
		if page.NextCursor == "" {
			t.Fatal("has_more without next_cursor")
		}
		cursor = page.NextCursor
	}
	if len(seen) != 7 {
		t.Fatalf("walked %d events, want 7", len(seen))
	}
}

func TestLogTimelineRejectsLegacyPageAndBadCursor(t *testing.T) {
	app, _ := newUsageTestApp(t)
	if _, err := app.logTimeline(map[string][]string{"page": {"2"}}); !errors.Is(err, webui.ErrInvalidRequest) {
		t.Fatalf("page=2 error = %v, want ErrInvalidRequest", err)
	}
	if _, err := app.logTimeline(map[string][]string{"cursor": {"!!!not-base64!!!"}}); !errors.Is(err, webui.ErrInvalidRequest) {
		t.Fatalf("bad cursor error = %v, want ErrInvalidRequest", err)
	}
}

func TestTimelineQueryFilterForwarding(t *testing.T) {
	app, _ := newUsageTestApp(t)
	query := map[string][]string{
		"type": {"api,runtime"}, "q": {"req-body"}, "page_size": {"5"}, "cursor": {"abc"},
	}
	filter, err := app.timelineFilter(query)
	if err != nil {
		t.Fatalf("timelineFilter: %v", err)
	}
	cursor, page, size, err := logging.ParseTimelinePage(url.Values(query))
	if err != nil {
		t.Fatalf("ParseTimelinePage: %v", err)
	}
	if len(filter.Types) != 2 || filter.Query != "req-body" || cursor != "abc" || page != 1 || size != 5 {
		t.Fatalf("filter = %+v cursor=%q page=%d size=%d", filter, cursor, page, size)
	}
	payload, err := app.logTimeline(map[string][]string{"from": {"2026-10-20"}, "to": {"2026-10-20"}})
	if err != nil || payload == nil {
		t.Fatalf("logTimeline = %v, %v", payload, err)
	}
}
