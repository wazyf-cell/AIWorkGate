package store

// Connectivity probe history (Settings → 连通性自检). One row per explicit
// user-triggered probe against A, a plugin model or a custom model. The table
// ships with migration v7; failed probes are stored too, so the history shows
// outages as well as recoveries.
//
// Retention: rows older than ConnectivityProbeRetentionDays are pruned inside
// InsertConnectivityProbe, relative to the row being written. Pruning on insert
// (rather than on startup) also bounds a long-running tray host, which may
// never restart. The created_at index makes the delete a range scan.

import (
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	// ConnectivityProbeRetentionDays is how long probe history is kept.
	ConnectivityProbeRetentionDays = 90
	// connectivityProbeDefaultLimit and connectivityProbeMaxLimit mirror the
	// admin API (20 default, 100 max) so a direct store caller gets the same
	// bounds.
	connectivityProbeDefaultLimit = 20
	connectivityProbeMaxLimit     = 100
)

// ConnectivityProbe is one persisted probe. ErrorCode is empty on success and
// one of unreachable, timeout, model_missing, not_configured or upstream_error
// otherwise. Detail is already redacted by the host (no keys, no URL query).
type ConnectivityProbe struct {
	ID        int64
	Kind      string
	Target    string
	TargetID  string
	OK        bool
	Status    int
	LatencyMS int64
	Detail    string
	ErrorCode string
	CreatedAt int64
}

// InsertConnectivityProbe writes one probe row and returns its id. Rows older
// than ConnectivityProbeRetentionDays (measured from the new row's CreatedAt)
// are removed in the same call. CreatedAt <= 0 means "now".
func (s *Store) InsertConnectivityProbe(row ConnectivityProbe) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("store: insert connectivity probe: store is not open")
	}
	kind := strings.TrimSpace(row.Kind)
	if kind == "" {
		return 0, fmt.Errorf("store: insert connectivity probe: %w: kind is required", ErrInvalidArgument)
	}
	createdAt := row.CreatedAt
	if createdAt <= 0 {
		createdAt = nowUnix()
	}
	cutoff := createdAt - int64(ConnectivityProbeRetentionDays)*24*60*60

	ctx, cancel := s.ctx()
	defer cancel()
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM connectivity_probes WHERE created_at < ?`, cutoff); err != nil {
		return 0, fmt.Errorf("store: insert connectivity probe: prune: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO connectivity_probes
		(kind, target, target_id, ok, status, latency_ms, detail, error_code, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		kind, strings.TrimSpace(row.Target), strings.TrimSpace(row.TargetID),
		boolInt(row.OK), row.Status, row.LatencyMS, row.Detail,
		strings.TrimSpace(row.ErrorCode), createdAt)
	if err != nil {
		return 0, fmt.Errorf("store: insert connectivity probe: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: insert connectivity probe: last insert id: %w", err)
	}
	return id, nil
}

// ListConnectivityProbes returns probes newest first, applying the optional
// kind and target_id filters. limit <= 0 takes the default (20); values above
// 100 are clamped. The result is never nil so the admin API can serialize an
// empty array.
func (s *Store) ListConnectivityProbes(limit int, kind, targetID string) ([]ConnectivityProbe, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: list connectivity probes: store is not open")
	}
	if limit <= 0 {
		limit = connectivityProbeDefaultLimit
	}
	if limit > connectivityProbeMaxLimit {
		limit = connectivityProbeMaxLimit
	}

	query := `SELECT id, kind, target, target_id, ok, status, latency_ms, detail,
		error_code, created_at FROM connectivity_probes`
	var (
		where []string
		args  []any
	)
	if kind = strings.TrimSpace(kind); kind != "" {
		where = append(where, "kind = ?")
		args = append(args, kind)
	}
	if targetID = strings.TrimSpace(targetID); targetID != "" {
		where = append(where, "target_id = ?")
		args = append(args, targetID)
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	// id breaks ties between rows written in the same second (the timestamp
	// resolution of created_at), keeping "newest first" stable.
	query += " ORDER BY created_at DESC, id DESC LIMIT ?"
	args = append(args, limit)

	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list connectivity probes: %w", err)
	}
	defer rows.Close()
	return scanConnectivityProbes(rows, limit)
}

// PruneConnectivityProbes removes rows created before cutoff and reports how
// many were deleted. InsertConnectivityProbe already prunes by retention; this
// exported form exists for tests and for a future startup hook.
func (s *Store) PruneConnectivityProbes(cutoff int64) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("store: prune connectivity probes: store is not open")
	}
	if cutoff <= 0 {
		cutoff = time.Now().AddDate(0, 0, -ConnectivityProbeRetentionDays).Unix()
	}
	ctx, cancel := s.ctx()
	defer cancel()
	res, err := s.db.ExecContext(ctx, `DELETE FROM connectivity_probes WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: prune connectivity probes: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: prune connectivity probes: rows affected: %w", err)
	}
	return n, nil
}

// ListConnectivityProbesTimelinePage returns up to limit probes older than the
// (beforeAt, beforeID) position inside the day window, newest first. beforeAt
// is Unix milliseconds; zero means "from the newest row", and a zero beforeID
// includes every row at beforeAt. The merged timeline uses it so a probe shows
// up next to the request and audit rows it explains.
func (s *Store) ListConnectivityProbesTimelinePage(fromDay, toDay string, beforeAt, beforeID int64, limit int) ([]ConnectivityProbe, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: list connectivity probes page: store is not open")
	}
	if limit <= 0 {
		limit = connectivityProbeDefaultLimit
	}
	if limit > maxProxyLogScan {
		limit = maxProxyLogScan
	}
	from, to := localDayBounds(fromDay, toDay)
	where := " WHERE created_at >= ? AND created_at < ?"
	args := []any{from, to}
	if beforeAt > 0 {
		if beforeID <= 0 {
			beforeID = math.MaxInt64
		}
		where += " AND (created_at * 1000 < ? OR (created_at * 1000 = ? AND id < ?))"
		args = append(args, beforeAt, beforeAt, beforeID)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT id, kind, target, target_id, ok, status,
		latency_ms, detail, error_code, created_at FROM connectivity_probes`+where+
		` ORDER BY created_at DESC, id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("store: list connectivity probes page: %w", err)
	}
	defer rows.Close()
	return scanConnectivityProbes(rows, limit)
}

// MaxConnectivityProbeID reports the newest probe row id, or 0.
func (s *Store) MaxConnectivityProbeID() (int64, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	ctx, cancel := s.ctx()
	defer cancel()
	var id int64
	if err := s.db.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(id), 0) FROM connectivity_probes").Scan(&id); err != nil {
		return 0, fmt.Errorf("store: max connectivity probe id: %w", err)
	}
	return id, nil
}

// ListConnectivityProbesSince returns probes newer than id inside the day
// window, oldest first, for the live timeline tail.
func (s *Store) ListConnectivityProbesSince(id int64, fromDay, toDay string, limit int) ([]ConnectivityProbe, int64, error) {
	if s == nil || s.db == nil {
		return nil, id, nil
	}
	if limit <= 0 {
		limit = connectivityProbeDefaultLimit
	}
	from, to := localDayBounds(fromDay, toDay)
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT id, kind, target, target_id, ok, status,
		latency_ms, detail, error_code, created_at FROM connectivity_probes
		WHERE id > ? AND created_at >= ? AND created_at < ? ORDER BY id ASC LIMIT ?`,
		id, from, to, limit)
	if err != nil {
		return nil, id, fmt.Errorf("store: connectivity probes since: %w", err)
	}
	defer rows.Close()
	out, err := scanConnectivityProbes(rows, limit)
	last := id
	for _, row := range out {
		if row.ID > last {
			last = row.ID
		}
	}
	return out, last, err
}

func scanConnectivityProbes(rows *sql.Rows, limit int) ([]ConnectivityProbe, error) {
	out := make([]ConnectivityProbe, 0, limit)
	for rows.Next() {
		var (
			row ConnectivityProbe
			ok  int64
		)
		if err := rows.Scan(&row.ID, &row.Kind, &row.Target, &row.TargetID, &ok,
			&row.Status, &row.LatencyMS, &row.Detail, &row.ErrorCode, &row.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan connectivity probe: %w", err)
		}
		row.OK = ok != 0
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate connectivity probes: %w", err)
	}
	return out, nil
}
