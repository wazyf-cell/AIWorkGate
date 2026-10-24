package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// stubWinCred replaces the vault reader for one test.
func stubWinCred(t *testing.T, blobs map[string][]byte) {
	t.Helper()
	previous := readWinCredBlob
	readWinCredBlob = func(target string) ([]byte, error) {
		blob, ok := blobs[target]
		if !ok {
			return nil, fmt.Errorf("element not found")
		}
		return blob, nil
	}
	t.Cleanup(func() { readWinCredBlob = previous })
}

func utf16LE(s string) []byte {
	units := []rune(s)
	out := make([]byte, 0, len(units)*2)
	for _, r := range units {
		if r < 0x10000 {
			out = append(out, byte(r), byte(r>>8))
			continue
		}
		r -= 0x10000
		hi := 0xD800 + (r >> 10)
		lo := 0xDC00 + (r & 0x3FF)
		out = append(out, byte(hi), byte(hi>>8), byte(lo), byte(lo>>8))
	}
	return out
}

func providerStateDoc(t *testing.T) []byte {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"providerState": map[string]any{
			"accountId": "acct-1",
			"userName":  "office-user",
			"user_id":   "user-1",
			"modelInfo": map[string]any{
				"model_auth_info": map[string]any{
					"model_app_key":    "app-key",
					"model_app_secret": "app-secret",
					"maas_api_key":     "maas-key",
				},
				"model_api_url_base": "maas.example.com",
			},
			"credential": map[string]any{
				"access": "iam-ak", "secret": "iam-sk", "sts_token": "sts-token",
				"project_id": "project-1", "expires_at": "2026-10-21T10:00:00Z",
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return payload
}

func TestResolveProviderStateFromWinCredChunks(t *testing.T) {
	payload := providerStateDoc(t)
	const chunkSize = 100
	const target = "OfficeAce/test/provider-state"
	const storage = "abc123"
	blobs := map[string][]byte{}
	for offset, i := 0, 0; offset < len(payload); i++ {
		n := min(chunkSize, len(payload)-offset)
		chunk := base64.StdEncoding.EncodeToString(payload[offset : offset+n])
		blobs[fmt.Sprintf("%s/chunks/%s/%d", target, storage, i)] = utf16LE(chunk)
		offset += n
	}
	envelope, _ := json.Marshal(map[string]any{
		"schemaVersion": 2, "encoding": "base64", "storageId": storage, "chunkCount": len(blobs),
	})
	blobs[target] = utf16LE(string(envelope))
	stubWinCred(t, blobs)

	rec := &sessionRecord{SessionID: "s1", ProviderStateRef: winCredScheme + target}
	rec.resolveProviderState()
	if rec.ProviderStateError != "" {
		t.Fatalf("resolveProviderState: %s", rec.ProviderStateError)
	}
	cred := credentialFromRecord(rec, "db")
	if !cred.HasBasic() || !cred.HasHMAC() {
		t.Fatalf("credential incomplete: basic=%v hmac=%v", cred.HasBasic(), cred.HasHMAC())
	}
	if cred.ModelAppKey != "app-key" || cred.ModelAPIBase != "https://maas.example.com/v2" {
		t.Fatalf("model material = %q / %q", cred.ModelAppKey, cred.ModelAPIBase)
	}
	if cred.IAMAccess != "iam-ak" || cred.STSToken != "sts-token" || cred.ProjectID != "project-1" {
		t.Fatalf("iam material = %q / %q / %q", cred.IAMAccess, cred.STSToken, cred.ProjectID)
	}
	if cred.UserKey() != "acct-1:user-1" {
		t.Fatalf("UserKey = %q", cred.UserKey())
	}
}

func TestResolveProviderStateKeepsInlineState(t *testing.T) {
	previous := readWinCredBlob
	readWinCredBlob = func(target string) ([]byte, error) {
		t.Fatalf("inline state must not touch the vault (target %q)", target)
		return nil, nil
	}
	t.Cleanup(func() { readWinCredBlob = previous })
	rec := &sessionRecord{SessionID: "s1", ProviderStateRef: "wincred://ignored"}
	rec.ProviderState.AccountID = "inline"
	rec.ProviderState.ModelInfo.ModelAuthInfo.ModelAppKey = "inline-key"
	rec.resolveProviderState()
	if rec.ProviderStateError != "" || rec.ProviderState.AccountID != "inline" {
		t.Fatalf("inline state was replaced: %+v (%s)", rec.ProviderState, rec.ProviderStateError)
	}
}

func TestResolveProviderStateReportsVaultFailure(t *testing.T) {
	stubWinCred(t, map[string][]byte{})
	rec := &sessionRecord{SessionID: "s1", ProviderStateRef: winCredScheme + "OfficeAce/missing"}
	rec.resolveProviderState()
	if !strings.Contains(rec.ProviderStateError, "Windows 凭据管理器") {
		t.Fatalf("StateError = %q", rec.ProviderStateError)
	}
	cred := credentialFromRecord(rec, "db")
	if cred.StateError == "" {
		t.Fatal("Credential.StateError must carry the vault failure")
	}
	if cred.HasBasic() {
		t.Fatal("an unread state must not look usable")
	}
}

func TestLoadWinCredProviderStateReadsUnchunkedUTF8(t *testing.T) {
	payload := providerStateDoc(t)
	stubWinCred(t, map[string][]byte{"OfficeAce/plain": payload})
	state, err := loadWinCredProviderState("OfficeAce/plain")
	if err != nil {
		t.Fatalf("loadWinCredProviderState: %v", err)
	}
	if state.AccountID != "acct-1" {
		t.Fatalf("account = %q", state.AccountID)
	}
}

func TestResolveProviderStateRejectsUnknownScheme(t *testing.T) {
	rec := &sessionRecord{SessionID: "s1", ProviderStateRef: "file:///tmp/state.json"}
	rec.resolveProviderState()
	if !strings.Contains(rec.ProviderStateError, "未知的存储方式") {
		t.Fatalf("StateError = %q", rec.ProviderStateError)
	}
}
