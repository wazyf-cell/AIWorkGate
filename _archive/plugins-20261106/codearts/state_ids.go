package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

func hashHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		for i := range b {
			b[i] = byte(i*31 + 7)
		}
	}
	return hex.EncodeToString(b)
}

func newIdentity(now int64) accountIdentity {
	return accountIdentity{
		SessionID: randomHex(16),
		ChatID:    randomHex(16),
		Source:    "derived",
		UpdatedAt: now,
	}
}

// stableAccountID derives an id that survives repeated logins of the same
// Huawei account: acc_<first 10 hex of a stable identity hash>.
func stableAccountID(c Credential) string {
	key := strings.TrimSpace(c.DomainID) + "|" + strings.TrimSpace(c.UserID)
	if strings.TrimSpace(c.UserID) == "" {
		key = "ak|" + c.AccessKeyID
	}
	return "acc_" + hashHex("codearts-account:" + key)[:10]
}

func maskUserID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "CodeArts 账号"
	}
	r := []rune(id)
	if len(r) <= 4 {
		return "****"
	}
	if len(r) <= 8 {
		return string(r[:2]) + "****" + string(r[len(r)-2:])
	}
	return string(r[:3]) + "****" + string(r[len(r)-2:])
}

func accountLabel(c Credential) string {
	if s := strings.TrimSpace(c.UserName); s != "" {
		return s
	}
	return maskUserID(c.UserID)
}

func floatPtr(v float64) *float64 { return &v }

// deepCopyAccount makes a shallow copy that is safe to hand to callers that
// only inspect scalar fields and read credential strings.
func deepCopyAccount(a *Account) *Account {
	if a == nil {
		return nil
	}
	cp := *a
	cp.Credential.DPoPPrivateJWK = append(json.RawMessage(nil), a.Credential.DPoPPrivateJWK...)
	return &cp
}
