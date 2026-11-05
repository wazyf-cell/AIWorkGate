package opchost

import (
	"net/url"
	"time"

	"aiworkgate/internal/model"
)

// Contract paths spoken by a plugin, and therefore by the bridge. They mirror
// internal/plugins/client.go and supervisor.go.
const (
	PathHealth   = "/health"
	PathMeta     = "/meta"
	PathModels   = "/v1/models"
	PathAccounts = "/accounts"
)

// CheckinPath returns the check-in path of one account, POST /accounts/{id}/checkin,
// exactly as internal/plugins/client.go Checkin builds it (url.PathEscape on the
// account id). There is no flat /checkin endpoint in the host contract: the
// host only calls the per-account path.
func CheckinPath(accountID string) string {
	return "/accounts/" + url.PathEscape(accountID) + "/checkin"
}

// Timeouts mirrored from internal/plugins/client.go (control/check-in/identity/
// response-header/max-body) and internal/plugins/supervisor.go (health). The
// bridge must answer within the same budget as any other plugin.
//
// Note on streaming: GET /v1/chat/completions is copied through by the gateway,
// which bounds the upstream response headers with ResponseHeaderTimeout and
// abandons a body that stays silent for 5 minutes, but applies no overall client
// timeout to a stream. Of the constants below only ResponseHeaderTimeout and
// MaxResponseBytes apply to a stream.
const (
	HealthTimeout         = 2 * time.Second
	ControlTimeout        = 60 * time.Second
	CheckinTimeout        = 5 * time.Minute
	IdentityTimeout       = 20 * time.Second
	ResponseHeaderTimeout = 90 * time.Second
	MaxResponseBytes      = 8 << 20
)

// Meta is the runtime information returned by GET /meta. It mirrors the Meta
// type of internal/plugins/client.go field for field.
type Meta struct {
	ID           string   `json:"id"`
	Name         string   `json:"name,omitempty"`
	Version      string   `json:"version"`
	APILevel     int      `json:"api_level"`
	Capabilities []string `json:"capabilities,omitempty"`
	AccountCount int      `json:"account_count"`
	UptimeS      int64    `json:"uptime_s"`
	StartedAt    int64    `json:"started_at,omitempty"`
}

// Health is the optional JSON body of GET /health. The host's health check
// (internal/plugins/supervisor.go checkHealth) accepts any 2xx answer and only
// rejects ok=false or a non-"ok" status, so both fields are optional and extra
// fields are ignored.
type Health struct {
	OK     *bool  `json:"ok,omitempty"`
	Status string `json:"status,omitempty"`
}

// Contract types for /meta, /v1/models, /accounts and /checkin. They are aliases
// onto internal/model, so the bridge and the host share exactly one definition;
// the only structs defined in this file are Meta and Health, which live in
// internal/plugins and would make a plugins -> opchost dependency a cycle if
// imported here.
//
// /v1/models is decoded by the host through model.ParseModelList, which accepts
// {"data":[...]}, {"models":[...]} and a bare array; a bridge should answer with
// one of those three shapes.
type (
	// Account is one row of GET /accounts.
	Account = model.Account
	// AccountsResponse is the envelope of GET /accounts: {"accounts":[...]}.
	AccountsResponse = model.AccountsResponse
	// ModelInfo is one row of GET /v1/models.
	ModelInfo = model.ModelInfo
	// CheckinResult is the answer of POST /accounts/{id}/checkin
	// (internal/model/model.go:228-245): result is ok|already|fail, plus the
	// optional credits_before/credits_after/message/at and, from contract v0.4,
	// kind/reset_at/dedup_scope/device_key.
	CheckinResult = model.CheckinResult
)

// CheckinRequest is the body of POST /accounts/{id}/checkin. The host always
// sends an empty JSON object (internal/plugins/client.go Checkin), so a bridge
// must accept {} here; the struct exists to make that explicit.
type CheckinRequest struct{}
