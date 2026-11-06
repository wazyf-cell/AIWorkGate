package main

import (
	"os"
	"strings"
)

// Environment variables that override individual CodeArts endpoints. They exist
// because the authorize entry point differs between Huawei's web portal and its
// desktop IDE plugin channel, and the right one for a given account is not
// something this plugin can detect.
//
// The defaults preserve the current behaviour: with no environment set,
// endpointsFromEnv returns a zero Endpoints and New() falls back to
// defaultEndpoints(). Every override is opt-in, so an existing install is
// unaffected and rolling back is just unsetting the variable.
const (
	envAuthorizeMode = "CODEARTS_AUTHORIZE_MODE"

	// authorizePortal is the CodeArts web portal entry (the historical default).
	authorizePortal = "https://codearts.huaweicloud.com/portal/authorize"

	// authorizeDoer is the desktop IDE plugin channel, which the reference
	// implementation uses for its JetBrains-style login:
	//   https://devcloud.cn-north-4.huaweicloud.com/doer/redirect
	// IdeaType/auth_callback_url/plugin-name/plugin-version/ticket_id are
	// appended by buildAuthorizeURL, so only the base differs.
	authorizeDoer = "https://devcloud.cn-north-4.huaweicloud.com/doer/redirect"
)

// endpointsFromEnv builds an Endpoints override from the environment.
//
// Recognised:
//
//	CODEARTS_AUTHORIZE_MODE=portal   default; https://codearts.huaweicloud.com/portal/authorize
//	CODEARTS_AUTHORIZE_MODE=doer     https://devcloud.cn-north-4.huaweicloud.com/doer/redirect
//	CODEARTS_AUTHORIZE_URL=<url>     explicit base URL, wins over the mode
//
// An unrecognised mode is ignored rather than fatal: a typo must not stop the
// plugin from starting, and silently keeping the default is the safe outcome.
func endpointsFromEnv() Endpoints {
	var ep Endpoints

	mode := strings.ToLower(strings.TrimSpace(os.Getenv(envAuthorizeMode)))
	switch mode {
	case "doer", "ide", "jetbrains":
		ep.Authorize = authorizeDoer
	case "", "portal", "web":
		// Keep the built-in default; leave empty so New() applies it.
	default:
		// Unknown value: keep the default.
	}

	if raw := strings.TrimSpace(os.Getenv("CODEARTS_AUTHORIZE_URL")); raw != "" {
		if isHTTPURL(raw) {
			ep.Authorize = raw
		}
	}
	return ep
}

// isHTTPURL reports whether s looks like an absolute http(s) URL. A malformed
// override is dropped so a bad value cannot redirect the login flow to
// something that will never respond.
func isHTTPURL(s string) bool {
	lower := strings.ToLower(s)
	return strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://")
}

// describeAuthorizeMode reports the effective mode for the plugin log, so an
// operator can see which entry point is in use without reading the environment.
func describeAuthorizeMode(current string) string {
	switch strings.TrimSpace(current) {
	case authorizeDoer:
		return "doer (desktop IDE channel)"
	case authorizePortal, "":
		return "portal (web, default)"
	default:
		return "custom (" + current + ")"
	}
}
