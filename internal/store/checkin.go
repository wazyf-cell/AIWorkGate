package store

import (
	"database/sql"
	"fmt"

	"aiworkgate/internal/model"
)

// UpsertCheckinRecord writes one (plugin, account, date) check-in result and
// returns its row id. A repeated report for the same day updates the row, but
// the strongest outcome of the day wins: an ok/already result is never
// downgraded to fail/skipped. credits_after keeps the largest reported value
// (a nil report never erases a known one), and the optional dedup_scope and
// device_key columns (plan 2026-10-21 §4) are overwritten only when the new
// record carries a value, so a later failure cannot erase the device identity a
// successful attempt already recorded.
func (s *Store) UpsertCheckinRecord(rec model.CheckinRecord) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("store: upsert checkin: store is not open")
	}
	if rec.PluginID == "" || rec.AccountID == "" || rec.Date == "" {
		return 0, fmt.Errorf("store: upsert checkin: %w: plugin_id, account_id and date are required", ErrInvalidArgument)
	}
	at := rec.At
	if at == 0 {
		at = nowUnix()
	}

	ctx, cancel := s.ctx()
	defer cancel()

	var id int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO checkin_results
		(plugin_id, account_id, account_label, date, result, credits_before, credits_after, message, at, dedup_scope, device_key)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(plugin_id, account_id, date) DO UPDATE SET
			account_label = excluded.account_label,
			result = CASE
				WHEN checkin_results.result IN ('ok', 'already')
				 AND COALESCE(excluded.result, '') NOT IN ('ok', 'already')
				THEN checkin_results.result
				ELSE excluded.result END,
			credits_before = excluded.credits_before,
			credits_after = CASE
				WHEN excluded.credits_after IS NULL THEN checkin_results.credits_after
				WHEN checkin_results.credits_after IS NULL THEN excluded.credits_after
				WHEN excluded.credits_after > checkin_results.credits_after THEN excluded.credits_after
				ELSE checkin_results.credits_after END,
			message = excluded.message,
			at = excluded.at,
			dedup_scope = COALESCE(excluded.dedup_scope, dedup_scope),
			device_key = COALESCE(excluded.device_key, device_key)
		RETURNING id`,
		rec.PluginID, rec.AccountID, nullStr(rec.AccountLabel), rec.Date,
		nullStr(rec.Result), floatArg(rec.CreditsBefore), floatArg(rec.CreditsAfter),
		nullStr(rec.Message), at, nullStr(rec.DedupScope), nullStr(rec.DeviceKey),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: upsert checkin: %w", err)
	}
	return id, nil
}

// QueryCheckinRecords lists check-in rows ordered by date descending. An empty
// pluginID matches every plugin; empty fromDate/toDate open that bound.
func (s *Store) QueryCheckinRecords(pluginID, fromDate, toDate string) ([]model.CheckinRecord, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: query checkins: store is not open")
	}
	q := `SELECT id, plugin_id, account_id, COALESCE(account_label, ''), date,
		COALESCE(result, ''), credits_before, credits_after, COALESCE(message, ''), COALESCE(at, 0),
		COALESCE(dedup_scope, ''), COALESCE(device_key, '')
		FROM checkin_results`
	var (
		conds []string
		args  []any
	)
	if pluginID != "" {
		conds = append(conds, "plugin_id = ?")
		args = append(args, pluginID)
	}
	if fromDate != "" {
		conds = append(conds, "date >= ?")
		args = append(args, fromDate)
	}
	if toDate != "" {
		conds = append(conds, "date <= ?")
		args = append(args, toDate)
	}
	if len(conds) > 0 {
		q += " WHERE " + conds[0]
		for _, c := range conds[1:] {
			q += " AND " + c
		}
	}
	q += " ORDER BY date DESC, at DESC, id DESC"

	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query checkins: %w", err)
	}
	defer rows.Close()

	var out []model.CheckinRecord
	for rows.Next() {
		var (
			rec model.CheckinRecord
			cb  sql.NullFloat64
			ca  sql.NullFloat64
		)
		if err := rows.Scan(&rec.ID, &rec.PluginID, &rec.AccountID, &rec.AccountLabel,
			&rec.Date, &rec.Result, &cb, &ca, &rec.Message, &rec.At,
			&rec.DedupScope, &rec.DeviceKey); err != nil {
			return nil, fmt.Errorf("store: query checkins: scan: %w", err)
		}
		if cb.Valid {
			v := cb.Float64
			rec.CreditsBefore = &v
		}
		if ca.Valid {
			v := ca.Float64
			rec.CreditsAfter = &v
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: query checkins: iterate: %w", err)
	}
	return out, nil
}

// CleanupCheckinRecords deletes rows with a date strictly before beforeDate
// (YYYY-MM-DD) and reports the number removed.
func (s *Store) CleanupCheckinRecords(beforeDate string) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("store: cleanup checkins: store is not open")
	}
	if beforeDate == "" {
		return 0, fmt.Errorf("store: cleanup checkins: %w: beforeDate is required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		res, err := s.db.ExecContext(ctx, `DELETE FROM checkin_results WHERE id IN (
			SELECT id FROM checkin_results WHERE date < ? LIMIT ?)`, beforeDate, cleanupBatch)
		if err != nil {
			return total, fmt.Errorf("store: cleanup checkins: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("store: cleanup checkins: rows affected: %w", err)
		}
		total += n
		if n < cleanupBatch {
			return total, nil
		}
	}
}

// floatArg maps an optional float to a driver argument.
func floatArg(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

// checkinRowExists is a test helper used to assert upsert semantics.
func (s *Store) checkinRowCount() (int, error) {
	ctx, cancel := s.ctx()
	defer cancel()
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM checkin_results`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}
