package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// readSessionRecords reads every OfficeAce session record, newest first.
//
// A rollback-journal database can be opened read-only in place: SQLite takes
// read locks and reports "database is locked" as expected. A WAL database is
// different: opening it read-only makes SQLite create/update storage.sqlite-shm
// for WAL locking, which would write to a file owned by OfficeAce. For WAL we
// copy main + -wal into a plugin-private temp dir and open that snapshot with
// mode=ro + query_only(1). OfficeAce's files are only ever read.
func readSessionRecords(ctx context.Context, dbPath, workDir string) ([]*sessionRecord, error) {
	st, err := os.Stat(dbPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, &sqliteUnavailableError{Path: dbPath, Err: fmt.Errorf("文件不存在")}
		}
		return nil, &sqliteUnavailableError{Path: dbPath, Err: err}
	}
	if st.IsDir() {
		return nil, &sqliteUnavailableError{Path: dbPath, Err: fmt.Errorf("路径是目录")}
	}

	if !isWALDatabase(dbPath) {
		return querySessions(ctx, dbPath, dbPath)
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, &sqliteUnavailableError{Path: dbPath, Err: ctx.Err()}
			case <-time.After(time.Duration(attempt) * 150 * time.Millisecond):
			}
		}
		recs, err := readSessionSnapshot(ctx, dbPath, workDir)
		if err == nil {
			return recs, nil
		}
		lastErr = err
		var su *sqliteUnavailableError
		if errors.As(err, &su) && (su.Locked || strings.Contains(strings.ToLower(su.Error()), "corrupt")) {
			continue
		}
		break
	}
	return nil, lastErr
}

// isWALDatabase reports whether the database file uses WAL (header bytes 18/19
// == 2) or has a sidecar -wal file.
func isWALDatabase(dbPath string) bool {
	if st, err := os.Stat(dbPath + "-wal"); err == nil && st.Size() > 0 {
		return true
	}
	f, err := os.Open(dbPath)
	if err != nil {
		return true // be conservative: snapshot instead of touching -shm
	}
	defer f.Close()
	var hdr [20]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return true
	}
	return hdr[18] == 2 && hdr[19] == 2
}

func readSessionSnapshot(ctx context.Context, dbPath, workDir string) ([]*sessionRecord, error) {
	dir, err := os.MkdirTemp(workDir, "session-snapshot-")
	if err != nil {
		dir, err = os.MkdirTemp("", "officeace-session-")
	}
	if err != nil {
		return nil, &sqliteUnavailableError{Path: dbPath, Err: fmt.Errorf("创建临时快照目录失败: %w", err)}
	}
	defer os.RemoveAll(dir)

	snapshot := filepath.Join(dir, "storage.sqlite")
	if err := copyFile(dbPath, snapshot); err != nil {
		return nil, &sqliteUnavailableError{Path: dbPath, Locked: isLockedErr(err), Err: fmt.Errorf("复制数据库快照失败: %w", err)}
	}
	if _, err := os.Stat(dbPath + "-wal"); err == nil {
		if err := copyFile(dbPath+"-wal", snapshot+"-wal"); err != nil {
			return nil, &sqliteUnavailableError{Path: dbPath, Locked: isLockedErr(err), Err: fmt.Errorf("复制 WAL 快照失败: %w", err)}
		}
	}
	return querySessions(ctx, dbPath, snapshot)
}

// querySessions opens openPath strictly read-only and scans session rows.
func querySessions(ctx context.Context, errorPath, openPath string) ([]*sessionRecord, error) {
	db, err := sql.Open("sqlite", sqliteReadOnlyDSN(openPath))
	if err != nil {
		return nil, &sqliteUnavailableError{Path: errorPath, Locked: isLockedErr(err), Err: err}
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	queryCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	rows, err := db.QueryContext(queryCtx,
		"SELECT value FROM oc_kv_string WHERE key LIKE ? ORDER BY key", sessionKeyPrefix+"%")
	if err != nil {
		return nil, &sqliteUnavailableError{Path: errorPath, Locked: isLockedErr(err), Err: err}
	}
	defer rows.Close()

	var out []*sessionRecord
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, &sqliteUnavailableError{Path: errorPath, Err: err}
		}
		var rec sessionRecord
		if err := json.Unmarshal([]byte(value), &rec); err != nil {
			continue
		}
		if rec.SessionID == "" {
			continue
		}
		rec.resolveProviderState()
		out = append(out, &rec)
	}
	if err := rows.Err(); err != nil {
		return nil, &sqliteUnavailableError{Path: errorPath, Locked: isLockedErr(err), Err: err}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return sessionSeenKey(out[i]) > sessionSeenKey(out[j])
	})
	return out, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func sessionSeenKey(r *sessionRecord) string {
	if r.LastSeenAt != "" {
		return r.LastSeenAt
	}
	return r.CreatedAt
}
