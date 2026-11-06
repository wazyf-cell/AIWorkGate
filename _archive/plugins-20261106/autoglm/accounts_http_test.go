package main

// Account-surface HTTP tests: the operator credential intake, the per-account
// sub-resource, and the endpoints whose only job is to answer honestly when
// nothing is configured.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestAdminCredentialsRejectsIncompleteDocument(t *testing.T) {
	s := newTestServer(t)
	for _, body := range []string{
		`{}`,
		`{"accessToken":"","userId":"u1"}`,
		`{"accessToken":"tok","userId":""}`,
		`{"accessToken":"tok"}`,
	} {
		rec := do(t, s, http.MethodPost, "/admin/credentials", body, true)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, rec.Code)
		}
	}
	if s.st.configured() {
		t.Fatal("a rejected document must not configure the store")
	}
}

func TestAdminCredentialsStoresAndFingerprints(t *testing.T) {
	s := newTestServer(t)
	rec := do(t, s, http.MethodPost, "/admin/credentials",
		`{"accessToken":"secret-token-xyz","userId":"u9","userName":"bob","region":"cn"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret-token-xyz") {
		t.Fatal("the response must carry a fingerprint, never the token")
	}
	got := decode(t, rec)
	fp, _ := got["key_fingerprint"].(string)
	if !strings.HasPrefix(fp, "sha256:") || len(fp) != len("sha256:")+8 {
		t.Fatalf("key_fingerprint = %q", fp)
	}
	if got["account_id"] != "cn/u9" {
		t.Fatalf("account_id = %v", got["account_id"])
	}
	if !s.st.configured() {
		t.Fatal("store must be configured after a valid document")
	}
}

func TestUnknownAccountSubresourceIsNotFound(t *testing.T) {
	s := newTestServer(t)
	rec := do(t, s, http.MethodPatch, "/accounts/cn/nobody", `{"enabled":false}`, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestReloadAcknowledges(t *testing.T) {
	s := newTestServer(t)
	rec := do(t, s, http.MethodPost, "/reload", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := decode(t, rec); got["ok"] != true {
		t.Fatalf("reload = %v", got)
	}
}

func TestCheckinWithoutAccountsReturnsEmptyResults(t *testing.T) {
	s := newTestServer(t)
	rec := do(t, s, http.MethodPost, "/checkin", `{"region":"all"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out struct {
		Results []any `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Results == nil {
		t.Fatal("results must be an empty array, not null")
	}
	if len(out.Results) != 0 {
		t.Fatalf("results = %v, want empty", out.Results)
	}
}

func TestLoginPollIsPendingBeforeAnyAccount(t *testing.T) {
	s := newTestServer(t)
	rec := do(t, s, http.MethodGet, "/accounts/login/some-flow", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	got := decode(t, rec)
	if got["status"] != "pending" {
		t.Fatalf("status = %v, want pending", got["status"])
	}
}

func TestUnknownLoginSessionDoesNotInheritCredential(t *testing.T) {
	s := newTestServer(t)
	if err := s.st.upsert(newAccount(accountData{UserID: "u-1", AccessToken: "at-old", Region: regionCN})); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	rec := do(t, s, http.MethodGet, "/accounts/login/never-issued", "", true)
	if got := decode(t, rec); got["status"] == "success" {
		t.Fatalf("unknown session inherited the stored credential: %v", got)
	}
}

func TestLoginSubmitRejectsUnsupportedValues(t *testing.T) {
	s := newTestServer(t)
	rec := do(t, s, http.MethodPost, "/accounts/login/f/submit", `{"values":{"nonsense":"1"}}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestAccountDeleteAcceptsRegionSlashUID(t *testing.T) {
	s := newTestServer(t)
	if err := s.st.upsert(newAccount(accountData{UserID: "u-1", AccessToken: "at-old", Region: regionCN})); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	rec := do(t, s, http.MethodDelete, "/accounts/cn/u-1", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d body %s", rec.Code, rec.Body.String())
	}
	if n := len(s.st.list()); n != 0 {
		t.Fatalf("accounts after delete = %d, want 0", n)
	}
}
