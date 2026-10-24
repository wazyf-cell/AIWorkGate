package usage

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"aiworkgate/internal/model"
	"aiworkgate/internal/store"
)

const (
	// DefaultRangeDays is the default summary window (§5.2: last 7 days).
	DefaultRangeDays = 7
	// MaxRangeDays caps an explicit range at 366 inclusive days.
	MaxRangeDays = 366
	// DefaultPageSize and MaxPageSize bound the sessions page.
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// ErrBadQuery marks a query the admin API should reject with 400.
var ErrBadQuery = errors.New("usage: invalid query")

// Query is a parsed /api/usage/* request.
type Query struct {
	FromDay  string
	ToDay    string
	Filter   store.UsageFilter
	Page     int
	PageSize int
	Model    string
}

// ParseQuery interprets from/to (YYYY-MM-DD local, inclusive), plugin, group
// and account filters plus page/page_size. Repeatable and comma-separated
// values are both accepted; dimensions AND, values within one dimension OR.
func ParseQuery(values url.Values, now time.Time) (Query, error) {
	loc := time.Local
	to := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	from := to.AddDate(0, 0, -(DefaultRangeDays - 1))

	if raw := firstValue(values, "from"); raw != "" {
		parsed, err := time.ParseInLocation("2006-01-02", raw, loc)
		if err != nil {
			return Query{}, fmt.Errorf("%w: from must be YYYY-MM-DD, got %q", ErrBadQuery, raw)
		}
		from = parsed
	}
	if raw := firstValue(values, "to"); raw != "" {
		parsed, err := time.ParseInLocation("2006-01-02", raw, loc)
		if err != nil {
			return Query{}, fmt.Errorf("%w: to must be YYYY-MM-DD, got %q", ErrBadQuery, raw)
		}
		to = parsed
	}
	if from.After(to) {
		from, to = to, from
	}
	if to.Sub(from) > time.Duration(MaxRangeDays-1)*24*time.Hour {
		from = to.AddDate(0, 0, -(MaxRangeDays - 1))
	}

	q := Query{
		FromDay:  from.Format("2006-01-02"),
		ToDay:    to.Format("2006-01-02"),
		Page:     1,
		PageSize: DefaultPageSize,
		Model:    strings.TrimSpace(firstValue(values, "model")),
		Filter: store.UsageFilter{
			FromDay: from.Format("2006-01-02"),
			ToDay:   to.Format("2006-01-02"),
			Plugins: multiValues(values, "plugin"),
			Groups:  multiValues(values, "group"),
		},
	}
	for _, item := range multiValues(values, "account") {
		ref, ok := parseAccountRef(item)
		if !ok {
			return Query{}, fmt.Errorf("%w: account must be plugin:account, got %q", ErrBadQuery, item)
		}
		q.Filter.Accounts = append(q.Filter.Accounts, ref)
	}
	if raw := strings.TrimSpace(firstValue(values, "page")); raw != "" {
		if n, err := parsePositive(raw); err == nil {
			q.Page = n
		}
	}
	if raw := strings.TrimSpace(firstValue(values, "page_size")); raw != "" {
		if n, err := parsePositive(raw); err == nil {
			if n > MaxPageSize {
				n = MaxPageSize
			}
			q.PageSize = n
		}
	}
	return q, nil
}

func parseAccountRef(item string) (model.AccountRef, bool) {
	item = strings.TrimSpace(item)
	// Accept both "mock:acc_1" and "plugin:mock:acc_1".
	item = strings.TrimPrefix(item, "plugin:")
	idx := strings.Index(item, ":")
	if idx <= 0 || idx == len(item)-1 {
		return model.AccountRef{}, false
	}
	pluginID := strings.TrimSpace(item[:idx])
	accountID := strings.TrimSpace(item[idx+1:])
	if pluginID == "" || accountID == "" {
		return model.AccountRef{}, false
	}
	return model.AccountRef{PluginID: pluginID, AccountID: accountID}, true
}

func firstValue(values url.Values, key string) string {
	if values == nil {
		return ""
	}
	items := values[key]
	if len(items) == 0 {
		return ""
	}
	return strings.TrimSpace(items[0])
}

// multiValues expands repeated params and comma-separated lists into a
// de-duplicated slice.
func multiValues(values url.Values, key string) []string {
	if values == nil {
		return nil
	}
	var out []string
	seen := make(map[string]bool)
	for _, raw := range values[key] {
		for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '|' }) {
			part = strings.TrimSpace(part)
			if part == "" || seen[part] {
				continue
			}
			seen[part] = true
			out = append(out, part)
		}
	}
	return out
}

func parsePositive(raw string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%w: want a positive integer, got %q", ErrBadQuery, raw)
	}
	return n, nil
}
