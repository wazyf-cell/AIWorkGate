package main

// Identity headers and the request signature.
//
// Ported field by field from the reference:
//
//	auth.go   authSign / bearer / stripBearer / newTraceID / userapiHeaders
//	checkin.go checkinHeaders
//	upstream.go proxyChatHeaders
//
// The sets differ per endpoint family on purpose (web vs desktop client
// fingerprints). Collapsing them would make the upstream reject the request, so
// each family keeps its own builder.

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// authSign computes X-Auth-Sign = md5("<appid>&<ts>&<appsecret>"). It is the
// shipped client signature, not a user secret.
func authSign(ts string) string {
	sum := md5.Sum([]byte(appID + "&" + ts + "&" + appSecret))
	return fmt.Sprintf("%x", sum)
}

// bearer normalises a token to "Bearer <tok>".
func bearer(token string) string {
	t := strings.TrimSpace(token)
	if strings.HasPrefix(strings.ToLower(t), "bearer ") {
		return t
	}
	return "Bearer " + t
}

// stripBearer removes a "Bearer " prefix, if any.
func stripBearer(token string) string {
	t := strings.TrimSpace(token)
	if strings.HasPrefix(strings.ToLower(t), "bearer ") {
		return t[7:]
	}
	return t
}

// newTraceID mirrors the reference's uuid-ish trace id: it is not RFC-4122,
// and that is deliberate — the reference generates it the same way.
func newTraceID() string {
	n := time.Now().UnixNano()
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		n&0xffffffff, n>>32&0xffff, n>>48&0xffff, n>>20&0xfff&0xffff, n&0xffffffffffff)
}

// userapiHeaders is the web-client signed header set (auth.go userapiHeaders).
func userapiHeaders(token string) map[string]string {
	ts := fmt.Sprintf("%d", time.Now().Unix())
	h := map[string]string{
		"Content-Type":     "application/json",
		"Accept":           "*/*",
		"X-Version":        webVersion,
		"X-Tm":             "web",
		"X-Product":        "autoclaw",
		"X-Client-Type":    "web",
		"X-Channel":        "official",
		"X-Auth-Appid":     appID,
		"X-Auth-TimeStamp": ts,
		"X-Auth-Sign":      authSign(ts),
		"X-Trace-Id":       newTraceID(),
		"X-Lang":           "en",
	}
	if token != "" {
		h["Authorization"] = bearer(token)
	}
	return h
}

// checkinHeaders is the desktop-client signed set (checkin.go checkinHeaders).
// It signs identically but claims X-Tm: mac / X-Client-Type: pc / the desktop
// version, which is what the task endpoints expect.
func checkinHeaders(token string) map[string]string {
	ts := fmt.Sprintf("%d", time.Now().Unix())
	h := map[string]string{
		"Content-Type":     "application/json",
		"Accept":           "application/json",
		"X-Version":        clientVer,
		"X-Tm":             "mac",
		"X-Product":        "autoclaw",
		"X-Client-Type":    "pc",
		"X-Channel":        "official",
		"X-Auth-Appid":     appID,
		"X-Auth-TimeStamp": ts,
		"X-Auth-Sign":      authSign(ts),
		"X-Trace-Id":       newTraceID(),
		"X-Lang":           "en",
	}
	if token != "" {
		h["Authorization"] = bearer(token)
	}
	return h
}

// proxyChatHeaders is the header set of the narrow proxy chat channel
// (upstream.go proxyChatHeaders). Note it sends the token twice, as both
// Authorization and X-Authorization, and pins User-Agent to the desktop client.
func proxyChatHeaders(token, model string) map[string]string {
	return map[string]string{
		"Accept":          "application/json, text/plain, */*",
		"Authorization":   bearer(token),
		"X-Authorization": bearer(token),
		"X-Request-Model": model,
		"X-Tm":            "mac",
		"X-Version":       clientVer,
		"X-Product":       "autoclaw",
		"X-Channel":       "official",
		"X-Lang":          "zh-CN",
		"X-Client-Type":   "pc",
		"X-Trace-Id":      newTraceID(),
		"x_trace_id":      "autoclaw-bridge",
		"User-Agent":      "AutoClaw/" + clientVer,
	}
}

// relayHeaders are the relay channel's headers: the sandbox bridge accepts the
// bearer and, for a backend (non-agent) model, an x-openclaw-model selector.
func relayHeaders(token string, model string) map[string]string {
	h := map[string]string{
		"Accept":       "application/json, text/plain, */*",
		"Content-Type": "application/json",
	}
	if token != "" {
		h["Authorization"] = bearer(token)
	}
	if model != "" {
		h["x-openclaw-model"] = model
	}
	return h
}

// jwtPayloadField decodes a JWT payload (no verification — the plugin only
// reads the claims the upstream itself signed) and returns one field.
func jwtPayloadField(token, key string) any {
	parts := strings.Split(stripBearer(token), ".")
	if len(parts) < 2 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		pad := strings.Repeat("=", (4-len(parts[1])%4)%4)
		payload, err = base64.URLEncoding.DecodeString(parts[1] + pad)
		if err != nil {
			return nil
		}
	}
	var doc map[string]any
	if json.Unmarshal(payload, &doc) != nil {
		return nil
	}
	return doc[key]
}

// jwtClaimInt reads an integer claim, tolerating the numeric/string duality.
func jwtClaimInt(token, key string) int64 {
	switch v := jwtPayloadField(token, key).(type) {
	case float64:
		return int64(v)
	case string:
		var n int64
		fmt.Sscanf(v, "%d", &n)
		return n
	}
	return 0
}
