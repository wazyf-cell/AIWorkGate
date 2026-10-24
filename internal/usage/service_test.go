package usage

import (
	"bytes"
	"encoding/csv"
	"net/url"
	"strings"
	"testing"
	"time"

	"aiworkgate/internal/model"
	"aiworkgate/internal/store"
)

func insertDaily(t *testing.T, st *store.Store, day, pluginID, accountID, modelName string,
	requests, prompt, completion int64, credits float64) {
	t.Helper()
	if _, err := st.DB().Exec(`INSERT INTO usage_daily
		(day, plugin_id, account_id, model, requests, errors, prompt_tokens, completion_tokens,
		 credits, latency_ms_sum, ttfb_ms_sum, ttfb_count)
		VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?, 0, 0, 0)
		ON CONFLICT(day, plugin_id, account_id, model) DO UPDATE SET
			requests = excluded.requests, prompt_tokens = excluded.prompt_tokens,
			completion_tokens = excluded.completion_tokens, credits = excluded.credits`,
		day, pluginID, accountID, modelName, requests, prompt, completion, credits); err != nil {
		t.Fatalf("insert usage_daily: %v", err)
	}
}

func insertSession(t *testing.T, st *store.Store, ts, source, account, modelName string, credits *float64) {
	t.Helper()
	if err := st.InsertAPIRequestLog(model.APIRequestLog{
		TS: ts, ReqID: "req-" + ts, Source: source, Account: account, Model: modelName,
		LogicalModel: "logical-" + modelName, ConversationID: "conv-1",
		Status: 200, PromptTokens: 7, CompletionTokens: 3, Credits: credits,
		TTFBMS: 12, LatencyMS: 99,
	}); err != nil {
		t.Fatalf("insert api log: %v", err)
	}
}

func TestParseQueryFiltersAndDefaults(t *testing.T) {
	now := fixedNow()
	q, err := ParseQuery(url.Values{}, now)
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}
	if q.FromDay != "2026-10-14" || q.ToDay != "2026-10-20" {
		t.Fatalf("default range = %s..%s, want last 7 days", q.FromDay, q.ToDay)
	}

	values := url.Values{
		"from":      {"2026-09-01"},
		"to":        {"2026-10-01"},
		"plugin":    {"mock,other", "third"},
		"group":     {"grp_1", "grp_2,grp_3"},
		"account":   {"mock:acc_1", "other:acc_2,mock:acc_3"},
		"page":      {"2"},
		"page_size": {"500"},
	}
	q, err = ParseQuery(values, now)
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}
	if q.FromDay != "2026-09-01" || q.ToDay != "2026-10-01" {
		t.Fatalf("range = %s..%s", q.FromDay, q.ToDay)
	}
	if len(q.Filter.Plugins) != 3 || len(q.Filter.Groups) != 3 || len(q.Filter.Accounts) != 3 {
		t.Fatalf("filters = %+v", q.Filter)
	}
	if q.Page != 2 || q.PageSize != MaxPageSize {
		t.Fatalf("paging = %d/%d, want 2/%d", q.Page, q.PageSize, MaxPageSize)
	}

	if _, err := ParseQuery(url.Values{"from": {"not-a-day"}}, now); err == nil {
		t.Fatal("bad from accepted")
	}
	if _, err := ParseQuery(url.Values{"account": {"missing-colon"}}, now); err == nil {
		t.Fatal("bad account accepted")
	}

	// A range beyond 366 days is clamped, not rejected.
	q, err = ParseQuery(url.Values{"from": {"2020-01-01"}, "to": {"2026-10-20"}}, now)
	if err != nil {
		t.Fatalf("ParseQuery long range: %v", err)
	}
	from, _ := time.ParseInLocation("2006-01-02", q.FromDay, time.Local)
	to, _ := time.ParseInLocation("2006-01-02", q.ToDay, time.Local)
	if days := int(to.Sub(from).Hours()/24) + 1; days != MaxRangeDays {
		t.Fatalf("clamped range = %d days, want %d", days, MaxRangeDays)
	}
}

func TestSummaryFilters(t *testing.T) {
	st := openTestStore(t)
	insertDaily(t, st, "2026-10-19", "plugin:mock", "acc_1", "m1", 2, 10, 0, 1)
	insertDaily(t, st, "2026-10-20", "plugin:mock", "acc_2", "m2", 3, 20, 0, 2)
	insertDaily(t, st, "2026-10-20", "plugin:other", "acc_3", "m3", 4, 40, 0, 0)

	if err := st.UpsertAccountMeta(model.AccountMeta{PluginID: "mock", AccountID: "acc_1", GroupID: "grp_1"}); err != nil {
		t.Fatalf("meta acc_1: %v", err)
	}
	if err := st.UpsertAccountMeta(model.AccountMeta{PluginID: "mock", AccountID: "acc_2", GroupID: "grp_2"}); err != nil {
		t.Fatalf("meta acc_2: %v", err)
	}
	if err := st.UpsertAccountMeta(model.AccountMeta{PluginID: "other", AccountID: "acc_3", GroupID: "grp_1"}); err != nil {
		t.Fatalf("meta acc_3: %v", err)
	}

	svc := NewService(st)
	svc.Now = fixedNow

	check := func(name string, values url.Values, wantRequests, wantAccounts int64, wantSeries int) {
		t.Helper()
		q, err := ParseQuery(values, fixedNow())
		if err != nil {
			t.Fatalf("%s: ParseQuery: %v", name, err)
		}
		res, err := svc.Summary(q)
		if err != nil {
			t.Fatalf("%s: Summary: %v", name, err)
		}
		if res.KPIs.Requests != wantRequests {
			t.Fatalf("%s: requests = %d, want %d", name, res.KPIs.Requests, wantRequests)
		}
		if res.KPIs.ActiveAccounts != wantAccounts {
			t.Fatalf("%s: active accounts = %d, want %d", name, res.KPIs.ActiveAccounts, wantAccounts)
		}
		if len(res.Series) != wantSeries {
			t.Fatalf("%s: series rows = %d, want %d (%+v)", name, len(res.Series), wantSeries, res.Series)
		}
	}

	base := url.Values{"from": {"2026-10-19"}, "to": {"2026-10-20"}}
	check("all", base, 9, 3, 3)
	check("plugin", url.Values{"from": {"2026-10-19"}, "to": {"2026-10-20"}, "plugin": {"mock"}}, 5, 2, 2)
	check("group", url.Values{"from": {"2026-10-19"}, "to": {"2026-10-20"}, "group": {"grp_1"}}, 6, 2, 2)
	check("account", url.Values{"from": {"2026-10-19"}, "to": {"2026-10-20"}, "account": {"mock:acc_1"}}, 2, 1, 1)
	check("plugin+group", url.Values{"from": {"2026-10-19"}, "to": {"2026-10-20"},
		"plugin": {"mock"}, "group": {"grp_2"}}, 3, 1, 1)
	check("day", url.Values{"from": {"2026-10-20"}, "to": {"2026-10-20"}}, 7, 2, 2)

	q, _ := ParseQuery(url.Values{"from": {"2026-10-19"}, "to": {"2026-10-20"}}, fixedNow())
	res, err := svc.Summary(q)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if res.KPIs.Credits != 3 || res.KPIs.PromptTokens != 70 {
		t.Fatalf("kpis = %+v", res.KPIs)
	}
}

func TestCreditsAndExpirySorting(t *testing.T) {
	st := openTestStore(t)
	now := fixedNow()
	insertDaily(t, st, "2026-10-20", "plugin:mock", "acc_1", "m1", 3, 0, 0, 1.5)
	insertDaily(t, st, "2026-10-20", "plugin:mock", "acc_2", "m2", 1, 0, 0, 0)

	if err := st.SaveQuotaSnapshot(model.QuotaSnapshot{
		PluginID: "mock", AccountID: "acc_1", Remaining: 90, Total: 100, At: now.Unix() - 100,
	}); err != nil {
		t.Fatalf("snapshot old: %v", err)
	}
	if err := st.SaveQuotaSnapshot(model.QuotaSnapshot{
		PluginID: "mock", AccountID: "acc_1", Remaining: 80, Total: 100,
		ExpiresAt: now.AddDate(0, 0, 3).Unix(), Plan: "Pro", At: now.Unix(),
	}); err != nil {
		t.Fatalf("snapshot acc_1: %v", err)
	}
	if err := st.SaveQuotaSnapshot(model.QuotaSnapshot{
		PluginID: "mock", AccountID: "acc_2", Remaining: 5, Total: 10,
		ExpiresAt: now.AddDate(0, 0, -2).Unix(), Plan: "Free", At: now.Unix(),
	}); err != nil {
		t.Fatalf("snapshot acc_2: %v", err)
	}
	if err := st.SaveQuotaSnapshot(model.QuotaSnapshot{
		PluginID: "mock", AccountID: "acc_3", Remaining: 1, At: now.Unix(),
	}); err != nil {
		t.Fatalf("snapshot acc_3: %v", err)
	}
	if err := st.UpsertAccountMeta(model.AccountMeta{PluginID: "mock", AccountID: "acc_1", Alias: "主号"}); err != nil {
		t.Fatalf("meta: %v", err)
	}
	if err := st.ReplaceAccountCache("mock", []model.Account{{ID: "acc_2", Label: "备用号", Enabled: true}}); err != nil {
		t.Fatalf("cache: %v", err)
	}

	svc := NewService(st)
	svc.Now = fixedNow
	q, _ := ParseQuery(url.Values{"from": {"2026-10-19"}, "to": {"2026-10-20"}}, fixedNow())

	credits, err := svc.Credits(q, false)
	if err != nil {
		t.Fatalf("Credits: %v", err)
	}
	if len(credits.Accounts) != 3 {
		t.Fatalf("accounts = %d, want 3", len(credits.Accounts))
	}
	first := credits.Accounts[0]
	if first.AccountID != "acc_1" || first.Remaining != 80 || first.Plan != "Pro" {
		t.Fatalf("acc_1 row = %+v (latest snapshot not used)", first)
	}
	if first.DisplayName != "主号" || first.DailyCredits != 1.5 {
		t.Fatalf("acc_1 display/daily = %q/%v", first.DisplayName, first.DailyCredits)
	}
	if first.DaysLeft == nil || *first.DaysLeft != 3 {
		t.Fatalf("acc_1 days_left = %v, want 3", first.DaysLeft)
	}
	second := credits.Accounts[1]
	if second.DisplayName != "备用号" {
		t.Fatalf("cache label fallback = %q", second.DisplayName)
	}
	if second.DaysLeft == nil || *second.DaysLeft >= 0 {
		t.Fatalf("past expiry days_left = %v, want negative", second.DaysLeft)
	}
	third := credits.Accounts[2]
	if third.AccountID != "acc_3" || third.DaysLeft != nil || third.DisplayName != "acc_3" {
		t.Fatalf("unknown expiry row = %+v", third)
	}

	expiry, err := svc.Credits(q, true)
	if err != nil {
		t.Fatalf("expiry: %v", err)
	}
	got := []string{expiry.Accounts[0].AccountID, expiry.Accounts[1].AccountID, expiry.Accounts[2].AccountID}
	want := []string{"acc_2", "acc_1", "acc_3"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expiry order = %v, want %v", got, want)
		}
	}
}

func TestSessionsPaginationAndFilters(t *testing.T) {
	st := openTestStore(t)
	credits := 2.0
	insertSession(t, st, "2026-10-19T10:00:00+08:00", "plugin:mock", "acc_1", "m1", &credits)
	insertSession(t, st, "2026-10-20T10:00:00+08:00", "plugin:mock", "acc_1", "m2", nil)
	insertSession(t, st, "2026-10-20T11:00:00+08:00", "plugin:other", "acc_2", "m1", &credits)
	if err := st.UpsertAccountMeta(model.AccountMeta{PluginID: "mock", AccountID: "acc_1", Alias: "主号", GroupID: "grp_1"}); err != nil {
		t.Fatalf("meta: %v", err)
	}

	svc := NewService(st)
	svc.Now = fixedNow
	q, err := ParseQuery(url.Values{
		"from": {"2026-10-19"}, "to": {"2026-10-20"}, "page_size": {"2"},
	}, fixedNow())
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}
	page, err := svc.Sessions(q)
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if page.Total != 3 || page.Page != 1 || page.PageSize != 2 || len(page.Items) != 2 {
		t.Fatalf("page 1 = %+v", page)
	}
	if page.Items[0].AccountName != "acc_2" {
		t.Fatalf("newest item = %+v", page.Items[0])
	}
	if page.Items[1].AccountName != "主号" {
		t.Fatalf("alias not applied: %+v", page.Items[1])
	}
	if page.Items[1].Credits != nil {
		t.Fatalf("unreported credits should be null: %+v", page.Items[1].Credits)
	}
	if page.Items[1].LogicalModel != "logical-m2" || page.Items[1].ConversationID != "conv-1" {
		t.Fatalf("session fields = %+v", page.Items[1])
	}

	q.Page = 2
	page, err = svc.Sessions(q)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("page 2 = %+v err=%v", page, err)
	}

	q, _ = ParseQuery(url.Values{
		"from": {"2026-10-19"}, "to": {"2026-10-20"},
		"plugin": {"mock"}, "model": {"m1"},
	}, fixedNow())
	page, err = svc.Sessions(q)
	if err != nil || page.Total != 1 || page.Items[0].PluginID != "plugin:mock" {
		t.Fatalf("filtered sessions = %+v err=%v", page, err)
	}

	q, _ = ParseQuery(url.Values{
		"from": {"2026-10-19"}, "to": {"2026-10-20"}, "account": {"other:acc_2"},
	}, fixedNow())
	page, err = svc.Sessions(q)
	if err != nil || page.Total != 1 || page.Items[0].AccountID != "acc_2" {
		t.Fatalf("account filter = %+v err=%v", page, err)
	}

	q, _ = ParseQuery(url.Values{
		"from": {"2026-10-19"}, "to": {"2026-10-20"}, "group": {"grp_1"},
	}, fixedNow())
	page, err = svc.Sessions(q)
	if err != nil || page.Total != 2 || page.Items[0].AccountID != "acc_1" {
		t.Fatalf("group filter = %+v err=%v", page, err)
	}
}

func TestExportCSVBlocks(t *testing.T) {
	st := openTestStore(t)
	insertDaily(t, st, "2026-10-20", "plugin:mock", "acc_1", "m1", 2, 5, 6, 1)
	if err := st.SaveQuotaSnapshot(model.QuotaSnapshot{
		PluginID: "mock", AccountID: "acc_1", Remaining: 42, Total: 100,
		ExpiresAt: fixedNow().AddDate(0, 0, 5).Unix(), Plan: "Pro", At: fixedNow().Unix(),
	}); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if err := st.UpsertAccountMeta(model.AccountMeta{PluginID: "mock", AccountID: "acc_1", Alias: "主号"}); err != nil {
		t.Fatalf("meta: %v", err)
	}

	svc := NewService(st)
	svc.Now = fixedNow
	q, _ := ParseQuery(url.Values{"from": {"2026-10-19"}, "to": {"2026-10-20"}}, fixedNow())
	var buf bytes.Buffer
	filename, err := svc.Export(&buf, q)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if filename != "usage-2026-10-19-2026-10-20.csv" {
		t.Fatalf("filename = %q", filename)
	}
	text := buf.String()
	if !strings.HasPrefix(text, "section,day,plugin_id,account_id,display_name,requests,prompt_tokens,completion_tokens,credits,remaining,total,expires_at,plan,daily_credits,days_left\n") {
		t.Fatalf("header missing: %q", text)
	}
	if !strings.Contains(text, "account,,mock,acc_1,主号") {
		t.Fatalf("account block missing: %q", text)
	}
	if !strings.Contains(text, "主号") {
		t.Fatalf("display name missing from CSV: %q", text)
	}

	records, err := csv.NewReader(bytes.NewReader(buf.Bytes())).ReadAll()
	if err != nil {
		t.Fatalf("parse CSV: %v", err)
	}
	// header, one series row, one account row.
	if len(records) != 3 {
		t.Fatalf("records = %d, want 3: %+v", len(records), records)
	}
	if records[0][0] != "section" || records[1][0] != "series" || records[2][0] != "account" {
		t.Fatalf("records = %+v", records)
	}
	if records[1][2] != "plugin:mock" || records[2][3] != "acc_1" || records[2][4] != "主号" {
		t.Fatalf("block content = %+v", records)
	}
}
