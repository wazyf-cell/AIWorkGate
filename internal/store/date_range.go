package store

import "time"

// nextDay returns the first calendar day after day and whether day parsed.
func nextDay(day string) (string, bool) {
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		return "", false
	}
	return t.AddDate(0, 0, 1).Format("2006-01-02"), true
}

// dayRangeConds appends an inclusive local-day range as half-open comparisons
// over a column that holds RFC3339 timestamps.
//
// ts stores values like "2026-10-21T14:27:24+08:00", so its first ten bytes
// are the local date and a plain string compare already orders by day.
// Wrapping the column in substr() instead hides it from idx_api_logs_ts and
// idx_audit_log_ts, turning every usage query into a full table scan.
//
// The upper bound moves to the next day and stays exclusive: `ts <= day` would
// exclude every row on that day, because the stored value always carries a
// time suffix. day is a validated constant-shaped argument, never a column
// name, so interpolation of column is safe here.
func dayRangeConds(conds []string, args []any, column, fromDay, toDay string) ([]string, []any) {
	if fromDay != "" {
		conds = append(conds, column+" >= ?")
		args = append(args, fromDay)
	}
	if toDay != "" {
		if nxt, ok := nextDay(toDay); ok {
			conds = append(conds, column+" < ?")
			args = append(args, nxt)
		} else {
			// An unparseable day keeps the old prefix semantics rather than
			// silently returning the whole table.
			conds = append(conds, "substr("+column+", 1, 10) <= ?")
			args = append(args, toDay)
		}
	}
	return conds, args
}

// dayRangeSQL renders the same day range as a standalone SQL fragment for
// queries that build their predicate inline instead of collecting conditions.
// from and to are always non-empty here, because callers pass analyticsWindow's
// output; an unparseable `to` keeps the old prefix semantics.
func dayRangeSQL(column, from, to string) string {
	sql := "1 = 1"
	if from != "" {
		sql += " AND " + column + " >= ?"
	}
	if to != "" {
		if _, ok := nextDay(to); ok {
			sql += " AND " + column + " < ?"
		} else {
			sql += " AND substr(" + column + ", 1, 10) <= ?"
		}
	}
	return sql
}

// dayRangeArgs returns the bind values matching dayRangeSQL for the same window.
func dayRangeArgs(from, to string) []any {
	args := make([]any, 0, 2)
	if from != "" {
		args = append(args, from)
	}
	if to != "" {
		if nxt, ok := nextDay(to); ok {
			args = append(args, nxt)
		} else {
			args = append(args, to)
		}
	}
	return args
}
