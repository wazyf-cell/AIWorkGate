package main

// Login flows: the upstream calls behind one submit.
//
// Ported from the Go reference (internal/autoclaw/engine.go
// loginGlobalImportDesktop / loginGlobalOAuth and upstream.go sendCode/login).
// Every branch either persists a credential through store.upsert or returns a
// typed error; nothing reports success before the account is in the store.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Typed failures let loginErrorStatus pick the HTTP status. Messages are for the
// operator and never carry tokens.
type loginConfigError struct{ msg string }

func (e *loginConfigError) Error() string { return e.msg }

type loginInputError struct{ msg string }

func (e *loginInputError) Error() string { return e.msg }

type loginLocalError struct{ msg string }

func (e *loginLocalError) Error() string { return e.msg }

// loginCNSendCode asks the cn region to text a one-time code.
func (s *server) loginCNSendCode(deviceID, phone string) error {
	phone = strings.TrimSpace(phone)
	if len(phone) < 6 {
		return &loginInputError{"手机号格式不正确"}
	}
	if err := s.up.sendCode(regionCN, deviceID, phone); err != nil {
		return err
	}
	return nil
}

// loginCN exchanges phone+code for a cn-region credential.
func (s *server) loginCN(deviceID, phone, code string) (string, string, error) {
	phone = strings.TrimSpace(phone)
	if phone == "" || strings.TrimSpace(code) == "" {
		return "", "", &loginInputError{"手机号和验证码都要填写"}
	}
	res, err := s.up.login(regionCN, deviceID, phone, code)
	if err != nil {
		return "", "", err
	}
	uid := strings.TrimSpace(fmt.Sprintf("%v", res.UserID))
	if uid == "" || uid == "<nil>" {
		return "", "", &UpstreamError{Kind: "client", Msg: "登录响应缺少 user_id"}
	}
	exp := jwtClaimInt(res.AccessToken, "exp")
	if exp == 0 {
		exp = time.Now().Unix() + accessTokenTTL
	}
	label := firstNonEmpty(res.UserName, maskPhone(phone))
	a := newAccount(accountData{
		AccessToken:  bearer(res.AccessToken),
		RefreshToken: res.RefreshToken,
		ExpiresAt:    exp,
		UserID:       uid,
		UserName:     label,
		Phone:        phone,
		DeviceID:     deviceID,
		Region:       regionCN,
	})
	if err := s.persistLogin(a); err != nil {
		return "", "", err
	}
	return stateKey(regionCN, uid), label, nil
}

// loginGlobalOAuth exchanges a pasted Google callback URL for a global account.
func (s *server) loginGlobalOAuth(callbackURL string) (string, string, error) {
	callbackURL = strings.TrimSpace(callbackURL)
	if callbackURL == "" {
		return "", "", &loginInputError{"请粘贴浏览器完成授权后的完整回调地址"}
	}
	u, err := url.Parse(callbackURL)
	if err != nil {
		return "", "", &loginInputError{"回调地址无法解析：" + err.Error()}
	}
	code, state := u.Query().Get("code"), u.Query().Get("state")
	if code == "" || state == "" {
		return "", "", &loginInputError{"回调地址缺少 code 或 state"}
	}
	deviceID := newDeviceID()
	full := s.up.base(regionGlobal) + "/userapi/overseasv1/google-oauth-login"
	raw, err := s.up.doJSON(http.MethodPost, "", full, userapiHeaders(""),
		map[string]any{"source_id": "autoclaw", "device_id": deviceID, "code": code,
			"state": state, "platform": "web"}, true)
	if err != nil {
		return "", "", err
	}
	var env map[string]any
	if json.Unmarshal(raw, &env) != nil {
		return "", "", &UpstreamError{Kind: "client", Msg: "oauth-login 响应不是 JSON"}
	}
	data, _ := env["data"].(map[string]any)
	if data == nil {
		data = env
	}
	token := firstNonEmpty(toString(data["access_token"]), toString(env["access_token"]))
	if token == "" {
		return "", "", &UpstreamError{Kind: "client", Msg: "oauth-login 未返回 access_token"}
	}
	uid := strings.TrimSpace(toString(jwtPayloadField(token, "user_id")))
	if uid == "" || uid == "<nil>" {
		return "", "", &UpstreamError{Kind: "client", Msg: "token 缺少 user_id"}
	}
	a := newAccount(accountData{
		AccessToken:  bearer(token),
		RefreshToken: firstNonEmpty(toString(data["refresh_token"]), toString(env["refresh_token"])),
		ExpiresAt:    time.Now().Unix() + 28*86400,
		UserID:       uid,
		UserName:     "global-oauth",
		DeviceID:     deviceID,
		Region:       regionGlobal,
	})
	if err := s.persistLogin(a); err != nil {
		return "", "", err
	}
	return stateKey(regionGlobal, uid), a.snapshot().UserName, nil
}

// loginGlobalImportDesktop imports the JWT the desktop client already holds.
// Absence of the directory is a readable 404, never a silent success.
func (s *server) loginGlobalImportDesktop() (string, string, error) {
	jwt, err := desktopJWT()
	if err != nil {
		return "", "", err
	}
	uid := strings.TrimSpace(toString(jwtPayloadField(jwt, "user_id")))
	if uid == "" || uid == "<nil>" {
		return "", "", &loginLocalError{"桌面端 JWT 缺少 user_id，无法建立账号"}
	}
	exp := jwtClaimInt(jwt, "exp")
	if exp == 0 {
		exp = time.Now().Unix() + 86400
	}
	a := newAccount(accountData{
		AccessToken: "Bearer " + jwt,
		ExpiresAt:   exp,
		UserID:      uid,
		UserName:    "global-desktop",
		DeviceID:    toString(jwtPayloadField(jwt, "device_id")),
		Region:      regionGlobal,
	})
	if err := s.persistLogin(a); err != nil {
		return "", "", err
	}
	return stateKey(regionGlobal, uid), "global-desktop", nil
}

// desktopJWT reads the newest X-Authorization the desktop client wrote.
func desktopJWT() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", &loginLocalError{"无法定位用户主目录：" + err.Error()}
	}
	dir := filepath.Join(home, ".openclaw-autoclaw")
	best, bestIAT := "", int64(-1)
	for _, name := range []string{"request-headers.json", "openclaw.json"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var doc map[string]any
		if json.Unmarshal(raw, &doc) != nil {
			continue
		}
		if headers, ok := doc["headers"].(map[string]any); ok {
			if tok := stripBearer(toString(headers["X-Authorization"])); tok != "" {
				return tok, nil
			}
		}
		models, _ := doc["models"].(map[string]any)
		providers, _ := models["providers"].(map[string]any)
		zai, _ := providers["zai"].(map[string]any)
		list, _ := zai["models"].([]any)
		for _, item := range list {
			m, _ := item.(map[string]any)
			h, _ := m["headers"].(map[string]any)
			tok := stripBearer(toString(h["X-Authorization"]))
			if tok == "" {
				continue
			}
			if iat := jwtClaimInt(tok, "iat"); iat > bestIAT {
				best, bestIAT = tok, iat
			}
		}
	}
	if best == "" {
		return "", &loginLocalError{"未找到 " + dir + "\\request-headers.json 或 openclaw.json 中的桌面端 JWT；请先登录 AutoGLM 桌面客户端"}
	}
	return best, nil
}

// persistLogin stores the credential and reads the real balance once.
func (s *server) persistLogin(a *account) error {
	if err := s.st.upsert(a); err != nil {
		return fmt.Errorf("保存账号失败：%w", err)
	}
	if pts, err := s.up.checkPoints(a); err == nil {
		s.st.setPoints(a, pts)
	}
	return nil
}
