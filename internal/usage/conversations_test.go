package usage

import (
	"errors"
	"net/url"
	"testing"

	"aiworkgate/internal/model"
	"aiworkgate/internal/store"
)

func openConversationStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(testTempDir(t))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return st
}

func seedConversations(t *testing.T, st *store.Store) {
	t.Helper()
	rows := []model.APIRequestLog{
		{TS: "2026-01-15T10:01:00Z", ReqID: "r1", Source: "plugin:a", Model: "m1",
			ConversationID: "c1", Status: 200, PromptTokens: 1, CompletionTokens: 1},
		{TS: "2026-01-15T10:02:00Z", ReqID: "r2", Source: "plugin:a", Model: "m1",
			ConversationID: "c2", Status: 200, PromptTokens: 2, CompletionTokens: 2},
		{TS: "2026-01-15T10:03:00Z", ReqID: "r3", Source: "plugin:a", Model: "m1",
			ConversationID: "c3", Status: 200, PromptTokens: 3, CompletionTokens: 3},
	}
	for _, rec := range rows {
		if err := st.InsertAPIRequestLog(rec); err != nil {
			t.Fatalf("insert %s: %v", rec.ReqID, err)
		}
	}
}

func conversationQuery(pageSize int) Query {
	return Query{
		Page:     1,
		PageSize: pageSize,
		Filter:   store.UsageFilter{FromDay: "2026-01-15", ToDay: "2026-01-15"},
	}
}

func conversationIDs(items []ConversationItem) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.ConversationID)
	}
	return out
}

func TestConversationsKeysetCursor(t *testing.T) {
	st := openConversationStore(t)
	seedConversations(t, st)
	svc := NewService(st)

	first, err := svc.Conversations(conversationQuery(2), "")
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if got := conversationIDs(first.Items); len(got) != 2 || got[0] != "c3" || got[1] != "c2" {
		t.Fatalf("page 1 ids = %v, want [c3 c2]", got)
	}
	if !first.HasMore || first.NextCursor == "" {
		t.Fatalf("page 1 hasMore=%v next=%q", first.HasMore, first.NextCursor)
	}

	second, err := svc.Conversations(conversationQuery(2), first.NextCursor)
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if got := conversationIDs(second.Items); len(got) != 1 || got[0] != "c1" {
		t.Fatalf("page 2 ids = %v, want [c1]", got)
	}
	if second.HasMore || second.NextCursor != "" {
		t.Fatalf("page 2 hasMore=%v next=%q, want false/empty", second.HasMore, second.NextCursor)
	}
}

func TestConversationsCursorAndPageSizeValidation(t *testing.T) {
	st := openConversationStore(t)
	svc := NewService(st)

	if _, err := svc.Conversations(conversationQuery(10), "not-a-cursor"); !errors.Is(err, ErrBadQuery) {
		t.Fatalf("invalid cursor err = %v, want ErrBadQuery", err)
	}

	page, err := svc.Conversations(conversationQuery(MaxPageSize+50), "")
	if err != nil {
		t.Fatalf("Conversations: %v", err)
	}
	if page.PageSize != MaxPageSize {
		t.Fatalf("page_size = %d, want clamp %d", page.PageSize, MaxPageSize)
	}

	if _, err := (&Service{}).Conversations(conversationQuery(10), ""); err == nil {
		t.Fatalf("nil store must return an error")
	}
}

func TestParseQueryCursor(t *testing.T) {
	q, err := ParseQuery(url.Values{"cursor": {"abc"}}, fixedNow())
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}
	if q.Cursor != "abc" {
		t.Fatalf("cursor = %q, want abc", q.Cursor)
	}
}
