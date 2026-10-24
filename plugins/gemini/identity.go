package main

// Device identity for the Gemini channel.
//
// What the upstream can actually see:
//
//   - five pinned fingerprint headers (x-machine-id: cmdc-pak,
//     x-vscode-sessionid: proxy, ...). They are protocol constants: changing
//     them is what makes Cloud Code refuse a desktop client, so they are not
//     rotatable and this plugin does not pretend otherwise.
//   - the envelope's per-request identifiers: requestId (agent/<ms>/<hex>) and
//     request.sessionId, a hash that includes a session generation counter.
//
// The generation is the one identity knob the protocol itself supports (the
// reference bumps it when a session overflows). Resetting the request identity
// therefore rotates the stored session tag and device tag: the next request
// carries a different sessionId, so the upstream groups it as a new session.
// The account is real, the rotation is observable on the wire, and nothing here
// changes a header the upstream fingerprints.

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"time"
)

// identityState is the plugin-owned, persisted identity.
type identityState struct {
	// DeviceID is the plugin's device tag. It is exposed to the operator and
	// rotated by a request-mode reset.
	DeviceID string `json:"device_id"`
	// SessionID is the session tag; the wire sessionId is derived from it.
	SessionID string `json:"session_id"`
	// UpdatedAt is when the identity was last issued or rotated.
	UpdatedAt int64 `json:"updated_at"`
}

// machineTag is the literal x-machine-id value the protocol sends.
const machineTag = "cmdc-pak"

// identityUserAgent is the literal User-Agent the protocol sends.
const identityUserAgent = "antigravity/4.3.0 (cmdc-pak)"

// newIdentityState issues a fresh identity.
func newIdentityState(now time.Time) identityState {
	return identityState{
		DeviceID:  randomHex(8),
		SessionID: randomHex(8),
		UpdatedAt: now.Unix(),
	}
}

// ensureIdentity fills in a missing identity so an older credential file (one
// written before this field existed) still reports a real device.
func (c *credentials) ensureIdentity(now time.Time) bool {
	if c.Identity != nil && c.Identity.DeviceID != "" && c.Identity.SessionID != "" {
		return false
	}
	id := newIdentityState(now)
	if c.Identity != nil {
		// Keep whatever half was present.
		id.DeviceID = firstNonEmpty(c.Identity.DeviceID, id.DeviceID)
		id.SessionID = firstNonEmpty(c.Identity.SessionID, id.SessionID)
	}
	c.Identity = &id
	return true
}

// sessionGeneration turns the session tag into the positive integer the session
// id hash mixes in. Zero means "no generation", per deriveSessionID.
func sessionGeneration(id *identityState) int {
	if id == nil || id.SessionID == "" {
		return 0
	}
	sum := 0
	for i := 0; i < len(id.SessionID); i++ {
		sum = (sum*31 + int(id.SessionID[i])) % 1000000
	}
	return sum + 1
}

// identityView is the GET /accounts identity object the host masks.
func (c credentials) identityView() map[string]any {
	id := c.Identity
	if id == nil {
		return map[string]any{
			"machine_id": machineTag,
			"source":     "derived",
			"user_agent": identityUserAgent,
		}
	}
	return map[string]any{
		"machine_id": machineTag,
		"device_id":  id.DeviceID,
		"session_id": id.SessionID,
		"user_agent": identityUserAgent,
		"updated_at": id.UpdatedAt,
		"source":     "derived",
		"generation": strconv.Itoa(sessionGeneration(id)),
	}
}

// randomHex returns n random bytes as hex. A random failure is degraded to a
// time-seeded value rather than a constant, so two accounts never collide.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		ts := strconv.FormatInt(time.Now().UnixNano(), 16)
		return hex.EncodeToString([]byte(ts))[:n*2]
	}
	return hex.EncodeToString(b)
}
