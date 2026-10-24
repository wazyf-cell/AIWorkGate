package main

// Device identity for the OpenCode channel.
//
// What the upstream can actually see:
//
//   - x-opencode-project, a 40-hex fingerprint derived from the credential and a
//     generation counter (projectID). It is stable for one install and changes
//     only when the generation is bumped, which is exactly what the identity
//     reset does. This is the one device-ish value the plugin owns, so it is the
//     device_id reported below.
//   - four same-value session headers carrying a `ses_...` id that is minted per
//     request (sessionID). There is no stored session identity to report and
//     nothing meaningful to rotate, so session_id is omitted rather than faked.
//   - the User-Agent actually sent (defaultUserAgent or the v2-shaped override).
//
// machine_id is omitted as well: this plugin never touches an OS-level
// identifier (manifest permissions.os_identity=false), so declaring one would be
// a promise with nothing behind it.
//
// Nothing here is a secret: the fingerprint is a SHA-1 of the credential, and it
// is already the header value every chat sends.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// identityView is the identity block of GET /accounts and the details endpoint.
// It is always available: the anonymous lane has a fingerprint too.
func identityView(c credentials) map[string]any {
	view := map[string]any{
		"device_id":  c.projectIDFor(),
		"user_agent": c.effectiveUserAgent(),
		"source":     "derived",
	}
	if at, ok := credentialUpdatedUnix(c.UpdatedAt); ok {
		view["updated_at"] = at
	}
	return view
}

// credentialUpdatedUnix converts the stored RFC-3339 stamp into the Unix seconds
// the contract uses. A missing or unparsable stamp is reported as absent instead
// of guessed.
func credentialUpdatedUnix(stamp string) (int64, bool) {
	stamp = strings.TrimSpace(stamp)
	if stamp == "" {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return 0, false
	}
	return t.Unix(), true
}

// handleIdentityDetails reports the derived identity without rotating it.
// handleAccountSub has already rejected any account id but "default".
func (s *server) handleIdentityDetails(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "identity": identityView(s.store.current())})
}

// handleIdentityReset rotates the project fingerprint by bumping the stored
// generation. mode "request" is the only mode: there is no OS identity to switch,
// so mode "os" is refused instead of pretending.
func (s *server) handleIdentityReset(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Mode string `json:"mode"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &in); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
			return
		}
	}
	mode := strings.ToLower(strings.TrimSpace(in.Mode))
	if mode == "" {
		mode = "request"
	}
	if mode != "request" {
		writeError(w, http.StatusBadRequest, "unsupported_identity_mode",
			"mode "+mode+" is not supported: this plugin has no OS identity; only the request fingerprint can be rotated (mode=request)")
		return
	}
	before := s.store.current().projectIDFor()
	generation, err := s.store.rotateFingerprint()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	creds := s.store.current()
	after := creds.projectIDFor()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"mode":       mode,
		"generation": generation,
		"rotated":    before != after,
		"project":    after,
		"identity":   identityView(creds),
	})
}
