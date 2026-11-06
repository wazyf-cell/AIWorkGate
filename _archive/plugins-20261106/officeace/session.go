package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

const sessionKeyPrefix = "office-claw:oc:session:id:"

// Credential is the in-memory view of the newest OfficeAce session. The
// underlying values are read from OfficeAce's storage.sqlite and are never
// persisted by this plugin.
type Credential struct {
	SessionID   string
	ProviderID  string
	UserID      string
	AccountID   string
	UserName    string
	DisplayName string

	ModelAppKey    string
	ModelAppSecret string
	MaasAPIKey     string
	ModelAPIBase   string

	IAMAccess  string
	IAMSecret  string
	STSToken   string
	ProjectID  string
	RefreshTok string

	STSExpiresAt     time.Time
	SessionExpiresAt time.Time
	RefreshExpiresAt time.Time
	LastSeenAt       time.Time

	// StateError explains why an externally stored provider state could not be
	// read. It never carries credential values.
	StateError string

	DBPath string
}

// UserKey is the stable identity used for acc_<hash>. OfficeAce uses
// <accountId>:<userId> as its own per-user directory name, so mirror that.
func (c *Credential) UserKey() string {
	switch {
	case c.AccountID != "" && c.UserID != "":
		return c.AccountID + ":" + c.UserID
	case c.AccountID != "":
		return c.AccountID
	case c.UserID != "":
		return c.UserID
	default:
		return c.SessionID
	}
}

func (c *Credential) HasBasic() bool {
	return c.ModelAppKey != "" && c.ModelAppSecret != "" && c.ModelAPIBase != ""
}

func (c *Credential) HasHMAC() bool {
	return c.ModelAppKey != "" && c.ModelAppSecret != "" && c.STSToken != "" && c.ModelAPIBase != ""
}

// HasMaasKey reports whether the session carries the MaaS bearer API key that
// OfficeAce stores as maas_api_key. It needs the model API base to route the
// request; the key itself is sent as "Authorization: Bearer <key>".
func (c *Credential) HasMaasKey() bool {
	return c.MaasAPIKey != "" && c.ModelAPIBase != ""
}

// HasUpstreamCredential reports whether any supported MaaS auth shape is
// available: OfficeAce's model app key (Basic/HMAC) or the MaaS bearer key.
func (c *Credential) HasUpstreamCredential() bool {
	return c.HasBasic() || c.HasHMAC() || c.HasMaasKey()
}

func (c *Credential) Expired(now time.Time) bool {
	if c.STSExpiresAt.IsZero() {
		return false
	}
	return !c.STSExpiresAt.After(now.Add(2 * time.Minute))
}

func (c *Credential) ChatEndpoint() string { return c.ModelAPIBase + "/chat/completions" }

// normalizeModelAPIBase accepts the bare hostname OfficeAce stores and yields
// https://host/v2 (the app's own normalizeBaseUrl behavior).
func normalizeModelAPIBase(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if !strings.HasPrefix(strings.ToLower(s), "http://") && !strings.HasPrefix(strings.ToLower(s), "https://") {
		s = "https://" + s
	}
	s = strings.TrimRight(s, "/")
	if !strings.HasSuffix(strings.ToLower(s), "/v2") {
		s += "/v2"
	}
	return s
}

// endpointForModelBase normalizes a catalog base URL to its /v2 root.
func endpointForModelBase(raw string) string { return normalizeModelAPIBase(raw) }

type rawCredential struct {
	Access           string `json:"access"`
	Secret           string `json:"secret"`
	STSToken         string `json:"sts_token"`
	ProjectID        string `json:"project_id"`
	ExpiresAt        string `json:"expires_at"`
	RefreshToken     string `json:"refresh_token"`
	RefreshExpiresAt string `json:"refresh_expires_at"`
}

type providerState struct {
	AuthType     string        `json:"authType"`
	AccountID    string        `json:"accountId"`
	PrincipalURN string        `json:"principalUrn"`
	UserName     string        `json:"userName"`
	UserID       string        `json:"user_id"`
	Credential   rawCredential `json:"credential"`
	CredentialV3 rawCredential `json:"credentialV3"`
	ModelInfo    struct {
		ModelAuthInfo struct {
			ModelAppKey    string `json:"model_app_key"`
			ModelAppSecret string `json:"model_app_secret"`
			MaasAPIKey     string `json:"maas_api_key"`
		} `json:"model_auth_info"`
		ModelAPIURLBase string `json:"model_api_url_base"`
	} `json:"modelInfo"`
}

type sessionRecord struct {
	SessionID        string        `json:"sessionId"`
	ProviderID       string        `json:"providerId"`
	UserID           string        `json:"userId"`
	DisplayName      string        `json:"displayName"`
	CreatedAt        string        `json:"createdAt"`
	LastSeenAt       string        `json:"lastSeenAt"`
	ExpiresAt        string        `json:"expiresAt"`
	ProviderState    providerState `json:"providerState"`
	ProviderStateRef string        `json:"providerStateRef"`
	// ProviderStateError is filled when the state lives in an external store
	// that could not be read; it never contains credential material.
	ProviderStateError string `json:"-"`
}

// sqliteUnavailableError is returned with a user-facing explanation when the
// OfficeAce database cannot be opened or is busy.
type sqliteUnavailableError struct {
	Path   string
	Locked bool
	Err    error
}

func (e *sqliteUnavailableError) Error() string {
	if e.Locked {
		return fmt.Sprintf("OfficeAce 数据库被占用（storage.sqlite 正在被写入），请稍后重试：%v", e.Err)
	}
	return fmt.Sprintf("无法读取 OfficeAce 数据库 %s：%v", e.Path, e.Err)
}

func (e *sqliteUnavailableError) Unwrap() error { return e.Err }

// sqliteReadOnlyDSN opens OfficeAce's SQLite strictly read-only and query-only.
func sqliteReadOnlyDSN(path string) string {
	p := filepath.ToSlash(path)
	// Escape characters that SQLite's URI parser treats specially.
	p = strings.ReplaceAll(p, "%", "%25")
	p = strings.ReplaceAll(p, "?", "%3f")
	p = strings.ReplaceAll(p, "#", "%23")
	return "file:" + p + "?mode=ro&_pragma=busy_timeout(2000)&_pragma=query_only(1)"
}

func isLockedErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "database is locked") || strings.Contains(s, "database is busy") ||
		strings.Contains(s, "sqlite_busy") || strings.Contains(s, "sqlite_locked") ||
		strings.Contains(s, "sharing violation") || strings.Contains(s, "lock violation") ||
		strings.Contains(s, "being used by another process")
}
