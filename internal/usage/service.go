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
	// WindowsOf optionally supplies the live multi-window quota (D-21/F-14)
	// of one account, as passed through verbatim by the bridge. When it
	// returns a non-empty slice the D-53 runway numerator sums the windows'
	// remaining amounts instead of the legacy scalar snapshot remaining.
	// The burn rate stays scalar either way: quota_snapshots keeps no window
	// history, so a per-window rate would need a store migration that is out
	// of this card's scope.
	WindowsOf func(pluginID, accountID string) []map[string]any
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
	// D-32②: averages derived from the already-rolled-up sums (store.UsageTotals).
	// Both are null (omitted) when their sample count is 0 — an engineered 0
	// would read as "instant answers" on an empty range.
	AvgLatencyMS *float64 `json:"avg_latency_ms,omitempty"`
	AvgTTFBMS    *float64 `json:"avg_ttfb_ms,omitempty"`
	// CacheHitRate is the P9-8 acceptance metric: cached tokens over the
	// prompt tokens that carried them. The plan's denominator note
	// ("prompt_tokens − cached") is the correction for dialects whose prompt
	// already includes the cached prefix; adding the cached tokens back onto
	// that corrected denominator reproduces prompt_tokens verbatim, so the
	// equivalent magpie-aligned form is cached / prompt. Rows whose prompt is
	// 0 are skipped by the positive-denominator guard below. Known limit
	// (recorded, not silently dropped): usage_daily carries no cache_unknown
	// flag, so rows where the vendor reports no cache semantics at all stay
	// in the denominator — the api_logs column exists but the daily rollup
	// (deliberately) does not sum it.
	CacheHitRate *float64 `json:"cache_hit_rate,omitempty"`
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
	// RunwayHours estimates how long the remaining credits last at the recent
	// burn rate (D-53); nil when there is not enough history. RunwayAt is the
	// projected empty time. RunwayBasis says which remaining the numerator
	// used: "windows" (multi-window D-21 data, summed) or "scalar" (the
	// legacy quota_snapshots remaining). Empty when RunwayHours is nil.
	RunwayHours *float64 `json:"runway_hours,omitempty"`
	RunwayAt    *int64   `json:"runway_at,omitempty"`
	RunwayBasis string   `json:"runway_basis,omitempty"`
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

// runwayWindow and runwayMinSpan bound the burn-rate estimate: only
// snapshots from the last day count, and the two observations must be at
// least an hour apart so a rounding wobble is not read as a burn rate.
const (
	runwayWindow  = 24 * time.Hour
	runwayMinSpan = time.Hour
)

// accountRunway estimates remaining hours from the newest and oldest quota
// snapshots inside the window. A refill (remaining went up) or a short span
// yields nil instead of a misleading number.
//
// D-53 multi-window: when WindowsOf supplies the account's live windows the
// numerator is the sum of their remaining amounts (F-14: amount is the
// remaining side of "876 / 4600"; limit−used is the fallback spelling) and
// the basis comes back as "windows". The burn rate stays the scalar snapshot
// delta either way — quota_snapshots keeps no per-window history (noted on
// Service.WindowsOf).
func (s *Service) accountRunway(pluginID, accountID string, now int64) (*float64, *int64, string) {
	if s == nil || s.Store == nil || now <= 0 {
		return nil, nil, ""
	}
	since := now - int64(runwayWindow/time.Second)
	history, err := s.Store.QueryQuotaSnapshots(pluginID, accountID, since)
	if err != nil || len(history) < 2 {
		return nil, nil, ""
	}
	newest, oldest := history[0], history[len(history)-1]
	spanHours := float64(newest.At-oldest.At) / 3600
	if spanHours < runwayMinSpan.Hours() {
		return nil, nil, ""
	}
	consumed := oldest.Remaining - newest.Remaining
	if consumed <= 0 {
		return nil, nil, ""
	}
	remaining := newest.Remaining
	basis := "scalar"
	if windows := s.windowsRemaining(pluginID, accountID); windows > 0 {
		remaining = windows
		basis = "windows"
	}
	hours := remaining / (consumed / spanHours)
	if hours < 0 || math.IsNaN(hours) || math.IsInf(hours, 0) {
		return nil, nil, ""
	}
	at := now + int64(hours*3600)
	return &hours, &at, basis
}

// windowsRemaining sums the D-21 windows' remaining side. amount is the
// remaining amount in the F-14 shape ("876 / 4600"), so it wins when
// present; limit−used covers plugins that only report the pair. A window
// with neither usable pair is skipped; a total of 0 means "no multi-window
// data" and the caller keeps the scalar numerator.
func (s *Service) windowsRemaining(pluginID, accountID string) float64 {
	if s.WindowsOf == nil {
		return 0
	}
	windows := s.WindowsOf(pluginID, accountID)
	if len(windows) == 0 {
		return 0
	}
	total := 0.0
	for _, w := range windows {
		if v, ok := windowField(w, "amount"); ok {
			total += v
			continue
		}
		limit, limitOK := windowField(w, "limit")
		used, usedOK := windowField(w, "used")
		if limitOK && usedOK && limit-used > 0 {
			total += limit - used
		}
	}
	return total
}

// windowField reads one numeric window field tolerantly: plugins pass the
// windows through verbatim (D-21 forbids force-mapping), so the value may
// arrive as float64, int64, int, or a numeric string. Non-positive and
// non-numeric values read as absent.
func windowField(w map[string]any, key string) (float64, bool) {
	raw, ok := w[key]
	if !ok {
		return 0, false
	}
	switch v := raw.(type) {
	case float64:
		if v > 0 {
			return v, true
		}
	case int64:
		if v > 0 {
			return float64(v), true
		}
	case int:
		if v > 0 {
			return float64(v), true
		}
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f > 0 {
			return f, true
		}
	}
	return 0, false
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
	// D-32② 零成本速赢：平均延迟/平均首字。聚合算法的输出（sums）原样使用，
	// 只在派生时做 0 除数保护；count=0 时字段缺省，不显示 0。
	if totals.Requests > 0 {
		avg := float64(totals.LatencyMSSum) / float64(totals.Requests)
		out.KPIs.AvgLatencyMS = &avg
	}
	if totals.TTFBCount > 0 {
		avg := float64(totals.TTFBMSSum) / float64(totals.TTFBCount)
		out.KPIs.AvgTTFBMS = &avg
	}
	// P9-8 命中率：cached / prompt（分母≤0 不计）。口径推导见 SummaryKPI.CacheHitRate。
	if totals.PromptTokens > 0 && totals.CachedTokens > 0 {
		rate := float64(totals.CachedTokens) / float64(totals.PromptTokens)
		out.KPIs.CacheHitRate = &rate
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
		runwayHours, runwayAt, runwayBasis := s.accountRunway(snap.PluginID, snap.AccountID, now)
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
			RunwayHours:  runwayHours,
			RunwayAt:     runwayAt,
			RunwayBasis:  runwayBasis,
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
	rows, total, err := s.Store.UsageSessions(q.Filter, q.Model, q.ConversationID, out.PageSize, offset)
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
	timeseries, err := s.Store.UsageTimeSeries(q.FromDay, q.ToDay, q.Granularity)
	if err != nil {
		return "", err
	}
	models, err := s.Store.ModelUsageRange(q.FromDay, q.ToDay, 50)
	if err != nil {
		return "", err
	}
	keys, err := s.Store.KeyUsageRangeAggregates(q.FromDay, q.ToDay, 50)
	if err != nil {
		return "", err
	}
	clears, err := s.Store.ListUsageClears(200)
	if err != nil {
		return "", err
	}
	formatCost := func(cost float64, sampled bool) string {
		if !sampled {
			return ""
		}
		return strconv.FormatFloat(cost, 'f', -1, 64)
	}
	cw := csv.NewWriter(w)
	write := func(record []string) error { return cw.Write(record) }
	// The request-log block below filters by unix time on the RFC3339 ts
	// column, so the day range is converted to inclusive unix bounds here.
	fromUnix, toUnix := requestWindowUnix(q.FromDay, q.ToDay)
	if err := write([]string{"section", "day", "plugin_id", "account_id", "display_name",
		"requests", "prompt_tokens", "completion_tokens", "credits",
		"remaining", "total", "expires_at", "plan", "daily_credits", "days_left",
		"granularity", "cache_write", "cache_write_1h", "cache_unknown", "reasoning_tokens",
		"cost_usd", "pricing_model", "purpose", "model_requested", "model_upstream", "model_served",
		"cleared_at", "clear_note", "key_id", "key_name", "cache_read",
		// P13-2 request-level columns (docs/p13/01): data lives in api_logs,
		// they are only filled by the section=request rows below; every
		// aggregate row leaves them empty.
		"time", "duration_ms", "ttft_ms", "status", "error", "session",
		"request_id", "endpoint", "provider", "model", "swapped", "source",
		"attempt", "usage_known"}); err != nil {
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
			"", "", "", "", "", "", "", "", "", "", "",
			"", "", "", "", "",
			"", "", "", "", "", "", "", "", "", "", "", "", "", "",
		}); err != nil {
			return "", err
		}
	}
	for _, row := range timeseries {
		if err := write([]string{
			"timeseries", row.Bucket, row.PluginID, "", "",
			strconv.FormatInt(row.Requests, 10),
			strconv.FormatInt(row.PromptTokens, 10),
			strconv.FormatInt(row.CompletionTokens, 10),
			strconv.FormatFloat(row.Credits, 'f', -1, 64),
			"", "", "", "", "", "",
			q.Granularity,
			strconv.FormatInt(row.CacheWrite, 10),
			strconv.FormatInt(row.CacheWrite1h, 10),
			strconv.FormatInt(row.CacheUnknown, 10),
			strconv.FormatInt(row.ReasoningTokens, 10),
			formatCost(row.CostUSD, row.CostSampled),
			"", "", "", "", "",
			"", "", "", "",
			strconv.FormatInt(row.CacheRead, 10),
			"", "", "", "", "", "", "", "", "", "", "", "", "", "",
		}); err != nil {
			return "", err
		}
	}
	for _, row := range models {
		if err := write([]string{
			"model", "", "", "", row.Model,
			strconv.FormatInt(row.Requests, 10),
			strconv.FormatInt(row.PromptTokens, 10),
			strconv.FormatInt(row.CompletionTokens, 10),
			strconv.FormatFloat(row.Credits, 'f', -1, 64),
			"", "", "", "", "", "",
			q.Granularity,
			strconv.FormatInt(row.CacheWrite, 10),
			strconv.FormatInt(row.CacheWrite1h, 10),
			"", strconv.FormatInt(row.ReasoningTokens, 10),
			formatCost(row.CostUSD, row.CostSampled),
			"", "", "", "", row.Model,
			"", "", "", "",
			strconv.FormatInt(row.CacheRead, 10),
			"", "", "", "", "", "", "", "", "", "", "", "", "", "",
		}); err != nil {
			return "", err
		}
	}
	for _, row := range keys {
		if err := write([]string{
			"key", "", "", "", row.KeyName,
			strconv.FormatInt(row.Requests, 10),
			strconv.FormatInt(row.PromptTokens, 10),
			strconv.FormatInt(row.CompletionTokens, 10),
			strconv.FormatFloat(row.Credits, 'f', -1, 64),
			"", "", "", "", "", "",
			"",
			strconv.FormatInt(row.CacheWrite, 10),
			strconv.FormatInt(row.CacheWrite1h, 10),
			"", strconv.FormatInt(row.ReasoningTokens, 10),
			formatCost(row.CostUSD, row.CostSampled),
			"", "", "", "", "",
			"", "", row.KeyID, row.KeyName,
			strconv.FormatInt(row.CacheRead, 10),
			"", "", "", "", "", "", "", "", "", "", "", "", "", "",
		}); err != nil {
			return "", err
		}
	}
	for _, item := range clears {
		local := time.Unix(item.ClearedAt, 0).In(time.Local)
		day := local.Format("2006-01-02")
		if day < q.FromDay || day > q.ToDay {
			continue
		}
		if err := write([]string{
			"clear", day, "", "", "",
			"", "", "", "",
			"", "", "", "", "", "",
			"", "", "", "", "", "", "", "", "", "", "",
			local.Format(time.RFC3339), item.Note,
			"", "",
			"",
			"", "", "", "", "", "", "", "", "", "", "", "", "", "",
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
			"", "", "", "", "", "", "", "", "", "", "",
			"", "", "", "", "",
			"", "", "", "", "", "", "", "", "", "", "", "", "", "",
		}); err != nil {
			return "", err
		}
	}

	// P13-2: a section=request block carrying the per-request columns from
	// api_logs (time, duration_ms, ttft_ms, status, error, session,
	// request_id, endpoint, provider, model, swapped, source, attempt,
	// usage_known). The aggregate rows above never touch a request, so they
	// leave these cells empty.
	requests, _, err := s.Store.ExportAPIRequestLogs(store.Filter{From: fromUnix, To: toUnix}, 0)
	if err != nil {
		return "", err
	}
	for _, rec := range requests {
		provider := deriveProvider(rec.Source)
		swapped := "false"
		if rec.ModelRequested != "" && rec.ModelServed != "" &&
			!strings.EqualFold(strings.TrimSpace(rec.ModelRequested), strings.TrimSpace(rec.ModelServed)) {
			swapped = "true"
		}
		usageKnown := "0"
		if rec.UsageKnown {
			usageKnown = "1"
		}
		cost := ""
		if rec.CostUSD != nil {
			cost = strconv.FormatFloat(*rec.CostUSD, 'f', -1, 64)
		}
		if err := write([]string{
			"request", requestDay(rec.TS), requestPlugin(rec.Source), rec.Account, "",
			"", "", "", "",
			"", "", "", "", "", "", "", "", "", "", "",
			cost, "",
			rec.Purpose, rec.ModelRequested, rec.ModelUpstream, rec.ModelServed,
			"", "", rec.KeyID, rec.KeyName, "",
			rec.TS, strconv.FormatInt(rec.LatencyMS, 10), strconv.FormatInt(rec.TTFBMS, 10),
			strconv.Itoa(rec.Status), rec.Error, rec.ConversationID, rec.ReqID,
			rec.Purpose, provider, rec.Model, swapped, rec.Source,
			strconv.Itoa(rec.Attempt), usageKnown,
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

// requestWindowUnix converts the export's inclusive YYYY-MM-DD range into
// unix seconds on the request log's RFC3339 ts column. Unparsable bounds mean
// "no filter" (0, 0).
func requestWindowUnix(fromDay, toDay string) (int64, int64) {
	loc := time.Local
	from, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(fromDay), loc)
	if err != nil {
		return 0, 0
	}
	to, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(toDay), loc)
	if err != nil {
		return 0, 0
	}
	return from.Unix(), to.Add(24*time.Hour).Unix() - 1
}

// requestDay extracts the YYYY-MM-DD day of an RFC3339 timestamp; an
// unparsable timestamp yields an empty cell rather than a broken row.
func requestDay(ts string) string {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(ts))
	if err != nil {
		return ""
	}
	return t.Format("2006-01-02")
}

// deriveProvider maps the log's source prefix to the P13 provider column:
// plugin:<id> -> <id>, custom:<id> -> "custom", "a" stays "a".
func deriveProvider(source string) string {
	s := strings.TrimSpace(source)
	switch {
	case strings.HasPrefix(s, "plugin:"):
		return strings.TrimSpace(strings.TrimPrefix(s, "plugin:"))
	case strings.HasPrefix(s, "custom:"):
		return "custom"
	default:
		return s
	}
}

// requestPlugin resolves the plugin_id cell of a request row: plugin:<id>
// yields the id, anything else leaves the cell empty (custom connections have
// no plugin row in the aggregate sections either).
func requestPlugin(source string) string {
	s := strings.TrimSpace(source)
	if strings.HasPrefix(s, "plugin:") {
		return strings.TrimSpace(strings.TrimPrefix(s, "plugin:"))
	}
	return ""
}
