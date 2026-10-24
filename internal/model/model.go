// Package model holds the shared data structures of the AIWorkGate plugin
// contract. It has no dependencies so every other package can import it.
package model

const (
	// MinAPILevel and MaxAPILevel bound the plugin contract levels this host
	// loads. Level 1 plugins keep working with whole-pool scheduling; level 2
	// adds the forced-account headers used by the scheduler (S3).
	MinAPILevel = 1
	MaxAPILevel = 2

	// APIVersion is the contract level a brand new plugin should declare.
	APIVersion = MinAPILevel

	CapUpstream        = "upstream"
	CapAccounts        = "accounts"
	CapCheckin         = "checkin"
	CapQuota           = "quota"
	CapLoginAPIKey     = "login_api_key"
	CapLoginOAuth      = "login_oauth"
	CapLoginPasteCode  = "login_paste_code"
	CapLoginIDEPlugin  = "login_ide_plugin"
	CapLoginLocalState = "login_local_state"
	CapLoginCookie     = "login_cookie"
	CapRefresh         = "refresh"
	CapEgress          = "egress"
	CapIdentityRequest = "identity_request"
	CapIdentityOS      = "identity_os_switch"

	AccountOK          = "ok"
	AccountExpired     = "expired"
	AccountInvalid     = "invalid"
	AccountRateLimited = "rate_limited"
	AccountUnknown     = "unknown"

	CheckinOK      = "ok"
	CheckinAlready = "already"
	CheckinFail    = "fail"
	// CheckinSkipped marks an account the host refused to call: it is cooling
	// down after a classified failure, or its device already signed in today.
	// It is not a success and never enters checkin_results as ok/already.
	CheckinSkipped = "skipped"

	// Account health kinds (plan 2026-10-21 §4). They mirror the error types of
	// A's tasks/trae_checkin.rs classify_error (MIT, 朱天伟); normal is the
	// absence of a health row. A plugin may report the kind on a check-in
	// failure; the host otherwise derives it from the status code/message.
	StatusKindNormal       = "normal"
	StatusKindRateLimited  = "rate_limited"
	StatusKindSessionDead  = "session_dead"
	StatusKindPlanLimit    = "plan_limit"
	StatusKindDeviceSigned = "device_signed"
	StatusKindQuota        = "quota"
	StatusKindBanned       = "banned"

	// Check-in dedup scopes: account means one check-in per account and day,
	// device means the upstream counts one check-in per device and day.
	DedupScopeAccount = "account"
	DedupScopeDevice  = "device"

	LoginNeedInput = "need_input"
	LoginPending   = "pending"
	LoginSuccess   = "success"
	LoginFailed    = "failed"

	EgressDirect  = "direct"
	EgressProxy   = "proxy"
	EgressBlocked = "blocked"

	// SystemProxyID is the reserved proxy row the host keeps in sync with the
	// Windows system proxy. Users bind accounts to it but cannot edit or delete
	// it: its endpoint comes from Internet Settings.
	SystemProxyID   = "system"
	SystemProxyName = "系统代理（跟随 Windows）"

	IdentityDerived = "derived"
	IdentityRandom  = "random"
	IdentityOS      = "os"

	StartupAlways   = "always"
	StartupOnDemand = "on_demand"
)

// Manifest is the on-disk plugin descriptor.
type Manifest struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Version      string            `json:"version"`
	APILevel     int               `json:"api_level"`
	Description  string            `json:"description,omitempty"`
	Author       string            `json:"author,omitempty"`
	Homepage     string            `json:"homepage,omitempty"`
	OS           []string          `json:"os,omitempty"`
	Capabilities []string          `json:"capabilities,omitempty"`
	Permissions  Permissions       `json:"permissions"`
	Models       []ModelInfo       `json:"models,omitempty"`
	Checkin      CheckinConfig     `json:"checkin"`
	Quota        QuotaConfig       `json:"quota"`
	Concurrency  ConcurrencyConfig `json:"concurrency"`
	Rules        RulesConfig       `json:"rules"`
	Egress       EgressConfig      `json:"egress"`
	Startup      StartupConfig     `json:"startup"`
	Entry        Entry             `json:"entry"`
	// Login is the optional login-policy block. When absent the host derives
	// the policy from capabilities and permissions (§5.1 login-state).
	Login *LoginConfig `json:"login,omitempty"`
}

// LoginConfig is the optional manifest.login block: it describes how a plugin
// authenticates accounts without the host ever seeing credentials.
type LoginConfig struct {
	Methods         []string `json:"methods,omitempty"`
	ReadsLocalState bool     `json:"reads_local_state,omitempty"`
	RequiresApp     bool     `json:"requires_app,omitempty"`
	AutoRefresh     bool     `json:"auto_refresh,omitempty"`
	Notes           string   `json:"notes,omitempty"`
}

type Permissions struct {
	ReadPaths      []string `json:"read_paths,omitempty"`
	LaunchApps     []string `json:"launch_apps,omitempty"`
	NetworkDomains []string `json:"network_domains,omitempty"`
	UseProxy       bool     `json:"use_proxy,omitempty"`
	OSIdentity     bool     `json:"os_identity,omitempty"`
	RequireAdmin   bool     `json:"require_admin,omitempty"`
}

type ModelInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	ContextLength int    `json:"context_length,omitempty"`
	Tools         bool   `json:"tools,omitempty"`
	Vision        bool   `json:"vision,omitempty"`
	// Contract v0.3 optional catalog fields (§3.4, §6.6). They only feed
	// scheduling scores and the catalog view; routing never depends on them.
	Vendor     string   `json:"vendor,omitempty"`
	Multiplier float64  `json:"multiplier,omitempty"`
	Thinking   []string `json:"thinking,omitempty"`
	Free       bool     `json:"free,omitempty"`
}

type CheckinConfig struct {
	DefaultTime     string `json:"default_time,omitempty"`
	SupportsHistory bool   `json:"supports_history,omitempty"`
}

type QuotaConfig struct {
	// PollMinutes nil means the contract default (30 minutes); 0 means the
	// host must not poll on a timer (quota is read when the dashboard asks).
	PollMinutes *int `json:"poll_minutes,omitempty"`
}

type ConcurrencyConfig struct {
	Suggested int `json:"suggested,omitempty"`
}

type RulesConfig struct {
	Packs []string `json:"packs,omitempty"`
}

type EgressConfig struct {
	Strict bool `json:"strict,omitempty"`
}

type StartupConfig struct {
	Mode         string `json:"mode,omitempty"`
	IdleTimeoutS int    `json:"idle_timeout_s,omitempty"`
}

type Entry struct {
	Exe  string   `json:"exe"`
	Args []string `json:"args,omitempty"`
}

// Account is what a plugin reports through GET /accounts.
type Account struct {
	ID         string      `json:"id"`
	Label      string      `json:"label"`
	Enabled    bool        `json:"enabled"`
	Status     string      `json:"status"`
	AddedAt    int64       `json:"added_at,omitempty"`
	Quota      Quota       `json:"quota"`
	Credential *Credential `json:"credential,omitempty"`
	Identity   *Identity   `json:"identity,omitempty"`
	LastError  string      `json:"last_error,omitempty"`

	// Contract v0.3 optional scheduling hints (§3.3). The host overlay wins
	// when it sets priority/weight; cooldowns are merged with the scheduler's
	// own failure state.
	Priority       int              `json:"priority,omitempty"`
	Weight         float64          `json:"weight,omitempty"`
	CooldownUntil  int64            `json:"cooldown_until,omitempty"`
	ModelCooldowns map[string]int64 `json:"model_cooldowns,omitempty"`
}

type Quota struct {
	Remaining      float64 `json:"remaining,omitempty"`
	Total          float64 `json:"total,omitempty"`
	Unlimited      bool    `json:"unlimited,omitempty"`
	ResetAt        int64   `json:"reset_at,omitempty"`
	ExpiresAt      int64   `json:"expires_at,omitempty"`
	Plan           string  `json:"plan,omitempty"`
	RateLimited    bool    `json:"rate_limited,omitempty"`
	TodayCheckedIn bool    `json:"today_checked_in,omitempty"`
	CheckedInAt    int64   `json:"checked_in_at,omitempty"`
}

// Credential is the optional credential health a plugin may report. The host
// never sees the secret itself, only its presence and lifetime.
type Credential struct {
	Present     bool  `json:"present"`
	ExpiresAt   int64 `json:"expires_at,omitempty"`
	Refreshable bool  `json:"refreshable,omitempty"`
}

type Identity struct {
	MachineID string `json:"machine_id,omitempty"`
	DeviceID  string `json:"device_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Source    string `json:"source,omitempty"`
	UpdatedAt int64  `json:"updated_at,omitempty"`
}

type AccountsResponse struct {
	Accounts []Account `json:"accounts"`
}

type CheckinResult struct {
	Result        string   `json:"result"`
	CreditsBefore *float64 `json:"credits_before,omitempty"`
	CreditsAfter  *float64 `json:"credits_after,omitempty"`
	Message       string   `json:"message,omitempty"`
	At            int64    `json:"at,omitempty"`

	// Contract v0.4 optional failure attribution (plan 2026-10-21 §4). A
	// plugin that can tell why a check-in failed reports Kind and, when the
	// upstream named a reset moment, ResetAt (Unix seconds). DedupScope and
	// DeviceKey describe upstream deduplication ("device" + an opaque device
	// key) so the host can skip the accounts sharing that device today.
	// Older plugins omit all four and the host falls back to its own rules.
	Kind       string `json:"kind,omitempty"`
	ResetAt    int64  `json:"reset_at,omitempty"`
	DedupScope string `json:"dedup_scope,omitempty"`
	DeviceKey  string `json:"device_key,omitempty"`
}

type RefreshResult struct {
	Result    string `json:"result"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
	Message   string `json:"message,omitempty"`
	At        int64  `json:"at,omitempty"`
}

type LoginField struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Type        string `json:"type"`
	Required    bool   `json:"required,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	Help        string `json:"help,omitempty"`
}

type LoginStart struct {
	SessionID    string       `json:"session_id"`
	Status       string       `json:"status"`
	Method       string       `json:"method,omitempty"`
	URL          string       `json:"url,omitempty"`
	Code         string       `json:"code,omitempty"`
	Instructions string       `json:"instructions,omitempty"`
	Fields       []LoginField `json:"fields,omitempty"`
	AccountID    string       `json:"account_id,omitempty"`
	Error        string       `json:"error,omitempty"`

	// Login links travel under several spellings in the wild. The host decodes
	// all of them and fills URL, so a plugin that says login_url (or an RFC
	// device-flow name) still reaches the browser affordance instead of a
	// pending session with nothing to open.
	LoginURL                string `json:"login_url,omitempty"`
	AuthorizeURL            string `json:"authorize_url,omitempty"`
	VerificationURI         string `json:"verification_uri,omitempty"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
}

// NormalizeLoginURL fills URL from the first non-empty alias. It never invents
// a value: with no alias present URL stays empty and the UI keeps waiting.
func (l *LoginStart) NormalizeLoginURL() {
	if l == nil || l.URL != "" {
		return
	}
	for _, candidate := range []string{
		l.AuthorizeURL, l.LoginURL, l.VerificationURIComplete, l.VerificationURI,
	} {
		if candidate != "" {
			l.URL = candidate
			return
		}
	}
}

type DetectedAccount struct {
	Label string `json:"label"`
	Hint  string `json:"hint,omitempty"`
}

type LoginStatus struct {
	Status    string            `json:"status"`
	AccountID string            `json:"account_id,omitempty"`
	Error     string            `json:"error,omitempty"`
	Detected  []DetectedAccount `json:"detected,omitempty"`
}

type LoginSubmit struct {
	Values map[string]string `json:"values"`
}

// PluginStatus is the host-side view of an installed plugin. The effective
// contract level is exposed as Manifest.APILevel (api_level in JSON).
type PluginStatus struct {
	Manifest    Manifest `json:"manifest"`
	Dir         string   `json:"dir"`
	Enabled     bool     `json:"enabled"`
	Running     bool     `json:"running"`
	Healthy     bool     `json:"healthy"`
	Failed      bool     `json:"failed"`
	Port        int      `json:"port,omitempty"`
	PID         int      `json:"pid,omitempty"`
	StartedAt   int64    `json:"started_at,omitempty"`
	InstalledAt int64    `json:"installed_at,omitempty"`
	LastError   string   `json:"last_error,omitempty"`
}

// ClientKey is what the UI sees; the secret is never returned.
type ClientKey struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Prefix     string `json:"prefix"`
	Enabled    bool   `json:"enabled"`
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt int64  `json:"last_used_at,omitempty"`
	// Scope and quota (plan 2026-10-21 §1, mirroring A's ApiKeyEntry in
	// api_keys.rs:254-293). An empty ModelScope means every model, an empty
	// BindPool means every pool and DailyQuota 0 means unlimited.
	ModelScope []string `json:"model_scope"`
	BindPool   string   `json:"bind_pool"`
	DailyQuota int      `json:"daily_quota"`
	// UsedToday is the request counter of the current local calendar day.
	// QuotaLeft is -1 when the key is unlimited, else max(0, quota-used).
	UsedToday int `json:"used_today"`
	QuotaLeft int `json:"quota_left"`
}

// Proxy is a user-configured upstream egress proxy.
type Proxy struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Type          string `json:"type"` // http | socks5
	Host          string `json:"host"`
	Port          int    `json:"port"`
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	Enabled       bool   `json:"enabled"`
	LastCheckAt   int64  `json:"last_check_at,omitempty"`
	LastExitIP    string `json:"last_exit_ip,omitempty"`
	LastLatencyMS int64  `json:"last_latency_ms,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	// Managed marks the reserved row the host owns. It and ManagedDetail are
	// derived on read and never stored; the UI hides edit/delete for that row.
	Managed       string `json:"managed,omitempty"`
	ManagedDetail string `json:"managed_detail,omitempty"`
}

// EgressFile is what the host writes for a plugin.
type EgressFile struct {
	Version  int                    `json:"version"`
	Strict   bool                   `json:"strict"`
	Default  EgressEntry            `json:"default"`
	Accounts map[string]EgressEntry `json:"accounts"`
}

type EgressEntry struct {
	Mode    string `json:"mode"`
	URL     string `json:"url,omitempty"`
	ProxyID string `json:"proxy_id,omitempty"`
}

type AccountEgress struct {
	PluginID  string `json:"plugin_id"`
	AccountID string `json:"account_id"`
	Mode      string `json:"mode"`
	ProxyID   string `json:"proxy_id,omitempty"`
}

// APIRequestLog is one row of the request log.
type APIRequestLog struct {
	TS               string `json:"ts"`
	ReqID            string `json:"req_id"`
	Source           string `json:"source"`
	Model            string `json:"model"`
	LogicalModel     string `json:"logical_model,omitempty"`
	KeyID            string `json:"key_id"`
	KeyName          string `json:"key_name"`
	Account          string `json:"account,omitempty"`
	ConversationID   string `json:"conversation_id,omitempty"`
	Stream           bool   `json:"stream"`
	Status           int    `json:"status"`
	LatencyMS        int64  `json:"latency_ms"`
	TTFBMS           int64  `json:"ttfb_ms,omitempty"`
	FirstTokenAt     string `json:"first_token_at,omitempty"`
	PromptTokens     int64  `json:"prompt_tokens,omitempty"`
	CompletionTokens int64  `json:"completion_tokens,omitempty"`
	// CachedTokens is the cached prompt prefix reported by the upstream
	// (OpenAI prompt_tokens_details.cached_tokens / Anthropic
	// cache_read_input_tokens). Zero means the upstream did not report it.
	CachedTokens int64    `json:"cached_tokens,omitempty"`
	Credits      *float64 `json:"credits,omitempty"`
	Error        string   `json:"error,omitempty"`
	BodyReq      string   `json:"body_req,omitempty"`
	BodyResp     string   `json:"body_resp,omitempty"`
}

// ProxyLog is one tunnelled CONNECT through a local egress forwarder. It
// records who dialled (plugin + account), through which proxy, to which
// target, and how it ended. Only metadata is recorded: the tunnel payload is
// TLS and stays opaque, and the upstream proxy credentials are never copied
// here.
type ProxyLog struct {
	ID         int64  `json:"id"`
	TS         string `json:"ts"`
	At         int64  `json:"at"`
	PluginID   string `json:"plugin_id"`
	AccountID  string `json:"account_id"`
	ProxyID    string `json:"proxy_id"`
	ProxyLabel string `json:"proxy_label,omitempty"`
	TargetHost string `json:"target_host"`
	TargetPort int    `json:"target_port"`
	// Result is "ok", "denied" (the upstream proxy refused the tunnel) or
	// "failed" (dial/handshake failure).
	Result     string `json:"result"`
	Status     int    `json:"status,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	BytesUp    int64  `json:"bytes_up"`
	BytesDown  int64  `json:"bytes_down"`
	Error      string `json:"error,omitempty"`
}

// Proxy log results.
const (
	ProxyResultOK     = "ok"
	ProxyResultDenied = "denied"
	ProxyResultFailed = "failed"
)

type CheckinRecord struct {
	ID            int64    `json:"id"`
	PluginID      string   `json:"plugin_id"`
	AccountID     string   `json:"account_id"`
	AccountLabel  string   `json:"account_label,omitempty"`
	Date          string   `json:"date"`
	Result        string   `json:"result"`
	CreditsBefore *float64 `json:"credits_before,omitempty"`
	CreditsAfter  *float64 `json:"credits_after,omitempty"`
	Message       string   `json:"message,omitempty"`
	At            int64    `json:"at"`
	// DedupScope/DeviceKey persist the upstream dedup semantics of the row
	// (plan 2026-10-21 §4). Empty means the plugin did not report them.
	DedupScope string `json:"dedup_scope,omitempty"`
	DeviceKey  string `json:"device_key,omitempty"`
}

type QuotaSnapshot struct {
	PluginID  string  `json:"plugin_id"`
	AccountID string  `json:"account_id"`
	Remaining float64 `json:"remaining"`
	Total     float64 `json:"total"`
	ExpiresAt int64   `json:"expires_at,omitempty"`
	Plan      string  `json:"plan,omitempty"`
	At        int64   `json:"at"`
}

// Rule packs: literal replacement plus regex strips, applied inside plugins.
type RulePack struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Rules       []Rule         `json:"rules,omitempty"`
	Strip       []StripPattern `json:"strip,omitempty"`
}

type Rule struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type StripPattern struct {
	Pattern string `json:"pattern"`
}

type MergedRules struct {
	Packs []string       `json:"packs"`
	Rules []Rule         `json:"rules"`
	Strip []StripPattern `json:"strip"`
}

// HostStatus is shown on the dashboard.
type HostStatus struct {
	Version        string `json:"version"`
	Port           int    `json:"port"`
	DataDir        string `json:"data_dir"`
	AConfigured    bool   `json:"a_configured"`
	ARunning       bool   `json:"a_running"`
	APort          int    `json:"a_port,omitempty"`
	AState         string `json:"a_state,omitempty"`
	ADetail        string `json:"a_detail,omitempty"`
	PluginsRunning int    `json:"plugins_running"`
	PluginsTotal   int    `json:"plugins_total"`
	// EgressAssigned counts accounts with an explicit proxy assignment;
	// EgressFailed is the subset whose proxy failed inside the evidence window.
	EgressAssigned int `json:"egress_assigned"`
	EgressFailed   int `json:"egress_failed"`
}
