package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func parseTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999999", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func parseUnix(s string) int64 {
	if t, ok := parseTime(s); ok {
		return t.Unix()
	}
	return 0
}

// credentialFromRecord converts the raw session JSON into the usable view.
// Newer credentialV3 values win for STS material; the long-term IAM AK/SK and
// OAuth refresh token stay from credential.
func credentialFromRecord(rec *sessionRecord, dbPath string) *Credential {
	ps := &rec.ProviderState
	base, v3 := ps.Credential, ps.CredentialV3
	c := &Credential{
		SessionID:      strings.TrimSpace(rec.SessionID),
		ProviderID:     strings.TrimSpace(rec.ProviderID),
		UserID:         firstNonEmpty(ps.UserID, rec.UserID),
		AccountID:      strings.TrimSpace(ps.AccountID),
		UserName:       strings.TrimSpace(ps.UserName),
		DisplayName:    strings.TrimSpace(rec.DisplayName),
		ModelAppKey:    strings.TrimSpace(ps.ModelInfo.ModelAuthInfo.ModelAppKey),
		ModelAppSecret: strings.TrimSpace(ps.ModelInfo.ModelAuthInfo.ModelAppSecret),
		MaasAPIKey:     strings.TrimSpace(ps.ModelInfo.ModelAuthInfo.MaasAPIKey),
		ModelAPIBase:   normalizeModelAPIBase(ps.ModelInfo.ModelAPIURLBase),
		IAMAccess:      firstNonEmpty(base.Access, v3.Access),
		IAMSecret:      firstNonEmpty(base.Secret, v3.Secret),
		STSToken:       firstNonEmpty(v3.STSToken, base.STSToken),
		ProjectID:      firstNonEmpty(v3.ProjectID, base.ProjectID),
		RefreshTok:     strings.TrimSpace(base.RefreshToken),
		StateError:     strings.TrimSpace(rec.ProviderStateError),
		DBPath:         dbPath,
	}
	if t, ok := parseTime(firstNonEmpty(v3.ExpiresAt, base.ExpiresAt, rec.ExpiresAt)); ok {
		c.STSExpiresAt = t
	}
	if t, ok := parseTime(rec.ExpiresAt); ok {
		c.SessionExpiresAt = t
	}
	if c.SessionExpiresAt.IsZero() {
		c.SessionExpiresAt = c.STSExpiresAt
	}
	if t, ok := parseTime(base.RefreshExpiresAt); ok {
		c.RefreshExpiresAt = t
	}
	if t, ok := parseTime(rec.LastSeenAt); ok {
		c.LastSeenAt = t
	}
	return c
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// fileSignature changes whenever the main DB or its WAL changes.
func fileSignature(dbPath string) string {
	part := func(p string) string {
		st, err := os.Stat(p)
		if err != nil {
			return "-"
		}
		return fmt.Sprintf("%d:%d", st.Size(), st.ModTime().UnixNano())
	}
	return part(dbPath) + "|" + part(dbPath+"-wal")
}

// readLiveCredential returns the newest session from OfficeAce's database.
func readLiveCredential(ctx context.Context, disc Discovery, workDir string) (*Credential, error) {
	if !disc.Found {
		return nil, &sqliteUnavailableError{Path: disc.DBPath, Err: fmt.Errorf("未找到 OfficeAce 安装目录")}
	}
	recs, err := readSessionRecords(ctx, disc.DBPath, workDir)
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, errNoSession
	}
	return credentialFromRecord(recs[0], disc.DBPath), nil
}

var errNoSession = errors.New("OfficeAce 尚未登录（storage.sqlite 中没有会话记录）")

// userModelJSONPaths returns candidate model.json files under
// %USERPROFILE%\.office-claw\users\<accountId>:<userId>\model.json (read-only).
func userModelJSONPaths(disc Discovery, cred *Credential) []string {
	base := disc.UserClawHome
	if base == "" {
		base = userClawHome()
	}
	if base == "" {
		return nil
	}
	users := filepath.Join(base, "users")
	var out []string
	if cred != nil && cred.UserKey() != "" {
		key := cred.UserKey()
		candidates := []string{
			filepath.Join(users, key, "model.json"),
			filepath.Join(users, strings.ReplaceAll(key, ":", "%3A"), "model.json"),
			filepath.Join(users, strings.ReplaceAll(key, ":", "%253A"), "model.json"),
		}
		for _, p := range candidates {
			if _, err := os.Stat(p); err == nil {
				out = append(out, p)
			}
		}
	}
	if len(out) == 0 {
		// Fall back to the most recently modified user model.json.
		entries, err := os.ReadDir(users)
		if err == nil {
			type cand struct {
				path string
				mod  time.Time
			}
			var cands []cand
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				p := filepath.Join(users, e.Name(), "model.json")
				if st, err := os.Stat(p); err == nil {
					cands = append(cands, cand{path: p, mod: st.ModTime()})
				}
			}
			sort.Slice(cands, func(i, j int) bool { return cands[i].mod.After(cands[j].mod) })
			for _, c := range cands {
				out = append(out, c.path)
			}
		}
	}
	return out
}

// redactURL drops credentials/query from a URL for logs.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Scheme + "://" + u.Host + u.Path
}
