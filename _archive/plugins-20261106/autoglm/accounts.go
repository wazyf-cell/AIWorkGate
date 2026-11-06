package main

// Account surface: listing with inlined quota, the per-account sub-resource
// (enable/disable, delete, checkin, refresh), and the operator credential intake.
//
// Everything here reads the scheduler view from pool.go and the credentials from
// store.go; no handler ever prints a token, only a fingerprint.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// handleAccounts reports every stored account with its scheduler state and
// quota. Quota is only filled when a balance was actually read; it is never
// fabricated.
func (s *server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	accounts := []map[string]any{}
	for _, row := range s.st.statusList() {
		status := "ok"
		if row.Disabled {
			status = "invalid"
		} else if row.Cooling {
			status = "rate_limited"
		}
		accounts = append(accounts, map[string]any{
			"id":      row.ID,
			"label":   firstNonEmpty(row.Nickname, row.UID),
			"enabled": !row.Disabled,
			"status":  status,
			"credential": map[string]any{
				"present":     true,
				"refreshable": s.refreshable(row.UID),
			},
			"quota": map[string]any{
				"remaining":    float64(row.Points),
				"rate_limited": row.Cooling,
			},
			"identity": map[string]any{
				"device_id": s.deviceID(row.UID),
				"source":    "derived",
			},
			"last_error": row.Reason,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
}

func (s *server) refreshable(uid string) bool {
	if a := s.st.byID(uid); a != nil {
		return strings.TrimSpace(a.snapshot().RefreshToken) != ""
	}
	return false
}

func (s *server) deviceID(uid string) string {
	if a := s.st.byID(uid); a != nil {
		return a.snapshot().DeviceID
	}
	return ""
}

// handleAccountSub dispatches /accounts/{id}[/{action}].
func (s *server) handleAccountSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/accounts/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "account id is required")
		return
	}
	// account ids are "region/uid", so a two-segment tail is the common case.
	id := parts[0]
	action := ""
	if len(parts) >= 3 {
		id = parts[0] + "/" + parts[1]
		action = parts[2]
	} else if len(parts) == 2 {
		// "region/uid" is the account id itself; only the known actions take
		// the second segment as a verb.
		switch parts[1] {
		case "checkin", "refresh":
			action = parts[1]
		default:
			id = parts[0] + "/" + parts[1]
		}
	}
	a := s.accountByID(id)
	if a == nil {
		writeError(w, http.StatusNotFound, "not_found", "account "+id+" is not stored")
		return
	}
	switch {
	case action == "checkin" && r.Method == http.MethodPost:
		res := s.engine.checkinOne(a, false)
		result := "ok"
		if !res.OK {
			result = "fail"
		} else if res.AlreadyDone {
			result = "already"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"result":  result,
			"message": firstNonEmpty(res.Msg, res.Error),
			"at":      nowUnix(),
		})
	case action == "refresh" && r.Method == http.MethodPost:
		s.engine.refreshOne(a)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case action == "" && r.Method == http.MethodPatch:
		var body struct {
			Enabled *bool `json:"enabled"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body)
		if body.Enabled != nil {
			if *body.Enabled {
				s.st.reenable(a)
			} else {
				s.st.disable(a, "manual disable (host)")
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case action == "" && r.Method == http.MethodDelete:
		if !s.st.remove(a.snapshot().UserID) {
			writeError(w, http.StatusNotFound, "not_found", "account "+id+" is not stored")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "unsupported action for "+r.URL.Path)
	}
}

func (s *server) accountByID(id string) *account {
	for _, a := range s.st.list() {
		snap := a.snapshot()
		if stateKey(snap.Region, snap.UserID) == id || snap.UserID == id {
			return a
		}
	}
	return nil
}

// handleAdminCredentials accepts an operator-pasted account document. The body
// may be the reference's autoclaw-<uid>.json verbatim (camelCase fields) or a
// flat object with access_token/user_id.
func (s *server) handleAdminCredentials(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST or PUT")
		return
	}
	var in struct {
		AccessToken     string `json:"accessToken"`
		AccessTokenAlt  string `json:"access_token"`
		RefreshToken    string `json:"refreshToken"`
		RefreshTokenAlt string `json:"refresh_token"`
		ExpiresAt       int64  `json:"expiresAt"`
		UserID          string `json:"userId"`
		UserIDAlt       any    `json:"user_id"`
		UserName        string `json:"userName"`
		Phone           string `json:"phone"`
		DeviceID        string `json:"deviceId"`
		Region          string `json:"region"`
		Label           string `json:"label"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	tok := firstNonEmpty(in.AccessToken, in.AccessTokenAlt)
	rt := firstNonEmpty(in.RefreshToken, in.RefreshTokenAlt)
	uid := firstNonEmpty(in.UserID, toString(in.UserIDAlt))
	if strings.TrimSpace(uid) == "" {
		uid = firstNonEmpty(toString(jwtPayloadField(tok, "user_id")), toString(jwtPayloadField(tok, "userId")))
	}
	if tok == "" || uid == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"accessToken and userId are required (userId can be omitted when the token is a JWT carrying user_id)")
		return
	}
	region := strings.TrimSpace(in.Region)
	if region != regionCN && region != regionGlobal {
		region = regionCN
	}
	exp := in.ExpiresAt
	if exp <= 0 {
		exp = jwtClaimInt(tok, "exp")
	}
	if exp <= 0 {
		exp = nowUnix() + accessTokenTTL
	}
	a := newAccount(accountData{
		AccessToken:  bearer(tok),
		RefreshToken: rt,
		ExpiresAt:    exp,
		UserID:       uid,
		UserName:     firstNonEmpty(in.UserName, in.Label),
		Phone:        in.Phone,
		DeviceID:     in.DeviceID,
		Region:       region,
	})
	if err := s.st.upsert(a); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	// A fresh credential deserves one real balance read, so /accounts is not
	// showing a stale zero. Failure here is not fatal.
	if pts, err := s.up.checkPoints(a); err == nil {
		s.st.setPoints(a, pts)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"configured":      true,
		"account_id":      stateKey(region, uid),
		"key_fingerprint": fingerprint(tok),
	})
}
