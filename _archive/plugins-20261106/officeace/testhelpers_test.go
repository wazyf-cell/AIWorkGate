package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "test-token-123"

var fixedNow = time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local)

type safeLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *safeLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *safeLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

type reqOpts struct {
	body    string
	token   string
	noAuth  bool
	headers map[string]string
}

func newTestPlugin(t *testing.T, mutate func(*Options)) (*Plugin, http.Handler, *safeLog) {
	t.Helper()
	log := &safeLog{}
	empty := ""
	o := Options{
		DataDir:             t.TempDir(),
		Token:               testToken,
		LogWriter:           log,
		IsolateDiscovery:    true,
		ForcedOfficeAceHome: &empty,
		UserClawHome:        t.TempDir(),
		LocalAPIURL:         "http://127.0.0.1:1",
		IAMURL:              "http://127.0.0.1:1",
		Now:                 func() time.Time { return fixedNow },
	}
	if mutate != nil {
		mutate(&o)
	}
	p, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p, p.Handler(), log
}

func doReq(t *testing.T, h http.Handler, method, path string, o reqOpts) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if o.body != "" {
		body = strings.NewReader(o.body)
	}
	req := httptest.NewRequest(method, path, body)
	if o.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if !o.noAuth {
		tok := o.token
		if tok == "" {
			tok = testToken
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for k, v := range o.headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeAs[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

// ---------- SQLite fixture ----------

type fixtureSession struct {
	SessionID        string
	UserID           string
	AccountID        string
	UserName         string
	DisplayName      string
	AppKey           string
	AppSecret        string
	MaasAPIKey       string
	Base             string
	STSToken         string
	STSExpiresAt     string
	SessionExpiresAt string
	IAMAccess        string
	IAMSecret        string
	RefreshExpiresAt string
	LastSeenAt       string
	V3STSToken       string
}

func defaultSession(base string) fixtureSession {
	exp := fixedNow.Add(time.Hour).Format(time.RFC3339)
	return fixtureSession{
		SessionID:        "sess-0001",
		UserID:           "0123456789abcdef0123456789abcdef",
		AccountID:        "fedcba9876543210fedcba9876543210",
		UserName:         "hid_testuser",
		DisplayName:      "hid_testuser",
		AppKey:           "test-app-key-0001",
		AppSecret:        "test-app-secret-0001",
		Base:             base,
		STSToken:         "sts-old-token",
		STSExpiresAt:     exp,
		SessionExpiresAt: exp,
		IAMAccess:        "TESTACCESSKEY0000001",
		IAMSecret:        "test-iam-secret-0001",
		RefreshExpiresAt: fixedNow.Add(30 * 24 * time.Hour).Format(time.RFC3339),
		LastSeenAt:       fixedNow.Add(-time.Minute).Format(time.RFC3339),
	}
}

func (s fixtureSession) userKey() string { return s.AccountID + ":" + s.UserID }

func (s fixtureSession) json() string {
	sessionExpires := s.SessionExpiresAt
	if sessionExpires == "" {
		sessionExpires = s.STSExpiresAt
	}
	v3Token := s.V3STSToken
	if v3Token == "" {
		v3Token = s.STSToken
	}
	v3Expires := s.STSExpiresAt
	if tm, ok := parseTime(s.STSExpiresAt); ok {
		v3Expires = tm.UTC().Format("2006-01-02T15:04:05.000000Z")
	}
	rec := map[string]any{
		"sessionId":   s.SessionID,
		"providerId":  "huawei-oauth",
		"userId":      s.userKey(),
		"displayName": s.DisplayName,
		"createdAt":   fixedNow.Add(-2 * time.Hour).Format(time.RFC3339),
		"lastSeenAt":  s.LastSeenAt,
		"expiresAt":   sessionExpires,
		"persist":     false,
		"providerState": map[string]any{
			"authType":     "huawei-oauth",
			"accountId":    s.AccountID,
			"userName":     s.UserName,
			"user_id":      s.UserID,
			"principalUrn": "urn:huawei:iam::test",
			"modelInfo": map[string]any{
				"model_auth_info": map[string]any{
					"model_app_key":    s.AppKey,
					"model_app_secret": s.AppSecret,
					"maas_api_key":     s.MaasAPIKey,
				},
				"model_api_url_base": s.Base,
			},
			"credential": map[string]any{
				"access":             s.IAMAccess,
				"secret":             s.IAMSecret,
				"sts_token":          s.STSToken,
				"project_id":         "4b9948a3a5ff4b7ca3b547f9ba8d68c9",
				"expires_at":         s.STSExpiresAt,
				"refresh_token":      "refresh-token",
				"refresh_expires_at": s.RefreshExpiresAt,
			},
			"credentialV3": map[string]any{
				"access":     s.IAMAccess,
				"secret":     s.IAMSecret,
				"sts_token":  v3Token,
				"project_id": "4b9948a3a5ff4b7ca3b547f9ba8d68c9",
				"expires_at": v3Expires,
			},
		},
	}
	b, _ := json.Marshal(rec)
	return string(b)
}

// makeInstall creates a temporary OfficeAce install root with data\storage.sqlite.
func makeInstall(t *testing.T, sessions ...fixtureSession) string {
	t.Helper()
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dataDir, "storage.sqlite")))
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE oc_kv_string (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	for _, s := range sessions {
		if _, err := db.Exec(`INSERT INTO oc_kv_string(key,value) VALUES(?,?)`,
			sessionKeyPrefix+s.SessionID, s.json()); err != nil {
			t.Fatalf("insert session: %v", err)
		}
	}
	return root
}

func installHome(t *testing.T, root string) *string { t.Helper(); return &root }

// ---------- fake OfficeAce local API ----------

type fakeLocal struct {
	mu        sync.Mutex
	srv       *httptest.Server
	auths     []string
	subCalls  int
	claimCall int
	subBody   string
	subStatus int
	claimBody string
	claimStat int
	models    string
	profiles  string
	rates     string
	onSub     func(n int) (int, string)
	onClaim   func(n int) (int, string)
	modelsErr int
}

func newFakeLocal(t *testing.T) *fakeLocal {
	t.Helper()
	fl := &fakeLocal{
		subBody:   subscriptionJSON(5000, 1200, fixedNow.Add(30*24*time.Hour)),
		claimBody: `{"bonusSkus":[{"activityId":"a1","activityName":"bonus","points":100,"currentValue":0}]}`,
		models:    `{"success":true,"list":[]}`,
		profiles:  `{"providers":[]}`,
		rates:     `{"models":[]}`,
	}
	fl.srv = httptest.NewTLSServer(http.HandlerFunc(fl.handle))
	t.Cleanup(fl.srv.Close)
	return fl
}

func (f *fakeLocal) URL() string { return f.srv.URL }

func (f *fakeLocal) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.auths = append(f.auths, r.Header.Get("Authorization"))
	var subStatus, claimStatus int
	var subBody, claimBody string
	switch r.URL.Path {
	case "/api/subscription/current":
		f.subCalls++
		n := f.subCalls
		on := f.onSub
		subStatus, subBody = f.subStatus, f.subBody
		f.mu.Unlock()
		if on != nil {
			subStatus, subBody = on(n)
		}
		w.Header().Set("Content-Type", "application/json")
		if subStatus == 0 {
			subStatus = 200
		}
		w.WriteHeader(subStatus)
		_, _ = io.WriteString(w, subBody)
		return
	case "/api/subscription/claim-bonus":
		f.claimCall++
		n := f.claimCall
		on := f.onClaim
		claimStatus, claimBody = f.claimStat, f.claimBody
		f.mu.Unlock()
		if on != nil {
			claimStatus, claimBody = on(n)
		}
		w.Header().Set("Content-Type", "application/json")
		if claimStatus == 0 {
			claimStatus = 200
		}
		w.WriteHeader(claimStatus)
		_, _ = io.WriteString(w, claimBody)
		return
	case "/api/maas-models":
		status := f.modelsErr
		body := f.models
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if status == 0 {
			status = 200
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
		return
	case "/api/model-config-profiles":
		body := f.profiles
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
		return
	case "/api/subscription/model-token-rates":
		body := f.rates
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
		return
	case "/api/islogin":
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"islogin":true,"userId":"test"}`)
		return
	default:
		f.mu.Unlock()
		http.NotFound(w, r)
	}
}

func subscriptionJSON(total, used float64, expires time.Time) string {
	return fmt.Sprintf(`{"planId":"p1","planName":"Pro","tier":"standard","status":"active","expiresAt":%q,"usedCredits":%g,"totalCredits":%g,"resourceId":"r1","autoRenew":false}`,
		expires.Format(time.RFC3339), used, total)
}

func (f *fakeLocal) setSubscription(total, used float64, expires time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subBody = subscriptionJSON(total, used, expires)
}

func (f *fakeLocal) setSubStatus(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subStatus = code
}

func (f *fakeLocal) setClaim(code int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claimStat = code
	f.claimBody = body
}

func (f *fakeLocal) authSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.auths...)
}

// ---------- fake MaaS ----------

type maasRequest struct {
	Path          string
	Authorization string
	SecurityToken string
	ProjectID     string
	Body          string
}

type fakeMaaS struct {
	mu       sync.Mutex
	srv      *httptest.Server
	requests []maasRequest
}

func newFakeMaaS(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, body string)) *fakeMaaS {
	t.Helper()
	fm := &fakeMaaS{}
	fm.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fm.mu.Lock()
		fm.requests = append(fm.requests, maasRequest{
			Path:          r.URL.Path,
			Authorization: r.Header.Get("Authorization"),
			SecurityToken: r.Header.Get("X-Security-Token"),
			ProjectID:     r.Header.Get("X-Project-ID"),
			Body:          string(body),
		})
		fm.mu.Unlock()
		handler(w, r, string(body))
	}))
	t.Cleanup(fm.srv.Close)
	return fm
}

func (f *fakeMaaS) URL() string { return f.srv.URL }

func (f *fakeMaaS) snapshot() []maasRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]maasRequest(nil), f.requests...)
}

func writeSSE(w http.ResponseWriter, chunks ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, c := range chunks {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", c)
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

func successSSE(w http.ResponseWriter, r *http.Request, body string) {
	writeSSE(w,
		`{"id":"chatcmpl-up","object":"chat.completion.chunk","model":"glm-5.3","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		`{"id":"chatcmpl-up","object":"chat.completion.chunk","model":"glm-5.3","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`,
		`{"id":"chatcmpl-up","object":"chat.completion.chunk","model":"glm-5.3","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":"stop"}]}`,
		`{"id":"chatcmpl-up","object":"chat.completion.chunk","model":"glm-5.3","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`,
	)
}

// ---------- fake IAM ----------

type fakeIAM struct {
	mu     sync.Mutex
	srv    *httptest.Server
	calls  int
	tokens []string
	status int
	token  string
}

func newFakeIAM(t *testing.T, token string) *fakeIAM {
	t.Helper()
	fi := &fakeIAM{token: token}
	fi.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fi.mu.Lock()
		fi.calls++
		fi.tokens = append(fi.tokens, r.Header.Get("X-Security-Token"))
		status := fi.status
		tok := fi.token
		fi.mu.Unlock()
		if status == 0 {
			status = 200
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status >= 300 {
			_, _ = io.WriteString(w, `{"error":"denied"}`)
			return
		}
		_, _ = fmt.Fprintf(w, `{"credential":{"securitytoken":%q,"expires_at":%q}}`,
			tok, fixedNow.Add(24*time.Hour).Format(time.RFC3339))
	}))
	t.Cleanup(fi.srv.Close)
	return fi
}

func (f *fakeIAM) URL() string { return f.srv.URL }

func (f *fakeIAM) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// ---------- fake HTTP proxy ----------

type fakeProxy struct {
	mu   sync.Mutex
	srv  *httptest.Server
	hits int
	urls []string
}

func newFakeProxy(t *testing.T) *fakeProxy {
	t.Helper()
	fp := &fakeProxy{}
	fp.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		fp.mu.Lock()
		fp.hits++
		fp.urls = append(fp.urls, r.URL.String())
		fp.mu.Unlock()
		writeSSE(w,
			`{"id":"chatcmpl-proxy","object":"chat.completion.chunk","model":"glm-5.3","choices":[{"index":0,"delta":{"content":"via proxy"},"finish_reason":"stop"}]}`,
			`{"id":"chatcmpl-proxy","object":"chat.completion.chunk","model":"glm-5.3","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		)
	}))
	t.Cleanup(fp.srv.Close)
	return fp
}

func (f *fakeProxy) URL() string { return f.srv.URL }

func (f *fakeProxy) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

// ---------- misc helpers ----------

func egressJSON(strict bool, accounts map[string]map[string]string) string {
	if accounts == nil {
		accounts = map[string]map[string]string{}
	}
	b, _ := json.Marshal(map[string]any{
		"version":  1,
		"strict":   strict,
		"default":  map[string]string{"mode": "direct"},
		"accounts": accounts,
	})
	return string(b)
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func sourceOf(rec *httptest.ResponseRecorder) string {
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	s, _ := m["source"].(string)
	return s
}

// sampleChatBody builds a minimal OpenAI request.
func sampleChatBody(model, content string, extra map[string]any) string {
	m := map[string]any{
		"model":    model,
		"messages": []map[string]any{{"role": "user", "content": content}},
	}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// readSessionForTest bypasses the plugin for focused parser tests.
func readSessionForTest(t *testing.T, root string) *Credential {
	t.Helper()
	cred, err := readLiveCredential(context.Background(), Discovery{Found: true, DBPath: filepath.Join(root, "data", "storage.sqlite")}, t.TempDir())
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	return cred
}
