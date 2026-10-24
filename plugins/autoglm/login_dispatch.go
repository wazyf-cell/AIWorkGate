package main

import (
	"errors"
	"net/http"
	"strings"
)

// methodForValues resolves which branch a submit belongs to. An explicit
// method/marker in the values wins over the flow's original method, so the
// dialog can switch branches without a new session.
func methodForValues(current string, v map[string]string) string {
	if strings.TrimSpace(v["callback_url"]) != "" {
		return "global_oauth"
	}
	if v["action"] == "import" || v["method"] == "global_import_desktop" {
		return "global_import_desktop"
	}
	if strings.TrimSpace(v["phone"]) != "" {
		return "phone_sms"
	}
	switch current {
	case "global_import_desktop", "global_oauth":
		return current
	}
	return ""
}

// runLoginStep dispatches one method. An empty accountID means "not finished".
func (s *server) runLoginStep(flow *loginFlow, method string, v map[string]string) (accountID, message string, err error) {
	device := flow.DeviceID
	if device == "" {
		device = newDeviceID()
		flow.DeviceID = device
	}
	switch method {
	case "global_oauth":
		return s.loginGlobalOAuth(v["callback_url"])
	case "global_import_desktop":
		return s.loginGlobalImportDesktop()
	default:
		phone := strings.TrimSpace(v["phone"])
		code := strings.TrimSpace(v["code"])
		if code == "" {
			if err := s.loginCNSendCode(device, phone); err != nil {
				return "", "", err
			}
			return "", "验证码已发送（" + maskPhone(phone) + "），请填写收到的验证码", nil
		}
		return s.loginCN(device, phone, code)
	}
}

// loginErrorStatus maps a login failure onto the HTTP status the host shows.
func loginErrorStatus(err error) (int, string) {
	var ce *loginConfigError
	if errors.As(err, &ce) {
		return http.StatusNotImplemented, "not_implemented"
	}
	var de *loginInputError
	if errors.As(err, &de) {
		return http.StatusBadRequest, "invalid_request"
	}
	var le *loginLocalError
	if errors.As(err, &le) {
		return http.StatusNotFound, "desktop_data_missing"
	}
	var ue *UpstreamError
	if errors.As(err, &ue) {
		if ue.Kind == "soft_rate" {
			return http.StatusTooManyRequests, "upstream_rate_limited"
		}
		// The upstream answers HTTP 200 with a business code for a bad phone or
		// verification code (400001 …). Those are the operator's input, not a
		// broken plugin, so the host must not turn them into a 502.
		if ue.Kind == "client" {
			return http.StatusBadRequest, "invalid_request"
		}
		return http.StatusBadGateway, "upstream_error"
	}
	return http.StatusBadGateway, "login_failed"
}

// maskPhone keeps the last four digits; the full number is never echoed.
func maskPhone(phone string) string {
	phone = strings.TrimSpace(phone)
	if len(phone) <= 4 {
		return "****"
	}
	return strings.Repeat("*", len(phone)-4) + phone[len(phone)-4:]
}

// uidOf strips the region prefix from "cn/<uid>".
func uidOf(accountID string) string {
	if i := strings.Index(accountID, "/"); i >= 0 {
		return accountID[i+1:]
	}
	return accountID
}
