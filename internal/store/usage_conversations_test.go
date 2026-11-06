package store

import (
	"strings"
	"testing"
	"time"

	"aiworkgate/internal/model"
)

func creditsPtr(v float64) *float64 { return &v }

func insertConversationLog(t *testing.T, s *Store, rec model.APIRequestLog) {
	t.Helper()
	if err := s.InsertAPIRequestLog(rec); err != nil {
		t.Fatalf("insert %s: %v", rec.ReqID, err)
	}
}

// seedConversationLogs covers the three grouping cases: a conversation shared by
// two providers, a second conversation, and two rows without a conversation id
// that must each become their own group.
func seedConversationLogs(t *testing.T, s *Store) {
	t.Helper()
	rows := []model.APIRequestLog{
		{TS: "2026-01-15T10:00:00Z", ReqID: "r1", Source: "plugin:a", Model: "m1",
			Account: "acc1", ConversationID: "c1", Status: 200, PromptTokens: 10, CompletionTokens: 5,
			Credits: creditsPtr(0.5)},
		{TS: "2026-01-15T10:05:00Z", ReqID: "r2", Source: "plugin:b", Model: "m2",
			LogicalModel: "lm2", Account: "acc2", ConversationID: "c1", Status: 500,
			PromptTokens: 20, CompletionTokens: 10, Credits: creditsPtr(1.0)},
		{TS: "2026-01-15T10:10:00Z", ReqID: "r3", Source: "plugin:a", Model: "m1",
			Account: "acc1", ConversationID: "c2", Status: 200},
		{TS: "2026-01-15T10:20:00Z", ReqID: "r4", Source: "plugin:a", Model: "m1",
			Account: "acc1", Status: 200},
		{TS: "2026-01-15T10:20:00Z", ReqID: "r5", Source: "plugin:a", Model: "m1",
			Account: "acc1", Status: 200},
	}
	for _, rec := range rows {
		insertConversationLog(t, s, rec)
	}
}

func conversationDayFilter() UsageFilter {
	return UsageFilter{FromDay: "2026-01-15", ToDay: "2026-01-15"}
}

func findConversation(t *testing.T, rows []UsageConversation, id string) UsageConversation {
	t.Helper()
	for _, row := range rows {
		if row.ConversationID == id {
			return row
		}
	}
	t.Fatalf("conversation %q missing from %+v", id, rows)
	return UsageConversation{}
}

func hasToken(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func TestUsageConversationsGroupsAndAggregates(t *testing.T) {
	s := openTestStore(t)
	seedConversationLogs(t, s)

	rows, hasMore, err := s.UsageConversations(conversationDayFilter(), "", "", "", 10)
	if err != nil {
		t.Fatalf("UsageConversations: %v", err)
	}
	if hasMore {
		t.Fatalf("hasMore = true with limit 10 over 4 groups")
	}
	if len(rows) != 4 {
		t.Fatalf("groups = %d (%+v), want 4", len(rows), rows)
	}

	c1 := findConversation(t, rows, "c1")
	if c1.Requests != 2 || c1.Errors != 1 {
		t.Fatalf("c1 requests/errors = %d/%d, want 2/1", c1.Requests, c1.Errors)
	}
	if c1.PromptTokens != 30 || c1.CompletionTokens != 15 {
		t.Fatalf("c1 tokens = %d/%d, want 30/15", c1.PromptTokens, c1.CompletionTokens)
	}
	if !c1.CreditsSampled || c1.Credits != 1.5 {
		t.Fatalf("c1 credits = %v (sampled %v), want 1.5", c1.Credits, c1.CreditsSampled)
	}
	if !hasToken(c1.Models, "m1") || !hasToken(c1.Models, "lm2") {
		t.Fatalf("c1 models = %v, want m1 + logical lm2", c1.Models)
	}
	if !hasToken(c1.Plugins, "plugin:a") || !hasToken(c1.Plugins, "plugin:b") {
		t.Fatalf("c1 plugins = %v", c1.Plugins)
	}
	if !hasToken(c1.Accounts, "acc1") || !hasToken(c1.Accounts, "acc2") {
		t.Fatalf("c1 accounts = %v", c1.Accounts)
	}

	c2 := findConversation(t, rows, "c2")
	if c2.CreditsSampled {
		t.Fatalf("c2 has no credit samples, got %v", c2.Credits)
	}

	// Rows without conversation_id must not be merged into one group.
	fallbacks := 0
	for _, row := range rows {
		if strings.HasPrefix(row.ConversationID, "req:") {
			fallbacks++
		}
	}
	if fallbacks != 2 {
		t.Fatalf("fallback groups = %d, want 2 (%+v)", fallbacks, rows)
	}
}

func TestUsageConversationsKeysetPagination(t *testing.T) {
	s := openTestStore(t)
	seedConversationLogs(t, s)

	first, hasMore, err := s.UsageConversations(conversationDayFilter(), "", "", "", 2)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if !hasMore || len(first) != 2 {
		t.Fatalf("page 1 = %d rows, hasMore %v; want 2 / true", len(first), hasMore)
	}
	// Newest first; the two 10:20 rows tie-break on the conversation key DESC.
	if !strings.HasPrefix(first[0].ConversationID, "req:r5:") ||
		!strings.HasPrefix(first[1].ConversationID, "req:r4:") {
		t.Fatalf("page 1 order = %q, %q", first[0].ConversationID, first[1].ConversationID)
	}

	last := first[len(first)-1]
	second, hasMore, err := s.UsageConversations(conversationDayFilter(), "", last.LastTS, last.ConversationID, 2)
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if hasMore || len(second) != 2 {
		t.Fatalf("page 2 = %d rows, hasMore %v; want 2 / false", len(second), hasMore)
	}
	if second[0].ConversationID != "c2" || second[1].ConversationID != "c1" {
		t.Fatalf("page 2 order = %q, %q; want c2, c1", second[0].ConversationID, second[1].ConversationID)
	}
}

func TestUsageConversationsModelFilter(t *testing.T) {
	s := openTestStore(t)
	seedConversationLogs(t, s)

	rows, _, err := s.UsageConversations(conversationDayFilter(), "m2", "", "", 10)
	if err != nil {
		t.Fatalf("UsageConversations: %v", err)
	}
	if len(rows) != 1 || rows[0].ConversationID != "c1" || rows[0].Requests != 1 {
		t.Fatalf("model filter rows = %+v, want only c1 with 1 request", rows)
	}
}

func TestAPILogThreeModelColumnsRoundtrip(t *testing.T) {
	s := openTestStore(t)
	if err := s.InsertAPIRequestLog(model.APIRequestLog{
		TS: "2026-01-15T10:00:00Z", ReqID: "r1", Model: "p1/m1",
		ModelRequested: "p1/m1", ModelUpstream: "m1-cheap", ModelServed: "m1-cheapest",
		Status: 200,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, err := s.QueryAPILogsTimelinePage(Filter{Limit: 10}, 0, 0, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %d err=%v", len(rows), err)
	}
	log := rows[0].Log
	if log.ModelRequested != "p1/m1" || log.ModelUpstream != "m1-cheap" || log.ModelServed != "m1-cheapest" {
		t.Fatalf("row = %+v", log)
	}
}

func TestModelUsageSummaryRanksServedModel(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().Format(time.RFC3339)
	rows := []model.APIRequestLog{
		{TS: now, ReqID: "r1", Model: "p1/m1", ModelServed: "cheap", Status: 200},
		{TS: now, ReqID: "r2", Model: "p1/m1", ModelServed: "cheap", Status: 200},
		{TS: now, ReqID: "r3", Model: "p1/m1", Status: 200},
	}
	for _, row := range rows {
		if err := s.InsertAPIRequestLog(row); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	out, err := s.ModelUsageSummary(7)
	if err != nil {
		t.Fatalf("ModelUsageSummary: %v", err)
	}
	if len(out) != 2 || out[0].Model != "cheap" || out[0].Requests != 2 || out[1].Model != "p1/m1" {
		t.Fatalf("rows = %+v", out)
	}
}

func TestAPILogCostColumnsRoundtrip(t *testing.T) {
	s := openTestStore(t)
	cost := 0.0048
	if err := s.InsertAPIRequestLog(model.APIRequestLog{
		TS: "2026-01-15T10:00:00Z", ReqID: "r1", Model: "m1", Status: 200,
		CostUSD: &cost, PricingModel: "m1",
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, err := s.QueryAPILogsTimelinePage(Filter{Limit: 10}, 0, 0, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %d err=%v", len(rows), err)
	}
	log := rows[0].Log
	if log.CostUSD == nil || *log.CostUSD != cost || log.PricingModel != "m1" {
		t.Fatalf("cost = %v pricing = %q", log.CostUSD, log.PricingModel)
	}
}

func TestAPILogCacheAndPurposeRoundtrip(t *testing.T) {
	s := openTestStore(t)
	if err := s.InsertAPIRequestLog(model.APIRequestLog{
		TS: "2026-01-15T10:00:00Z", ReqID: "r1", Model: "m1", Status: 200,
		CacheWrite: 300, CacheWrite1h: 120, CacheUnknown: true,
		ReasoningTokens: 9, Purpose: "batch",
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, err := s.QueryAPILogsTimelinePage(Filter{Limit: 10}, 0, 0, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %d err=%v", len(rows), err)
	}
	log := rows[0].Log
	if log.CacheWrite != 300 || log.CacheWrite1h != 120 || !log.CacheUnknown ||
		log.ReasoningTokens != 9 || log.Purpose != "batch" {
		t.Fatalf("row = %+v", log)
	}
}
