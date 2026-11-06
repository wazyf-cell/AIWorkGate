// Package main — the signed 讯飞 account calls behind the WeChat scan.
//
// Ported from loomy-oauth.ts (postLoomyAccount / bindLoomyThirdAccount /
// bindLoomySkip) and loomy-sign.ts. The signing pair is read ONLY from the
// operator's environment (LOOMY_XFYUN_ACCESS_KEY_ID / LOOMY_XFYUN_ACCESS_KEY_SECRET,
// see sign.go); this file contains no credential and no default for one.
//
// ⚠️ The body is serialized ONCE and the same bytes are both signed and sent
// (postLoomyAccount): re-serializing would change the byte order/spacing and
// invalidate the signature.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Signed account paths (loomy-oauth.ts).
const (
	pathBindAuth     = "/login/thirdAccount/bind/auth"
	pathBindSkip     = "/login/thirdAccount/bind/skip"
	pathBindSendMsg  = "/login/thirdAccount/bind/sendMsg"
	pathBindCheckCde = "/login/thirdAccount/bind/checkCode"
)

// accountEndpoint is accountBase. A variable so an end-to-end test can stand in
// for the 讯飞 account API; production keeps the reference's origin.
var accountEndpoint = accountBase

// appID is LOOMY_APP_ID: an application identifier, not a secret.
const appID = "GM3LOOMY"

// accountBody is buildLoomyAccountBody: base + param, with the reference's
// hard-coded desktop fingerprint (ua stays "macOS" on Windows on purpose).
func accountBody(param map[string]any) map[string]any {
	return map[string]any{
		"base": map[string]any{
			"appid":   appID,
			"modelid": "Web",
			"version": "1.0.0",
			"devid":   "web",
			"ua":      "Loomy|Desktop|Electron|macOS",
			"traceid": randomHex(16),
		},
		"param": param,
	}
}

// signingEnvHint is the actionable sentence shown whenever the signing pair is
// missing: it names both variables so the operator never has to read source.
func signingEnvHint() string {
	return "请设置环境变量 " + envAccessKeyID + " 与 " + envAccessKeySecret + " 后重启宿主"
}

// postSigned is postLoomyAccount: one signed POST to the account API, with the
// business envelope unwrapped. A non-"000000" code is an error even though the
// HTTP status is 200 — Loomy reports failures in the body.
func (s *server) postSigned(ctx context.Context, path string, param map[string]any) ([]byte, error) {
	if !s.signing.complete() {
		return nil, fmt.Errorf("微信扫码后的讯飞换票需要签名密钥：%s", signingEnvHint())
	}
	body, err := json.Marshal(accountBody(param))
	if err != nil {
		return nil, err
	}
	// One serialization, shared by the signature and the request body.
	payload := string(body)
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, accountEndpoint+path, strings.NewReader(payload))
	if err != nil {
		return nil, err
	}
	for k, vs := range signLoomy(s.signing, http.MethodPost, path, nil, payload, time.Now()) {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	req.Header.Set("User-Agent", wechatUA)
	resp, err := s.client.httpFor("").Do(req)
	if err != nil {
		return nil, fmt.Errorf("讯飞账号请求失败（%s）：%w", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	ok, code, message, data := parseEnvelope(raw)
	if !ok {
		if message == "" {
			message = fmt.Sprintf("讯飞账号请求失败（%s，HTTP %d）", path, resp.StatusCode)
		}
		return nil, fmt.Errorf("%s（code=%s）", message, code)
	}
	return data, nil
}

// bindAuthAnswer is step 1's response: whether 讯飞 already knows this WeChat
// account and the rcode every later step carries.
type bindAuthAnswer struct {
	// Bind is 1 when the WeChat account already has a 讯飞 phone binding, which
	// lets bind/skip mint the session directly; 0 means a phone still has to be
	// bound. A missing value is treated as 0 (the reference's conservative
	// choice): worst case the user binds once more, never a session without a
	// phone.
	Bind     int    `json:"bind"`
	RCode    string `json:"rcode"`
	IsNew    int    `json:"isnew"`
	Nickname string `json:"nickname"`
}

// bindAuth is bindLoomyThirdAccount: exchange the one-time wx_code for an rcode.
func (s *server) bindAuth(ctx context.Context, code string) (bindAuthAnswer, error) {
	raw, err := s.postSigned(ctx, pathBindAuth, map[string]any{
		"tcode": map[string]any{"code": code},
		"type":  "wx",
	})
	if err != nil {
		return bindAuthAnswer{}, err
	}
	var out bindAuthAnswer
	if err := json.Unmarshal(raw, &out); err != nil {
		return bindAuthAnswer{}, fmt.Errorf("微信授权响应不是预期 JSON：%w", err)
	}
	if strings.TrimSpace(out.RCode) == "" {
		return bindAuthAnswer{}, fmt.Errorf("微信授权响应缺少 rcode（无法继续登录）")
	}
	return out, nil
}

// loginResult is the session a successful exchange yields.
type loginResult struct {
	Session string `json:"session"`
	UserID  string `json:"userid"`
	Phone   string `json:"phone"`
}

// bindSkip is bindLoomySkip: mint the session for an already-bound WeChat user.
func (s *server) bindSkip(ctx context.Context, rcode string) (loginResult, error) {
	raw, err := s.postSigned(ctx, pathBindSkip, map[string]any{
		"rcode":  rcode,
		"expire": sessionTTLSeconds,
	})
	if err != nil {
		return loginResult{}, err
	}
	var out loginResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return loginResult{}, fmt.Errorf("微信登录响应不是预期 JSON：%w", err)
	}
	if strings.TrimSpace(out.Session) == "" {
		return loginResult{}, fmt.Errorf("微信登录响应缺少 session")
	}
	if strings.TrimSpace(out.UserID) == "" {
		return loginResult{}, fmt.Errorf("微信登录响应缺少 userid")
	}
	return out, nil
}

// bindSendMsg is bindLoomySendMsg: send the SMS code to the phone being bound.
func (s *server) bindSendMsg(ctx context.Context, rcode, phone string) (string, error) {
	raw, err := s.postSigned(ctx, pathBindSendMsg, map[string]any{
		"rcode":  rcode,
		"phone":  phone,
		"ccode":  "86",
		"expire": 300,
	})
	if err != nil {
		return "", err
	}
	var out struct {
		MsgID string `json:"msgid"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("绑定手机号响应不是预期 JSON：%w", err)
	}
	if strings.TrimSpace(out.MsgID) == "" {
		return "", fmt.Errorf("绑定手机号响应缺少 msgid")
	}
	return out.MsgID, nil
}

// bindCheckCode is bindLoomyCheckCode: verify the SMS code, which both binds the
// phone and completes the login.
func (s *server) bindCheckCode(ctx context.Context, rcode, mcode, msgid string) (loginResult, error) {
	raw, err := s.postSigned(ctx, pathBindCheckCde, map[string]any{
		"rcode":  rcode,
		"mcode":  mcode,
		"msgid":  msgid,
		"expire": sessionTTLSeconds,
	})
	if err != nil {
		return loginResult{}, err
	}
	var out loginResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return loginResult{}, fmt.Errorf("绑定登录响应不是预期 JSON：%w", err)
	}
	if strings.TrimSpace(out.Session) == "" {
		return loginResult{}, fmt.Errorf("绑定登录响应缺少 session")
	}
	if strings.TrimSpace(out.UserID) == "" {
		return loginResult{}, fmt.Errorf("绑定登录响应缺少 userid")
	}
	return out, nil
}

// expiresAtMs computes the credential expiry the reference stores: the login
// moment plus LOOMY_SESSION_TTL_SECONDS, as a millisecond string. The server
// never sends one — `expire` is only a declaration.
func expiresAtMs(now time.Time) string {
	return strconv.FormatInt(now.Add(sessionTTLSeconds*time.Second).UnixMilli(), 10)
}

// persistSession stores the 讯飞 session through the existing credential path
// (store.set → DATA_DIR\accounts.dat, DPAPI-sealed). The token is never logged.
func (s *server) persistSession(res loginResult, label string) error {
	prev, _ := s.store.get()
	creds := credentials{
		// access_token holds the 32-hex 讯飞 session, exactly the field the
		// reference's account pool identifies a credential by.
		AccessToken: res.Session,
		UserID:      res.UserID,
		Session:     res.Session,
		ExpiresAt:   expiresAtMs(time.Now()),
		BaseURL:     prev.BaseURL,
		Label:       strings.TrimSpace(label),
	}
	if creds.Label == "" {
		creds.Label = strings.TrimSpace(res.Phone)
	}
	if creds.Label == "" {
		creds.Label = "Loomy"
	}
	return s.store.set(creds)
}

// exchangeCode runs step 1 and, when the account is already phone-bound, step 4.
//
// bind == 0 means this WeChat has no 讯飞 phone binding yet. The reference
// finishes that with bindSendMsg + bindCheckCode driven from its own page; this
// plugin has no form for it in the host's login dialog (the poll contract carries
// no field definitions), so it says exactly that instead of pretending to
// succeed with a session it cannot obtain.
func (s *server) exchangeCode(ctx context.Context, code string) (loginResult, error) {
	auth, err := s.bindAuth(ctx, code)
	if err != nil {
		return loginResult{}, err
	}
	if auth.Bind != 1 {
		return loginResult{}, fmt.Errorf("该微信尚未绑定讯飞手机号，需要先在 Loomy 官方客户端用该微信登录并绑定手机号，" +
			"之后回到这里重新扫码即可（本插件无法代填手机号与短信验证码）")
	}
	return s.bindSkip(ctx, auth.RCode)
}
