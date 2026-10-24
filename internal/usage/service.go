package usage

import (
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"aiworkgate/internal/store"
)

// Service answers the /api/usage/* endpoints from the store.
type Service struct {
	Store *store.Store
	Now   func() time.Time
}

// NewService returns a usage service bound to st.
func NewService(st *store.Store) *Service {
	return &Service{Store: st, Now: time.Now}
}

// SummaryKPI is the scalar block of /api/usage/summary.
type SummaryKPI struct {
	Requests         int64   `json:"requests"`
	Errors           int64   `json:"errors"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	Credits          float64 `json:"credits"`
	ActiveAccounts   int64   `json:"active_accounts"`
}

// SummaryPoint is one day/plugin row of the summary series.
type SummaryPoint struct {
	Day              string  `json:"day"`
	PluginID         string  `json:"plugin_id"`
	Requests         int64   `json:"requests"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	Credits          float64 `json:"credits"`
}

// SummaryResponse is the body of GET /api/usage/summary.
type SummaryResponse struct {
	KPIs   SummaryKPI     `json:"kpis"`
	Series []SummaryPoint `json:"series"`
}

// CreditItem is one account row of /api/usage/credits and /api/usage/expiry.
type CreditItem struct {
	PluginID     string  `json:"plugin_id"`
	AccountID    string  `json:"account_id"`
	DisplayName  string  `json:"display_name"`
	Remaining    float64 `json:"remaining"`
	Total        float64 `json:"total"`
	ExpiresAt    int64   `json:"expires_at"`
	Plan         string  `json:"plan"`
	DailyCredits float64 `json:"daily_credits"`
	DaysLeft     *int    `json:"days_left"`
}

// CreditsResponse is the body of GET /api/usage/credits and .../expiry.
type CreditsResponse struct {
	Accounts []CreditItem `json:"accounts"`
}

// SessionItem is one request row of /api/usage/sessions.
type SessionItem struct {
	TS               string   `json:"ts"`
	ReqID            string   `json:"req_id"`
	PluginID         string   `json:"plugin_id"`
	AccountID        string   `json:"account_id"`
	AccountName      string   `json:"account_name"`
	Model            string   `json:"model"`
	LogicalModel     string   `json:"logical_model"`
	ConversationID   string   `json:"conversation_id"`
	PromptTokens     int64    `json:"prompt_tokens"`
	CompletionTokens int64    `json:"completion_tokens"`
	Credits          *float64 `json:"credits"`
	TTFBMS           int64    `json:"ttfb_ms"`
	LatencyMS        int64    `json:"latency_ms"`
	Status           int      `json:"status"`
}

// SessionsResponse is the body of GET /api/usage/sessions.
type SessionsResponse struct {
	Items    []SessionItem `json:"items"`
	Total    int           `json:"total"`
	Page     int           `json:"page"`
	PageSize int           `json:"page_size"`
}

func (s *Service) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Summary aggregates usage_daily for the queried range.
func (s *Service) Summary(q Query) (SummaryResponse, error) {
	out := SummaryResponse{Series: []SummaryPoint{}}
	if s == nil || s.Store == nil {
		return out, fmt.Errorf("usage: store is not available")
	}
	totals, err := s.Store.UsageTotalsOf(q.Filter)
	if err != nil {
		return out, err
	}
	rows, err := s.Store.UsageSeriesOf(q.Filter)
	if err != nil {
		return out, err
	}
	out.KPIs = SummaryKPI{
		Requests:         totals.Requests,
		Errors:           totals.Errors,
		PromptTokens:     totals.PromptTokens,
		CompletionTokens: totals.CompletionTokens,
		Credits:          totals.Credits,
		ActiveAccounts:   totals.ActiveAccounts,
	}
	for _, row := range rows {
		out.Series = append(out.Series, SummaryPoint{
			Day:              row.Day,
			PluginID:         row.PluginID,
			Requests:         row.Requests,
			PromptTokens:     row.PromptTokens,
			CompletionTokens: row.CompletionTokens,
			Credits:          row.Credits,
		})
	}
	return out, nil
}

// Credits returns the newest quota snapshot of every matching account.
// sortByExpiry switches the ordering used by /api/usage/expiry (unknown
// expiries come last).
func (s *Service) Credits(q Query, sortByExpiry bool) (CreditsResponse, error) {
	out := CreditsResponse{Accounts: []CreditItem{}}
	if s == nil || s.Store == nil {
		return out, fmt.Errorf("usage: store is not available")
	}
	snapshots, err := s.Store.UsageAccountCredits(q.Filter)
	if err != nil {
		return out, err
	}
	names, err := s.Store.AccountDisplayNames()
	if err != nil {
		return out, err
	}
	now := s.now().Unix()
	for _, snap := range snapshots {
		name := strings.TrimSpace(names[snap.PluginID+"\x00"+snap.AccountID])
		if name == "" {
			name = snap.AccountID
		}
		out.Accounts = append(out.Accounts, CreditItem{
			PluginID:     snap.PluginID,
			AccountID:    snap.AccountID,
			DisplayName:  name,
			Remaining:    snap.Remaining,
			Total:        snap.Total,
			ExpiresAt:    snap.ExpiresAt,
			Plan:         snap.Plan,
			DailyCredits: snap.DailyCredits,
			DaysLeft:     daysLeft(snap.ExpiresAt, now),
		})
	}
	sort.SliceStable(out.Accounts, func(i, j int) bool {
		a, b := out.Accounts[i], out.Accounts[j]
		if sortByExpiry {
			switch {
			case a.ExpiresAt == 0 && b.ExpiresAt == 0:
			case a.ExpiresAt == 0:
				return false
			case b.ExpiresAt == 0:
				return true
			case a.ExpiresAt != b.ExpiresAt:
				return a.ExpiresAt < b.ExpiresAt
			}
		}
		if a.PluginID != b.PluginID {
			return a.PluginID < b.PluginID
		}
		return a.AccountID < b.AccountID
	})
	return out, nil
}

// daysLeft converts an expiry into whole local days, negative once past.
// Unknown expiries yield nil so the UI can show "未知".
func daysLeft(expiresAt, now int64) *int {
	if expiresAt <= 0 {
		return nil
	}
	delta := expiresAt - now
	days := int(math.Floor(float64(delta) / 86400))
	if delta > 0 && delta%86400 != 0 {
		days++
	}
	return &days
}

// Sessions returns request rows inside the log retention window.
func (s *Service) Sessions(q Query) (SessionsResponse, error) {
	out := SessionsResponse{Items: []SessionItem{}, Page: q.Page, PageSize: q.PageSize}
	if s == nil || s.Store == nil {
		return out, fmt.Errorf("usage: store is not available")
	}
	if out.Page < 1 {
		out.Page = 1
	}
	if out.PageSize < 1 {
		out.PageSize = DefaultPageSize
	}
	if out.PageSize > MaxPageSize {
		out.PageSize = MaxPageSize
	}
	offset := (out.Page - 1) * out.PageSize
	rows, total, err := s.Store.UsageSessions(q.Filter, q.Model, out.PageSize, offset)
	if err != nil {
		return out, err
	}
	names, err := s.Store.AccountDisplayNames()
	if err != nil {
		return out, err
	}
	out.Total = total
	for _, row := range rows {
		pluginID := strings.TrimPrefix(row.Source, "plugin:")
		name := strings.TrimSpace(names[pluginID+"\x00"+row.Account])
		if name == "" {
			name = row.Account
		}
		out.Items = append(out.Items, SessionItem{
			TS:               row.TS,
			ReqID:            row.ReqID,
			PluginID:         row.Source,
			AccountID:        row.Account,
			AccountName:      name,
			Model:            row.Model,
			LogicalModel:     row.LogicalModel,
			ConversationID:   row.ConversationID,
			PromptTokens:     row.PromptTokens,
			CompletionTokens: row.CompletionTokens,
			Credits:          row.Credits,
			TTFBMS:           row.TTFBMS,
			LatencyMS:        row.LatencyMS,
			Status:           row.Status,
		})
	}
	return out, nil
}

// Export writes one CSV document with a section column: "series" rows carry
// the daily usage series, "account" rows carry the per-account credit block.
// A uniform header keeps the file parseable while still exposing both blocks.
func (s *Service) Export(w io.Writer, q Query) (string, error) {
	summary, err := s.Summary(q)
	if err != nil {
		return "", err
	}
	credits, err := s.Credits(q, false)
	if err != nil {
		return "", err
	}
	cw := csv.NewWriter(w)
	write := func(record []string) error { return cw.Write(record) }
	if err := write([]string{"section", "day", "plugin_id", "account_id", "display_name",
		"requests", "prompt_tokens", "completion_tokens", "credits",
		"remaining", "total", "expires_at", "plan", "daily_credits", "days_left"}); err != nil {
		return "", err
	}
	for _, row := range summary.Series {
		if err := write([]string{
			"series", row.Day, row.PluginID, "", "",
			strconv.FormatInt(row.Requests, 10),
			strconv.FormatInt(row.PromptTokens, 10),
			strconv.FormatInt(row.CompletionTokens, 10),
			strconv.FormatFloat(row.Credits, 'f', -1, 64),
			"", "", "", "", "", "",
		}); err != nil {
			return "", err
		}
	}
	for _, item := range credits.Accounts {
		days := ""
		if item.DaysLeft != nil {
			days = strconv.Itoa(*item.DaysLeft)
		}
		if err := write([]string{
			"account", "", item.PluginID, item.AccountID, item.DisplayName,
			"", "", "", "",
			strconv.FormatFloat(item.Remaining, 'f', -1, 64),
			strconv.FormatFloat(item.Total, 'f', -1, 64),
			strconv.FormatInt(item.ExpiresAt, 10),
			item.Plan,
			strconv.FormatFloat(item.DailyCredits, 'f', -1, 64),
			days,
		}); err != nil {
			return "", err
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return "", err
	}
	return fmt.Sprintf("usage-%s-%s.csv", q.FromDay, q.ToDay), nil
}
