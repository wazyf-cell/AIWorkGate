// Package main — device identity for Raccoon.
//
// What is real here, and what is not:
//
//   - device_id is the reference's own credential field. raccoonHeaders() sends
//     it as X-Client-Device-ID on the /api/web/auth/v1 calls (user_info is the
//     one this plugin makes). So the value below is genuinely seen upstream.
//   - machine_id and session_id have no Raccoon header in the reference: chat
//     and the catalog send neither, and no product field carries a machine id.
//     They are therefore local, randomly generated identifiers, reported with
//     source "random" so nobody mistakes them for an OS fingerprint.
//   - user_agent is the User-Agent this plugin actually sends (userAgent).
//
// Only `identity_request` is declared in the manifest: mode "os" would mean
// switching the machine-level OS identity, which this plugin does not touch, so
// declaring identity_os_switch would be a lie.
package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// deviceIdentity is the identity block reported inside each account.
type deviceIdentity struct {
	MachineID string `json:"machine_id,omitempty"`
	DeviceID  string `json:"device_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Source    string `json:"source,omitempty"`
	UpdatedAt int64  `json:"updated_at,omitempty"`
}

func (d *deviceIdentity) deviceID() string {
	if d == nil {
		return ""
	}
	return strings.TrimSpace(d.DeviceID)
}

// newIdentity mints a fresh identity at login time.
func newIdentity() *deviceIdentity {
	now := time.Now().Unix()
	return &deviceIdentity{
		MachineID: randomHex(16),
		DeviceID:  randomHex(16),
		SessionID: randomHex(16),
		Source:    "random",
		UpdatedAt: now,
	}
}

// rotate returns a copy with fresh request-level identifiers. It never touches
// the machine-level id: this plugin has no OS identity capability (manifest
// permissions.os_identity=false), and machine_id is a local reporting value, not
// something the OS or the upstream sees. A new value is returned instead of
// mutating in place so a failed store write cannot leave the process and the
// file disagreeing.
func (d *deviceIdentity) rotate() *deviceIdentity {
	next := &deviceIdentity{
		DeviceID:  randomHex(16),
		SessionID: randomHex(16),
		Source:    "random",
		UpdatedAt: time.Now().Unix(),
	}
	if d != nil {
		next.MachineID = d.MachineID
	}
	if next.MachineID == "" {
		next.MachineID = randomHex(16)
	}
	return next
}

// identityView is the JSON shape the host reads out of /accounts.
func identityView(d *deviceIdentity) map[string]any {
	if d == nil {
		return nil
	}
	return map[string]any{
		"machine_id": d.MachineID,
		"device_id":  d.DeviceID,
		"session_id": d.SessionID,
		"user_agent": userAgent,
		"source":     d.Source,
		"updated_at": d.UpdatedAt,
	}
}

// handleAccountAction serves POST /accounts/{id}/identity/reset and
// POST /accounts/{id}/refresh.
func (s *server) handleAccountAction(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/accounts/"), "/")
	parts := strings.Split(rest, "/")
	if len(parts) == 2 && parts[1] == "refresh" {
		s.handleRefresh(w, r, parts[0])
		return
	}
	if len(parts) != 3 || parts[1] != "identity" || parts[2] != "reset" {
		writeError(w, http.StatusNotFound, "no_such_endpoint",
			"unknown account endpoint: "+r.URL.Path)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	accountID := parts[0]
	if accountID != "default" {
		writeError(w, http.StatusNotFound, "unknown_account", "Raccoon 只有一个账号（id=default）")
		return
	}
	var in struct {
		Mode string `json:"mode"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in)
	mode := strings.ToLower(strings.TrimSpace(in.Mode))
	if mode == "" {
		mode = "request"
	}
	if mode == "os" {
		// Declaring identity_request is not a licence to pretend: switching a
		// machine-level identity needs identity_os_switch + permissions.os_identity,
		// neither of which this plugin has. Refuse instead of rotating a local
		// random value and calling that an OS identity switch.
		writeError(w, http.StatusNotImplemented, "identity_os_not_supported",
			"Raccoon 只有请求级身份（manifest 声明 identity_request、permissions.os_identity=false）："+
				"本插件不改写整机/客户端身份，mode=os 不会执行，也不假装成功")
		return
	}
	if mode != "request" {
		writeError(w, http.StatusBadRequest, "bad_mode", `mode 只能是 "request" 或 "os"`)
		return
	}
	creds, ok := s.store.get()
	if !ok {
		writeError(w, http.StatusConflict, "not_configured", "尚未登录，没有可重置的设备身份")
		return
	}
	if creds.Identity == nil {
		creds.Identity = newIdentity()
	}
	creds.Identity = creds.Identity.rotate()
	if err := s.store.set(creds); err != nil {
		writeError(w, http.StatusInternalServerError, "store_failed", "身份已轮换但落盘失败："+err.Error())
		return
	}
	// Report exactly what changed. device_id/session_id/machine_id are local
	// values shown in /accounts: chat and model_catalog send none of them, and
	// user_info sends only device_id as X-Client-Device-ID, during a scan login
	// that mints a fresh identity anyway. So the reset has no upstream effect and
	// must not claim one.
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"mode":            mode,
		"changed":         []string{"device_id", "session_id"},
		"unchanged":       []string{"machine_id"},
		"upstream_effect": "none",
		"identity":        identityView(creds.Identity),
		"note": "只更换本插件保存、并在 /accounts 展示的请求级标识（device_id、session_id）。" +
			"chat 与 model_catalog 不携带这些字段；device_id 只出现在 user_info 的 X-Client-Device-ID 上，" +
			"而 user_info 仅在下一次扫码登录时调用，届时会重新生成一套标识。" +
			"因此本次重置不改变任何上游请求，也不会让当前 access_token 失效。",
	})
}
