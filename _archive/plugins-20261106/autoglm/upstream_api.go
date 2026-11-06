package main

// userapi account-domain endpoints: SMS code, phone login, token refresh, wallet
// balance, sandbox list/apply/ensure/wake.
//
// Ported from upstream.go of the reference. Every path string below is the
// reference's constant; none was inferred.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ---- account-domain endpoints (userapi) ----

func (u *upstream) sendCode(region, deviceID, phone string) error {
	_, err := u.doJSON(http.MethodPost, "", u.base(region)+"/userapi/v1/agent-send-code",
		userapiHeaders(""), map[string]any{"source_id": "autoclaw", "device_id": deviceID, "phone": phone}, false)
	return err
}

type loginResult struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	UserID       any    `json:"user_id"`
	UserName     string `json:"user_name"`
}

func (u *upstream) login(region, deviceID, phone, code string) (*loginResult, error) {
	codeNum := int64(0)
	fmt.Sscanf(strings.TrimSpace(code), "%d", &codeNum)
	raw, err := u.doJSON(http.MethodPost, "", u.base(region)+"/userapi/v1/agent-login/", userapiHeaders(""),
		map[string]any{"source_id": "autoclaw", "device_id": deviceID, "phone": phone, "code": codeNum, "platform": "web"}, false)
	if err != nil {
		return nil, err
	}
	var data loginResult
	if json.Unmarshal(raw, &data) != nil || strings.TrimSpace(data.AccessToken) == "" {
		return nil, upstreamErrf("client", 0, 0, "login_failed: no access_token")
	}
	return &data, nil
}

// refresh swaps a refreshToken for a new access token and writes it back into a.
// The caller persists a afterwards.
func (u *upstream) refresh(a *account) error {
	snap := a.snapshot()
	if strings.TrimSpace(snap.RefreshToken) == "" {
		return errors.New("no refreshToken")
	}
	raw, err := u.doJSON(http.MethodPost, stateKey(snap.Region, snap.UserID), u.base(snap.Region)+"/userapi/v1/refresh",
		userapiHeaders(snap.AccessToken),
		map[string]any{"source_id": "autoclaw", "device_id": snap.DeviceID, "refresh_token": snap.RefreshToken}, false)
	if err != nil {
		var ue *UpstreamError
		if errors.As(err, &ue) && ue.Kind != "none" {
			return upstreamErrf("session_dead", ue.Status, ue.Code, "refresh_failed: %s", ue.Msg)
		}
		return err
	}
	var data struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if json.Unmarshal(raw, &data) != nil || strings.TrimSpace(data.AccessToken) == "" {
		return upstreamErrf("session_dead", 401, 0, "refresh_failed: re-login required")
	}
	a.update(func(d *accountData) {
		d.AccessToken = data.AccessToken
		if data.RefreshToken != "" {
			d.RefreshToken = data.RefreshToken
		}
	})
	return nil
}

// checkPoints reads the wallet balance (the quota capability).
func (u *upstream) checkPoints(a *account) (int, error) {
	snap := a.snapshot()
	raw, err := u.doJSON(http.MethodGet, stateKey(snap.Region, snap.UserID),
		u.base(snap.Region)+"/agent-assetmgr/api/v2/wallets?biz_app_id=autoclaw",
		userapiHeaders(snap.AccessToken), nil, false)
	if err != nil {
		return 0, err
	}
	var data struct {
		TotalBalance any `json:"total_balance"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return 0, nil
	}
	return int(toInt64(data.TotalBalance)), nil
}

type sandboxInfo struct {
	SandboxID       string `json:"sandbox_id"`
	SandboxEndpoint string `json:"sandbox_endpoint"`
	EndTimestamp    int64  `json:"end_timestamp"`
}

func (u *upstream) listSandboxes(a *account) ([]sandboxInfo, error) {
	snap := a.snapshot()
	raw, err := u.doJSON(http.MethodGet, stateKey(snap.Region, snap.UserID),
		u.base(snap.Region)+"/agentdr/v2/assistant/sandbox/list",
		userapiHeaders(snap.AccessToken), nil, false)
	if err != nil {
		return nil, err
	}
	var lst []sandboxInfo
	if json.Unmarshal(raw, &lst) == nil && lst != nil {
		return lst, nil
	}
	var wrap struct {
		SandboxList []sandboxInfo `json:"sandbox_list"`
	}
	if json.Unmarshal(raw, &wrap) == nil {
		return wrap.SandboxList, nil
	}
	return nil, nil
}

func (u *upstream) applySandbox(a *account) error {
	snap := a.snapshot()
	_, err := u.doJSON(http.MethodPost, stateKey(snap.Region, snap.UserID), u.base(snap.Region)+"/agentdr/v2/assistant/sandbox/apply",
		userapiHeaders(snap.AccessToken), map[string]any{"sandbox_name": "default"}, false)
	return err
}

// ensureSandbox guarantees a live sandbox and normalises its endpoint to the
// relay proxy base.
func (u *upstream) ensureSandbox(a *account) (string, string, error) {
	now := time.Now().Unix()
	snap := a.snapshot()
	hasValid := snap.SandboxID != "" && snap.SandboxEndpoint != "" && (snap.EndTimestamp == 0 || snap.EndTimestamp > now)
	if hasValid {
		normalised := relayProxyBase(snap.SandboxEndpoint, snap.SandboxID)
		a.update(func(d *accountData) { d.SandboxEndpoint = normalised })
		return snap.SandboxID, normalised, nil
	}
	lst, err := u.listSandboxes(a)
	if err != nil {
		return "", "", err
	}
	if len(lst) == 0 {
		if err := u.applySandbox(a); err != nil {
			return "", "", err
		}
		lst, err = u.listSandboxes(a)
		if err != nil {
			return "", "", err
		}
		if len(lst) == 0 {
			return "", "", upstreamErrf("server", 0, 0, "no sandbox available after apply")
		}
	}
	pick := lst[0]
	for _, s := range lst {
		exp, pexp := s.EndTimestamp, pick.EndTimestamp
		if (exp == 0 || exp > now) && (pick.SandboxID == "" || (pexp != 0 && pexp <= now)) {
			pick = s
		}
	}
	if pick.SandboxID == "" {
		return "", "", upstreamErrf("server", 0, 0, "sandbox response missing sandbox_id")
	}
	proxy := relayProxyBase(pick.SandboxEndpoint, pick.SandboxID)
	a.update(func(d *accountData) {
		d.SandboxID = pick.SandboxID
		d.SandboxEndpoint = proxy
		d.EndTimestamp = pick.EndTimestamp
	})
	return pick.SandboxID, proxy, nil
}

// wakeSandbox extends the sandbox lease before a chat request.
func (u *upstream) wakeSandbox(a *account, sandboxID string) error {
	snap := a.snapshot()
	bare := relayBaseFromProxy(snap.SandboxEndpoint)
	_, err := u.relayJSON(stateKey(snap.Region, snap.UserID), snap.AccessToken, bare, http.MethodPost,
		"/v1/sandboxes/"+sandboxID+"/wake", map[string]any{
			"access_hold_ms":               600000,
			"reason":                       "autoclaw2api chat request",
			"apply_business_busy_cooldown": true,
		})
	return err
}
