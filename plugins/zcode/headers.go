package main

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// apiKeyHeaderSet is the identity set api.z.ai accepts for a coding-plan key.
// x-api-key is authoritative; the anthropic-version header is mandatory for the
// Messages protocol.
func apiKeyHeaderSet(key string, extra http.Header) [][2]string {
	h := [][2]string{}
	if extra != nil {
		if v := extra.Get("anthropic-beta"); v != "" {
			h = append(h, [2]string{"anthropic-beta", v})
		}
	}
	return append(h,
		[2]string{"content-type", "application/json"},
		[2]string{"x-api-key", key},
		[2]string{"anthropic-version", "2023-06-01"},
		[2]string{"User-Agent", "ZCode/" + appVersion},
		[2]string{"accept", "*/*"},
	)
}

// chatHeaderSet is the identity header set for the messages endpoint.
//
// Ported field by field from the reference (upstream/chatheaders.go). The
// omissions are as important as the inclusions: x-api-key, x-query-id and
// x-session-id must not be sent, because the official start-plan client omits
// them and sending x-query-id/x-session-id triggers upstream 3012.
func chatHeaderSet(jwt, appVer, deviceMid string, extra http.Header) [][2]string {
	h := [][2]string{}
	// anthropic-beta passes through when the caller supplies it.
	if extra != nil {
		if v := extra.Get("anthropic-beta"); v != "" {
			h = append(h, [2]string{"anthropic-beta", v})
		}
	}
	h = append(h,
		[2]string{"content-type", "application/json"},
		[2]string{"Authorization", "Bearer " + jwt},
		[2]string{"anthropic-version", "2023-06-01"},
		// identity set, in the reference's order
		[2]string{"HTTP-Referer", "https://zcode.z.ai/"},
		[2]string{"User-Agent", "ZCode/" + appVer},
		[2]string{"X-ZCode-App-Version", appVer},
		[2]string{"X-Title", "Z Code@cli"},
		[2]string{"X-ZCode-Agent", "glm"},
		[2]string{"X-Platform", "win32-x64"},
		[2]string{"X-Release-Channel", "production"},
		[2]string{"X-Client-Language", "en-001"},
		[2]string{"X-Client-Timezone", "Europe/Bucharest"},
		[2]string{"X-Os-Category", "windows"},
		[2]string{"X-Os-Version", "10.0.26200"},
		[2]string{"X-Device-Mid", deviceMid},
		// trace trio: regenerated per request, and ONLY these three
		[2]string{"x-request-id", newUUID()},
		[2]string{"x-zcode-session-type", "main"},
		[2]string{"x-zcode-trace-id", newUUID()},
		// httpx's own defaults
		[2]string{"accept", "*/*"},
		[2]string{"accept-encoding", "gzip, deflate"},
	)
	// X-Aliyun-Captcha-Verify-Param/Region are deliberately absent: the
	// reference sends a single-use param (and the region it was minted for) per
	// request. Those values are dynamic, and this plugin has no solver to mint
	// them, so there is no honest constant to put here.
	return h
}

// billingHeaderSet is the identity set for the billing family (balance/quota).
// It is not the chat set: the reference's billing client identifies as the
// electron desktop, while chat identifies as the cli.
func billingHeaderSet(jwt, deviceMid string) [][2]string {
	h := [][2]string{
		[2]string{"Authorization", "Bearer " + jwt},
		[2]string{"Accept", "*/*"},
		[2]string{"Accept-Language", "*"},
		[2]string{"User-Agent", "ZCode/" + billingVersion},
		[2]string{"HTTP-Referer", "https://zcode.z.ai"},
		[2]string{"X-Title", "Z Code@electron"},
		[2]string{"X-ZCode-App-Version", billingVersion},
		// os.release() semantics, not a marketing name
		[2]string{"X-Os-Category", "windows"},
		[2]string{"X-Os-Version", "10.0.26200"},
		[2]string{"X-Platform", "win32-x64"},
		[2]string{"X-Release-Channel", "stable"},
		[2]string{"X-Client-Language", "en-US"},
		// the same language/timezone pair the activation events report
		[2]string{"X-Client-Timezone", "America/New_York"},
		[2]string{"x-request-id", newUUID()},
		[2]string{"sec-fetch-mode", "cors"},
	}
	if deviceMid != "" {
		h = append(h, [2]string{"X-Device-Mid", deviceMid})
	}
	return h
}

// newUUID returns a random RFC-4122 v4 UUID, matching the reference's
// util.GenerateUUID so the trace headers look like a real client's.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	dst := make([]byte, 36)
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst)
}
