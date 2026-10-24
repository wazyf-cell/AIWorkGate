package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type refreshResp struct {
	Result    string `json:"result"`
	ExpiresAt int64  `json:"expires_at"`
	Message   string `json:"message"`
	At        int64  `json:"at"`
}

func TestReadSessionsNewestRecordAndCredentialParsing(t *testing.T) {
	older := defaultSession("old.example.test")
	older.SessionID = "sess-old"
	older.STSToken = "sts-older"
	older.LastSeenAt = fixedNow.Add(-24 * time.Hour).Format(time.RFC3339)

	newer := defaultSession("maas.example.test")
	newer.SessionID = "sess-new"
	newer.STSToken = "sts-base"
	newer.V3STSToken = "sts-v3"
	newer.LastSeenAt = fixedNow.Add(-time.Second).Format(time.RFC3339)
	newer.UserID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	newer.AccountID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	root := makeInstall(t, older, newer)
	cred := readSessionForTest(t, root)

	if cred.SessionID != "sess-new" {
		t.Fatalf("session = %q, want newest", cred.SessionID)
	}
	if cred.STSToken != "sts-v3" {
		t.Fatalf("sts token = %q, want credentialV3 to win", cred.STSToken)
	}
	if cred.UserKey() != newer.userKey() {
		t.Fatalf("user key = %q", cred.UserKey())
	}
	if !cred.HasBasic() || !cred.HasHMAC() {
		t.Fatalf("credential materials missing: %+v", cred)
	}
	if cred.ModelAPIBase != "https://maas.example.test/v2" {
		t.Fatalf("model api base = %q", cred.ModelAPIBase)
	}
	if cred.ChatEndpoint() != "https://maas.example.test/v2/chat/completions" {
		t.Fatalf("chat endpoint = %q", cred.ChatEndpoint())
	}
	if cred.IAMAccess != newer.IAMAccess || cred.IAMSecret != newer.IAMSecret {
		t.Fatalf("IAM material mismatch: %+v", cred)
	}
	if cred.RefreshTok != "refresh-token" || cred.RefreshExpiresAt.IsZero() {
		t.Fatalf("refresh token not parsed: %+v", cred)
	}
	if cred.Expired(fixedNow) {
		t.Fatal("future session reported expired")
	}
}

func TestReadSessionsSkipsInvalidJSON(t *testing.T) {
	root := makeInstall(t, defaultSession("maas.example.test"))
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(root, "data", "storage.sqlite")))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO oc_kv_string(key,value) VALUES(?,?)`, sessionKeyPrefix+"bad", "{not json"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	recs, err := readSessionRecords(context.Background(), filepath.Join(root, "data", "storage.sqlite"), t.TempDir())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(recs) != 1 || recs[0].SessionID != "sess-0001" {
		t.Fatalf("records = %+v", recs)
	}
}

func TestEmptyDatabaseMeansLoginRequired(t *testing.T) {
	root := makeInstall(t)
	_, err := readLiveCredential(context.Background(), Discovery{Found: true, DBPath: filepath.Join(root, "data", "storage.sqlite")}, t.TempDir())
	if !errors.Is(err, errNoSession) {
		t.Fatalf("err = %v, want errNoSession", err)
	}
}

func TestExpiredSessionRefreshViaIAM(t *testing.T) {
	s := defaultSession("maas.example.test")
	s.STSExpiresAt = fixedNow.Add(-time.Hour).Format(time.RFC3339)
	s.SessionExpiresAt = s.STSExpiresAt
	root := makeInstall(t, s)

	iam := newFakeIAM(t, "sts-renewed")
	p, h, _ := newTestPlugin(t, func(o *Options) {
		o.ForcedOfficeAceHome = installHome(t, root)
		o.IAMURL = iam.URL()
	})
	acctID := accountIDFor(s.userKey())
	if got := p.accountCount(); got != 1 {
		t.Fatalf("account count = %d", got)
	}
	if a, _ := p.findAccount(acctID); a.Status != statusExpired {
		t.Fatalf("status = %q, want expired", a.Status)
	}

	rec := doReq(t, h, http.MethodPost, "/accounts/"+acctID+"/refresh", reqOpts{})
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh status = %d body %s", rec.Code, rec.Body.String())
	}
	res := decodeAs[refreshResp](t, rec)
	if res.Result != "ok" || res.ExpiresAt == 0 {
		t.Fatalf("refresh = %+v", res)
	}
	if iam.callCount() != 1 {
		t.Fatalf("IAM calls = %d, want 1", iam.callCount())
	}
	if a, _ := p.findAccount(acctID); a.Status != statusOK || a.LastError != "" {
		t.Fatalf("account after refresh = %+v", a)
	}
	cred, err := p.liveCredential(context.Background())
	if err != nil {
		t.Fatalf("live credential: %v", err)
	}
	if cred.STSToken != "sts-renewed" || cred.Expired(fixedNow) {
		t.Fatalf("refreshed credential = %+v", cred)
	}
}

func TestRefreshFailureMarksExpired(t *testing.T) {
	s := defaultSession("maas.example.test")
	s.STSExpiresAt = fixedNow.Add(-time.Hour).Format(time.RFC3339)
	s.SessionExpiresAt = s.STSExpiresAt
	root := makeInstall(t, s)
	iam := newFakeIAM(t, "never")
	iam.status = 403

	p, h, _ := newTestPlugin(t, func(o *Options) {
		o.ForcedOfficeAceHome = installHome(t, root)
		o.IAMURL = iam.URL()
	})
	acctID := accountIDFor(s.userKey())
	rec := doReq(t, h, http.MethodPost, "/accounts/"+acctID+"/refresh", reqOpts{})
	res := decodeAs[refreshResp](t, rec)
	if res.Result != "fail" || !strings.Contains(res.Message, "重新登录") {
		t.Fatalf("refresh failure = %+v", res)
	}
	if a, _ := p.findAccount(acctID); a.Status != statusExpired || a.LastError == "" {
		t.Fatalf("account = %+v", a)
	}
	// The 403 triggers a no-token retry once.
	if iam.callCount() != 2 {
		t.Fatalf("IAM calls = %d, want 2 (with and without STS token)", iam.callCount())
	}
}

func TestWALSnapshotReadsFramesWithoutTouchingSHM(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dbPath := filepath.Join(dataDir, "storage.sqlite")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		t.Fatalf("wal: %v", err)
	}
	if _, err := db.Exec("PRAGMA wal_autocheckpoint=0"); err != nil {
		t.Fatalf("autocheckpoint: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE oc_kv_string (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	older := defaultSession("maas.example.test")
	older.SessionID = "wal-old"
	older.LastSeenAt = fixedNow.Add(-24 * time.Hour).Format(time.RFC3339)
	newer := defaultSession("maas.example.test")
	newer.SessionID = "wal-new"
	newer.LastSeenAt = fixedNow.Add(-time.Second).Format(time.RFC3339)
	for _, s := range []fixtureSession{older, newer} {
		if _, err := db.Exec(`INSERT INTO oc_kv_string(key,value) VALUES(?,?)`, sessionKeyPrefix+s.SessionID, s.json()); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	if st, err := os.Stat(dbPath + "-wal"); err != nil || st.Size() == 0 {
		t.Fatalf("expected non-empty WAL: %v", err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(dbPath+"-shm", old, old); err != nil {
		t.Fatalf("chtimes shm: %v", err)
	}

	recs, err := readSessionRecords(context.Background(), dbPath, t.TempDir())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(recs) != 2 || recs[0].SessionID != "wal-new" {
		t.Fatalf("records = %+v", recs)
	}
	st, err := os.Stat(dbPath + "-shm")
	if err != nil {
		t.Fatalf("stat shm: %v", err)
	}
	if !st.ModTime().Equal(old) {
		t.Fatalf("shm was modified: %v -> %v", old, st.ModTime())
	}
}

func TestLockedDatabaseReturnsClearError(t *testing.T) {
	root := makeInstall(t, defaultSession("maas.example.test"))
	dbPath := filepath.Join(root, "data", "storage.sqlite")

	writer, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	defer writer.Close()
	writer.SetMaxOpenConns(1)
	conn, err := writer.Conn(context.Background())
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN EXCLUSIVE"); err != nil {
		t.Fatalf("begin exclusive: %v", err)
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), "ROLLBACK") }()

	start := time.Now()
	_, err = readSessionRecords(context.Background(), dbPath, t.TempDir())
	if err == nil {
		t.Fatal("expected locked error")
	}
	var su *sqliteUnavailableError
	if !errors.As(err, &su) || !su.Locked {
		t.Fatalf("err = %v (%T), want locked sqliteUnavailableError", err, err)
	}
	if !strings.Contains(err.Error(), "被占用") {
		t.Fatalf("error not user friendly: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 1500*time.Millisecond {
		t.Fatalf("returned too quickly (%v); busy_timeout not applied?", elapsed)
	}
}

func TestAccountIDIsStableAcrossRestarts(t *testing.T) {
	s := defaultSession("maas.example.test")
	root := makeInstall(t, s)
	p1, _, _ := newTestPlugin(t, func(o *Options) { o.ForcedOfficeAceHome = installHome(t, root) })
	p2, _, _ := newTestPlugin(t, func(o *Options) { o.ForcedOfficeAceHome = installHome(t, root) })
	want := accountIDFor(s.userKey())
	if got := p1.state.Accounts[0].ID; got != want {
		t.Fatalf("id1 = %q, want %q", got, want)
	}
	if got := p2.state.Accounts[0].ID; got != want {
		t.Fatalf("id2 = %q, want %q", got, want)
	}
}
