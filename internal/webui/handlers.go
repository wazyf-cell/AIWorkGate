package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"aiworkgate/internal/model"
	"aiworkgate/internal/plugins"
	"aiworkgate/internal/rules"
)

var pluginActions = map[string]bool{
	"enable":    true,
	"disable":   true,
	"start":     true,
	"stop":      true,
	"uninstall": true,
	"restart":   true,
	"reload":    true,
}

// Identity reset modes accepted by POST /api/accounts/identity/reset.
const (
	identityModeRequest = "request"
	identityModeOS      = model.IdentityOS
)

// ---------------------------------------------------------------- status

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if s.opts.Status == nil {
		s.unavailable(w)
		return
	}
	writeJSON(w, http.StatusOK, s.opts.Status())
}

// ---------------------------------------------------------------- settings

func (s *server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet, http.MethodPut) {
		return
	}

	if r.Method == http.MethodGet {
		if s.opts.GetSettings == nil {
			s.unavailable(w)
			return
		}
		raw, err := s.opts.GetSettings()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "read settings: "+err.Error(), "internal_error")
			return
		}
		trimmed := []byte(strings.TrimSpace(string(raw)))
		if len(trimmed) == 0 {
			trimmed = []byte("{}")
		}
		if !json.Valid(trimmed) {
			writeError(w, http.StatusInternalServerError, "settings are not valid JSON", "internal_error")
			return
		}
		writeRawJSON(w, http.StatusOK, trimmed)
		return
	}

	if s.opts.SaveSettings == nil {
		s.unavailable(w)
		return
	}
	var raw json.RawMessage
	if !decodeJSON(w, r, &raw) {
		return
	}
	payload, ok := ensureJSONObject(raw)
	if !ok {
		writeError(w, http.StatusBadRequest, "settings body must be a JSON object", "bad_request")
		return
	}
	if err := s.opts.SaveSettings(payload); err != nil {
		writeError(w, http.StatusInternalServerError, "save settings: "+err.Error(), "internal_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------------------------------------------------------------- secrets

func (s *server) handleAKey(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet, http.MethodPut) {
		return
	}

	if r.Method == http.MethodGet {
		if s.opts.AKeyConfigured == nil {
			s.unavailable(w)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"configured": s.opts.AKeyConfigured()})
		return
	}

	if s.opts.SetAKey == nil {
		s.unavailable(w)
		return
	}
	var body struct {
		Key string `json:"key"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	key := strings.TrimSpace(body.Key)
	if key == "" {
		writeError(w, http.StatusBadRequest, "key is required", "bad_request")
		return
	}
	if err := s.opts.SetAKey(key); err != nil {
		writeError(w, http.StatusInternalServerError, "save A key: "+err.Error(), "internal_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) handleLANPassword(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPut) {
		return
	}
	if s.opts.SetLANPassword == nil {
		s.unavailable(w)
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := s.opts.SetLANPassword(body.Password); err != nil {
		writeError(w, http.StatusInternalServerError, "save LAN password: "+err.Error(), "internal_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------------------------------------------------------------- plugins

func (s *server) handlePlugins(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if s.opts.Plugins == nil {
		s.unavailable(w)
		return
	}
	plugins := s.opts.Plugins()
	if plugins == nil {
		plugins = []model.PluginStatus{}
	}
	writeJSON(w, http.StatusOK, plugins)
}

// installFormField is the multipart field carrying the plugin archive.
const installFormField = "file"

// errUploadTooLarge reports an upload that grew past the archive cap while it
// was being copied (a missing or lying Content-Length cannot bypass the limit).
var errUploadTooLarge = errors.New("plugin archive is too large")

// isTooLarge reports whether err is the body-limit rejection produced by
// http.MaxBytesReader, so an oversized upload answers 413 instead of the
// generic "file is required" message.
func isTooLarge(err error) bool {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return true
	}
	// multipart wraps the reader error in its own text; checking the sentinel
	// above covers the normal path, this covers a wrapped copy.
	return strings.Contains(err.Error(), "request body too large")
}

// handlePluginInstall installs a plugin from an uploaded zip.
//
// Only a multipart upload is accepted. The old JSON form took a server-side
// path (zip_path / path) and handed it straight to the installer, so any caller
// could point at an arbitrary readable file - and a UNC path would make Windows
// send the machine account's NTLM credentials to a remote SMB host. Either way
// "install a plugin" meant "execute anything, as the gateway user".
//
// The archive is spooled to a private temporary file and deleted once the
// install returns. A fresh install stays stopped unless the request explicitly
// asks for enabled=true.
func (s *server) handlePluginInstall(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	if s.opts.InstallPlugin == nil {
		s.unavailable(w)
		return
	}
	// Cap the whole request before any of it is buffered. plugins.MaxZipBytes is
	// the installer's existing archive limit, so a larger body could not be
	// installed anyway.
	r.Body = http.MaxBytesReader(w, r.Body, plugins.MaxZipBytes)
	if err := r.ParseMultipartForm(maxBodyBytes); err != nil {
		if isTooLarge(err) {
			writeError(w, http.StatusRequestEntityTooLarge, "plugin archive is too large", "payload_too_large")
			return
		}
		writeError(w, http.StatusBadRequest,
			`plugin file is required (multipart field "`+installFormField+`")`, "invalid_request_error")
		return
	}
	enabled, ok := parseInstallEnabled(r)
	if !ok {
		writeError(w, http.StatusBadRequest,
			`form field "enabled" must be true or false`, "invalid_request_error")
		return
	}
	file, header, err := r.FormFile(installFormField)
	if err != nil {
		writeError(w, http.StatusBadRequest,
			`plugin file is required (multipart field "`+installFormField+`")`, "invalid_request_error")
		return
	}
	defer file.Close()
	if header.Size > plugins.MaxZipBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "plugin archive is too large", "payload_too_large")
		return
	}

	archive, cleanup, err := s.spoolUpload(file)
	if err != nil {
		if errors.Is(err, errUploadTooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "plugin archive is too large", "payload_too_large")
			return
		}
		writeError(w, http.StatusInternalServerError, "stage plugin upload failed", "internal_error")
		return
	}
	defer cleanup()

	if err := s.opts.InstallPlugin(archive, enabled); err != nil {
		writeError(w, http.StatusInternalServerError, "install plugin: "+err.Error(), "internal_error")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "running": enabled})
}

// parseInstallEnabled reads the optional "enabled" form value. Absent means
// false: a freshly installed plugin stays stopped until the user starts it.
// The second result is false only for a value that is neither a known true nor
// a known false spelling, which the handler answers 400 for.
func parseInstallEnabled(r *http.Request) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(r.FormValue("enabled"))) {
	case "", "false", "0", "off":
		return false, true
	case "true", "1", "on":
		return true, true
	default:
		return false, false
	}
}

// spoolUpload copies the uploaded archive to a temporary file the installer can
// open by path, enforcing plugins.MaxZipBytes as it copies. The file lives in a
// private per-request directory under the OS temp root (webui deliberately has
// no knowledge of the host data directory) and is removed by the returned
// cleanup, which is safe to call more than once.
func (s *server) spoolUpload(src io.Reader) (string, func(), error) {
	dir, err := os.MkdirTemp("", "aiworkgate-plugin-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	out, err := os.CreateTemp(dir, "plugin-*.zip")
	if err != nil {
		cleanup()
		return "", nil, err
	}
	path := out.Name()
	// One byte over the cap must fail the request rather than store a truncated
	// archive that the installer would later reject for the wrong reason.
	written, copyErr := io.Copy(out, io.LimitReader(src, plugins.MaxZipBytes+1))
	closeErr := out.Close()
	if copyErr != nil {
		cleanup()
		return "", nil, copyErr
	}
	if closeErr != nil {
		cleanup()
		return "", nil, closeErr
	}
	if written > plugins.MaxZipBytes {
		cleanup()
		return "", nil, errUploadTooLarge
	}
	return path, cleanup, nil
}

func (s *server) handlePluginAction(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	if s.opts.PluginAction == nil {
		s.unavailable(w)
		return
	}
	id := r.PathValue("id")
	action := strings.ToLower(r.PathValue("action"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "plugin id is required", "bad_request")
		return
	}
	if !pluginActions[action] {
		writeError(w, http.StatusBadRequest, "unknown plugin action: "+action, "bad_request")
		return
	}
	if err := s.opts.PluginAction(id, action); err != nil {
		writeError(w, http.StatusInternalServerError, "plugin action "+action+": "+err.Error(), "internal_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "action": action})
}

// handlePluginMeta serves GET /api/plugins/{id}/meta: the runtime view the
// plugin reports about itself. The status comes from the host's plugin list so
// an unknown or stopped plugin never reaches the callback, which would
// otherwise cold-start an on-demand plugin just to read its uptime.
func (s *server) handlePluginMeta(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if s.opts.PluginMeta == nil || s.opts.Plugins == nil {
		s.unavailable(w)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "plugin id is required", "bad_request")
		return
	}
	st, ok := s.pluginStatus(id)
	if !ok {
		writeError(w, http.StatusNotFound, "plugin not found: "+id, "plugin_not_found")
		return
	}
	if !st.Running {
		writeError(w, http.StatusServiceUnavailable, "plugin is not running: "+id, "plugin_not_running")
		return
	}
	meta, err := s.opts.PluginMeta(id)
	if err != nil {
		writePluginMetaError(w, id, err)
		return
	}
	if meta == nil {
		meta = map[string]any{}
	}
	writeJSON(w, http.StatusOK, meta)
}

// writePluginMetaError maps a failed /meta call onto the admin contract: a
// plugin that never answered is 504, any other failure (non-2xx, unreadable
// body) is 502. The plugin's message is kept because it is what tells the user
// why the plugin refused.
func writePluginMetaError(w http.ResponseWriter, id string, err error) {
	switch {
	case errors.Is(err, ErrPluginNotFound):
		writeError(w, http.StatusNotFound, "plugin not found: "+id, "plugin_not_found")
	case errors.Is(err, ErrPluginNotRunning):
		writeError(w, http.StatusServiceUnavailable, "plugin is not running: "+id, "plugin_not_running")
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "plugin meta timed out: "+err.Error(), "plugin_meta_failed")
	default:
		writeError(w, http.StatusBadGateway, "plugin meta failed: "+err.Error(), "plugin_meta_failed")
	}
}

// ---------------------------------------------------------------- accounts

func (s *server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if s.opts.Accounts == nil {
		s.unavailable(w)
		return
	}
	writeJSON(w, http.StatusOK, s.opts.Accounts(strings.TrimSpace(r.URL.Query().Get("plugin"))))
}

// pluginStatus resolves one plugin through Options.Plugins. A host that does
// not expose the plugin list cannot validate an account-level request.
func (s *server) pluginStatus(id string) (model.PluginStatus, bool) {
	if s.opts.Plugins == nil {
		return model.PluginStatus{}, false
	}
	for _, st := range s.opts.Plugins() {
		if st.Manifest.ID == id {
			return st, true
		}
	}
	return model.PluginStatus{}, false
}

// findProxy resolves one configured proxy through Options.Proxies.
func (s *server) findProxy(id string) (model.Proxy, bool) {
	if s.opts.Proxies == nil {
		return model.Proxy{}, false
	}
	for _, proxy := range s.opts.Proxies() {
		if proxy.ID == id {
			return proxy, true
		}
	}
	return model.Proxy{}, false
}

// handleAccountEgress stores one account's egress assignment (M7). The request
// is host-level and validated here because it must fail fast: the host never
// writes an egress entry the plugin could not honour.
func (s *server) handleAccountEgress(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPut) {
		return
	}
	if s.opts.SetAccountEgress == nil || s.opts.Plugins == nil {
		s.unavailable(w)
		return
	}
	var body struct {
		PluginID  string `json:"plugin_id"`
		AccountID string `json:"account_id"`
		Mode      string `json:"mode"`
		ProxyID   string `json:"proxy_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	pluginID := strings.TrimSpace(body.PluginID)
	accountID := strings.TrimSpace(body.AccountID)
	mode := strings.ToLower(strings.TrimSpace(body.Mode))
	proxyID := strings.TrimSpace(body.ProxyID)

	if pluginID == "" || accountID == "" {
		writeError(w, http.StatusBadRequest, "plugin_id and account_id are required", "invalid_egress")
		return
	}
	if _, ok := s.pluginStatus(pluginID); !ok {
		writeError(w, http.StatusNotFound, "plugin not found: "+pluginID, "not_found")
		return
	}

	switch mode {
	case "", "default":
		// An empty mode means "back to the plugin default": drop the row.
		mode = "default"
		proxyID = ""
	case model.EgressDirect:
		proxyID = ""
	case model.EgressProxy:
		if proxyID == "" {
			writeError(w, http.StatusBadRequest, "proxy_id is required for mode proxy", "invalid_egress")
			return
		}
		proxy, ok := s.findProxy(proxyID)
		if !ok {
			writeError(w, http.StatusBadRequest, "unknown proxy: "+proxyID, "invalid_egress")
			return
		}
		if !proxy.Enabled {
			writeError(w, http.StatusBadRequest, "proxy is disabled: "+proxyID, "invalid_egress")
			return
		}
	default:
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("mode must be direct, proxy or default, got %q", mode), "invalid_egress")
		return
	}

	reloaded, warning, err := s.opts.SetAccountEgress(pluginID, accountID, mode, proxyID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "save account egress: "+err.Error(), "internal_error")
		return
	}
	payload := map[string]any{"ok": true, "reloaded": reloaded}
	if warning != "" {
		payload["warning"] = warning
	}
	writeJSON(w, http.StatusOK, payload)
}

// handleIdentityReset proxies a device-identity reset to the plugin (M8). The
// host never changes the OS identity itself; mode "os" additionally needs an
// explicit confirmation and a plugin that may switch machine identifiers.
func (s *server) handleIdentityReset(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	if s.opts.ResetIdentity == nil || s.opts.Plugins == nil {
		s.unavailable(w)
		return
	}
	var body struct {
		PluginID  string `json:"plugin_id"`
		AccountID string `json:"account_id"`
		Mode      string `json:"mode"`
		Confirm   bool   `json:"confirm"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	pluginID := strings.TrimSpace(body.PluginID)
	accountID := strings.TrimSpace(body.AccountID)
	mode := strings.ToLower(strings.TrimSpace(body.Mode))

	if pluginID == "" {
		writeError(w, http.StatusBadRequest, "plugin_id is required", "bad_request")
		return
	}
	st, ok := s.pluginStatus(pluginID)
	if !ok {
		writeError(w, http.StatusNotFound, "plugin not found: "+pluginID, "not_found")
		return
	}
	if !st.Running {
		writeError(w, http.StatusConflict, "plugin is not running: "+pluginID, "plugin_not_running")
		return
	}
	if accountID == "" {
		writeError(w, http.StatusBadRequest, "account_id is required", "bad_request")
		return
	}
	switch mode {
	case identityModeRequest:
	case identityModeOS:
		if !body.Confirm {
			writeError(w, http.StatusBadRequest,
				"os identity reset requires confirm=true", "confirmation_required")
			return
		}
		if !hasCapability(st.Manifest, model.CapIdentityOS) || !st.Manifest.Permissions.OSIdentity {
			writeError(w, http.StatusBadRequest,
				"plugin does not support os identity switching: "+pluginID, "os_identity_unsupported")
			return
		}
	default:
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("mode must be request or os, got %q", mode), "bad_request")
		return
	}

	if err := s.opts.ResetIdentity(pluginID, accountID, mode); err != nil {
		// Keep the plugin's own 4xx/501 (e.g. identity_not_applicable) instead
		// of turning an honest "unsupported" into a gateway failure.
		writePluginCallError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func hasCapability(mf model.Manifest, capability string) bool {
	for _, c := range mf.Capabilities {
		if strings.TrimSpace(c) == capability {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- account login

// loginMethods is the method set fixed by the plugin contract (§6). An empty
// method lets the plugin choose its own default flow.
var loginMethods = map[string]bool{
	"api_key":     true,
	"oauth":       true,
	"ide_plugin":  true,
	"paste_code":  true,
	"local_state": true,
	"cookie":      true,
}

// pluginForCall validates a plugin id and rejects a stopped plugin before any
// request is relayed, so the caller gets a precise status instead of a
// connection error.
func (s *server) pluginForCall(w http.ResponseWriter, pluginID string) bool {
	st, ok := s.pluginStatus(pluginID)
	if !ok {
		writeError(w, http.StatusNotFound, "plugin not found: "+pluginID, "not_found")
		return false
	}
	if !st.Running {
		writeError(w, http.StatusConflict, "plugin is not running: "+pluginID, "plugin_not_running")
		return false
	}
	return true
}

// writePluginCallError maps adapter errors onto the admin API contract: an
// unknown plugin is 404, a disabled or stopped one 409, a request the plugin
// itself rejected as invalid stays a 4xx (it is a user error, not a gateway
// failure), and anything else is 502 with the plugin's message.
func writePluginCallError(w http.ResponseWriter, err error) {
	type httpStatusError interface {
		error
		HTTPStatus() int
	}
	switch {
	case errors.Is(err, ErrPluginNotFound):
		writeError(w, http.StatusNotFound, err.Error(), "not_found")
	case errors.Is(err, ErrPluginNotRunning):
		writeError(w, http.StatusConflict, err.Error(), "plugin_not_running")
	default:
		var se httpStatusError
		if errors.As(err, &se) {
			if code := se.HTTPStatus(); (code >= 400 && code < 500) || code == http.StatusNotImplemented {
				writeError(w, code, err.Error(), "plugin_error")
				return
			}
		}
		writeError(w, http.StatusBadGateway, err.Error(), "plugin_error")
	}
}

// handleAccountLoginStart opens a login session. The host renders the returned
// instructions, link, code and fields but never stores credential values.
func (s *server) handleAccountLoginStart(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	if s.opts.AccountLoginStart == nil || s.opts.Plugins == nil {
		s.unavailable(w)
		return
	}
	var body struct {
		PluginID string `json:"plugin_id"`
		Method   string `json:"method"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	pluginID := strings.TrimSpace(body.PluginID)
	method := strings.ToLower(strings.TrimSpace(body.Method))
	if pluginID == "" {
		writeError(w, http.StatusBadRequest, "plugin_id is required", "bad_request")
		return
	}
	if method != "" && !loginMethods[method] {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("method must be api_key, oauth, ide_plugin, paste_code, local_state, cookie or empty, got %q", method), "bad_request")
		return
	}
	if !s.pluginForCall(w, pluginID) {
		return
	}
	start, err := s.opts.AccountLoginStart(pluginID, method)
	if err != nil {
		writePluginCallError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, start)
}

// handleAccountLoginSession polls (GET) or cancels (DELETE) one login session.
func (s *server) handleAccountLoginSession(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet, http.MethodDelete) {
		return
	}
	sessionID := strings.TrimSpace(r.PathValue("sid"))
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "login session id is required", "bad_request")
		return
	}
	pluginID := strings.TrimSpace(r.URL.Query().Get("plugin"))
	if pluginID == "" {
		writeError(w, http.StatusBadRequest, "plugin query parameter is required", "bad_request")
		return
	}

	if r.Method == http.MethodDelete {
		if s.opts.AccountLoginCancel == nil || s.opts.Plugins == nil {
			s.unavailable(w)
			return
		}
		if !s.pluginForCall(w, pluginID) {
			return
		}
		if err := s.opts.AccountLoginCancel(pluginID, sessionID); err != nil {
			writePluginCallError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}

	if s.opts.AccountLoginPoll == nil || s.opts.Plugins == nil {
		s.unavailable(w)
		return
	}
	if !s.pluginForCall(w, pluginID) {
		return
	}
	status, err := s.opts.AccountLoginPoll(pluginID, sessionID)
	if err != nil {
		writePluginCallError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// handleAccountLoginSubmit forwards form values (API key, cookie, ...) to the
// plugin. The values are never logged or persisted by the host.
func (s *server) handleAccountLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	if s.opts.AccountLoginSubmit == nil || s.opts.Plugins == nil {
		s.unavailable(w)
		return
	}
	sessionID := strings.TrimSpace(r.PathValue("sid"))
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "login session id is required", "bad_request")
		return
	}
	var body struct {
		PluginID string            `json:"plugin_id"`
		Values   map[string]string `json:"values"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	pluginID := strings.TrimSpace(body.PluginID)
	if pluginID == "" {
		writeError(w, http.StatusBadRequest, "plugin_id is required", "bad_request")
		return
	}
	if len(body.Values) == 0 {
		writeError(w, http.StatusBadRequest, "values is required", "bad_request")
		return
	}
	if !s.pluginForCall(w, pluginID) {
		return
	}
	status, err := s.opts.AccountLoginSubmit(pluginID, sessionID, body.Values)
	if err != nil {
		writePluginCallError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// handleAccountPatch applies a partial update such as {"enabled":false}.
func (s *server) handleAccountPatch(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	if s.opts.AccountPatch == nil || s.opts.Plugins == nil {
		s.unavailable(w)
		return
	}
	var body struct {
		PluginID  string         `json:"plugin_id"`
		AccountID string         `json:"account_id"`
		Patch     map[string]any `json:"patch"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	pluginID := strings.TrimSpace(body.PluginID)
	accountID := strings.TrimSpace(body.AccountID)
	if pluginID == "" || accountID == "" {
		writeError(w, http.StatusBadRequest, "plugin_id and account_id are required", "bad_request")
		return
	}
	if len(body.Patch) == 0 {
		writeError(w, http.StatusBadRequest, "patch must not be empty", "bad_request")
		return
	}
	if !s.pluginForCall(w, pluginID) {
		return
	}
	if err := s.opts.AccountPatch(pluginID, accountID, body.Patch); err != nil {
		writePluginCallError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleAccountDelete removes one account and its credentials from a plugin.
func (s *server) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	if s.opts.AccountDelete == nil || s.opts.Plugins == nil {
		s.unavailable(w)
		return
	}
	var body struct {
		PluginID  string `json:"plugin_id"`
		AccountID string `json:"account_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	pluginID := strings.TrimSpace(body.PluginID)
	accountID := strings.TrimSpace(body.AccountID)
	if pluginID == "" || accountID == "" {
		writeError(w, http.StatusBadRequest, "plugin_id and account_id are required", "bad_request")
		return
	}
	if !s.pluginForCall(w, pluginID) {
		return
	}
	if err := s.opts.AccountDelete(pluginID, accountID); err != nil {
		writePluginCallError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleAccountRefresh asks the plugin to renew one account's credentials.
func (s *server) handleAccountRefresh(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	if s.opts.AccountRefresh == nil || s.opts.Plugins == nil {
		s.unavailable(w)
		return
	}
	var body struct {
		PluginID  string `json:"plugin_id"`
		AccountID string `json:"account_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	pluginID := strings.TrimSpace(body.PluginID)
	accountID := strings.TrimSpace(body.AccountID)
	if pluginID == "" || accountID == "" {
		writeError(w, http.StatusBadRequest, "plugin_id and account_id are required", "bad_request")
		return
	}
	if !s.pluginForCall(w, pluginID) {
		return
	}
	result, err := s.opts.AccountRefresh(pluginID, accountID)
	if err != nil {
		writePluginCallError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": result})
}

// ---------------------------------------------------------------- rules

// handleRules lists the stored rule packs and the names of the builtin ones.
func (s *server) handleRules(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if s.opts.RulePacks == nil {
		s.unavailable(w)
		return
	}
	packs, builtin, err := s.opts.RulePacks()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list rule packs: "+err.Error(), "internal_error")
		return
	}
	if packs == nil {
		packs = []model.RulePack{}
	}
	if builtin == nil {
		builtin = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"packs": packs, "builtin": builtin})
}

// handleRulePack saves (PUT) or deletes (DELETE) one pack, identified by the
// path so a body can never rename a different pack.
func (s *server) handleRulePack(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPut, http.MethodDelete) {
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeError(w, http.StatusBadRequest, "rule pack name is required", "bad_request")
		return
	}

	if r.Method == http.MethodDelete {
		if s.opts.DeleteRulePack == nil {
			s.unavailable(w)
			return
		}
		if err := s.opts.DeleteRulePack(name); err != nil {
			writeRulePackError(w, "delete rule pack", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}

	if s.opts.SaveRulePack == nil {
		s.unavailable(w)
		return
	}
	var pack model.RulePack
	if !decodeJSON(w, r, &pack) {
		return
	}
	bodyName := strings.TrimSpace(pack.Name)
	if bodyName == "" {
		pack.Name = name
	} else if bodyName != name {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("body name %q does not match path name %q", bodyName, name), "invalid_rules")
		return
	}
	if err := s.opts.SaveRulePack(pack); err != nil {
		writeRulePackError(w, "save rule pack", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleRulePackReset restores a builtin pack to its shipped content.
func (s *server) handleRulePackReset(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	if s.opts.ResetRulePack == nil {
		s.unavailable(w)
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeError(w, http.StatusBadRequest, "rule pack name is required", "bad_request")
		return
	}
	if err := s.opts.ResetRulePack(name); err != nil {
		writeRulePackError(w, "reset rule pack", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleRulesReload rewrites every rules file and reloads the running plugins.
// Reload failures are best effort and reported per plugin, still with 200.
func (s *server) handleRulesReload(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	if s.opts.ReloadRules == nil {
		s.unavailable(w)
		return
	}
	reloaded, failed, err := s.opts.ReloadRules()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reload rules: "+err.Error(), "internal_error")
		return
	}
	if reloaded == nil {
		reloaded = []string{}
	}
	payload := map[string]any{"ok": true, "reloaded": reloaded}
	if len(failed) > 0 {
		payload["failed"] = failed
	}
	writeJSON(w, http.StatusOK, payload)
}

// writeRulePackError maps the rules package errors onto the admin API types.
func writeRulePackError(w http.ResponseWriter, action string, err error) {
	switch {
	case errors.Is(err, rules.ErrInvalidPack):
		writeError(w, http.StatusBadRequest, action+": "+err.Error(), "invalid_rules")
	case errors.Is(err, rules.ErrPackNotFound):
		writeError(w, http.StatusNotFound, action+": "+err.Error(), "not_found")
	default:
		writeError(w, http.StatusInternalServerError, action+": "+err.Error(), "internal_error")
	}
}

// ---------------------------------------------------------------- check-in

func (s *server) handleCheckins(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if s.opts.Checkins == nil {
		s.unavailable(w)
		return
	}
	query := r.URL.Query()
	writeJSON(w, http.StatusOK, s.opts.Checkins(
		strings.TrimSpace(query.Get("plugin")),
		strings.TrimSpace(query.Get("from")),
		strings.TrimSpace(query.Get("to")),
	))
}

func (s *server) handleCheckinAll(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	if s.opts.CheckinAll == nil {
		s.unavailable(w)
		return
	}
	// The body is optional; the host schedules every enabled account.
	writeJSON(w, http.StatusOK, s.opts.CheckinAll())
}

// ---------------------------------------------------------------- proxies

func (s *server) handleProxies(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet, http.MethodPost) {
		return
	}

	if r.Method == http.MethodGet {
		if s.opts.Proxies == nil {
			s.unavailable(w)
			return
		}
		proxies := s.opts.Proxies()
		if proxies == nil {
			proxies = []model.Proxy{}
		}
		writeJSON(w, http.StatusOK, proxies)
		return
	}

	if s.opts.SaveProxy == nil {
		s.unavailable(w)
		return
	}
	var proxy model.Proxy
	if !decodeJSON(w, r, &proxy) {
		return
	}
	if err := normalizeProxy(&proxy); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "bad_request")
		return
	}
	if proxy.ID == model.SystemProxyID {
		writeError(w, http.StatusConflict, "系统代理行由宿主跟随 Windows 设置维护，不能手动创建或修改", "managed_proxy")
		return
	}
	if err := s.opts.SaveProxy(proxy); err != nil {
		writeError(w, http.StatusInternalServerError, "save proxy: "+err.Error(), "internal_error")
		return
	}
	// The id is generated inside the app layer and the password is kept in the
	// secret store, so echo the saved row rather than the request body: a caller
	// needs the id to bind the proxy, and must not get the password back.
	if proxy.ID == "" && s.opts.Proxies != nil {
		for _, saved := range s.opts.Proxies() {
			if saved.Name == proxy.Name && saved.Host == proxy.Host && saved.Port == proxy.Port {
				proxy = saved
				break
			}
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "proxy": proxy})
}

func (s *server) handleProxy(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPut, http.MethodDelete) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "proxy id is required", "bad_request")
		return
	}

	if r.Method == http.MethodDelete {
		if id == model.SystemProxyID {
			writeError(w, http.StatusConflict, "系统代理行由宿主跟随 Windows 设置维护，不能删除", "managed_proxy")
			return
		}
		if s.opts.DeleteProxy == nil {
			s.unavailable(w)
			return
		}
		if err := s.opts.DeleteProxy(id); err != nil {
			writeError(w, http.StatusInternalServerError, "delete proxy: "+err.Error(), "internal_error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
		return
	}

	if id == model.SystemProxyID {
		writeError(w, http.StatusConflict, "系统代理行由宿主跟随 Windows 设置维护，不能手动修改", "managed_proxy")
		return
	}
	if s.opts.SaveProxy == nil {
		s.unavailable(w)
		return
	}
	var proxy model.Proxy
	if !decodeJSON(w, r, &proxy) {
		return
	}
	proxy.ID = id
	if err := normalizeProxy(&proxy); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "bad_request")
		return
	}
	if err := s.opts.SaveProxy(proxy); err != nil {
		writeError(w, http.StatusInternalServerError, "save proxy: "+err.Error(), "internal_error")
		return
	}
	// Echo the stored row, never the submitted secret: the password lives in the
	// secret store, and a request body must not be reflected into client logs.
	if s.opts.Proxies != nil {
		for _, saved := range s.opts.Proxies() {
			if saved.ID == proxy.ID {
				proxy = saved
				break
			}
		}
	}
	proxy.Password = ""
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "proxy": proxy})
}

func (s *server) handleProxyCheck(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	if s.opts.CheckProxy == nil {
		s.unavailable(w)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "proxy id is required", "bad_request")
		return
	}
	writeJSON(w, http.StatusOK, s.opts.CheckProxy(id))
}

func normalizeProxy(proxy *model.Proxy) error {
	proxy.Name = strings.TrimSpace(proxy.Name)
	proxy.Host = strings.TrimSpace(proxy.Host)
	proxy.Type = strings.ToLower(strings.TrimSpace(proxy.Type))
	if proxy.Name == "" {
		return errors.New("proxy name is required")
	}
	if proxy.Host == "" {
		return errors.New("proxy host is required")
	}
	if proxy.Port < 1 || proxy.Port > 65535 {
		return errors.New("proxy port must be between 1 and 65535")
	}
	if proxy.Type == "" {
		proxy.Type = "http"
	}
	if proxy.Type != "http" && proxy.Type != "socks5" {
		return fmt.Errorf("proxy type must be http or socks5, got %q", proxy.Type)
	}
	return nil
}

// ---------------------------------------------------------------- logs

func (s *server) handleRequestLogs(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if s.opts.RequestLogs == nil {
		s.unavailable(w)
		return
	}
	query := make(map[string]string, len(r.URL.Query()))
	for key, values := range r.URL.Query() {
		if len(values) > 0 {
			query[key] = values[0]
		}
	}
	writeJSON(w, http.StatusOK, s.opts.RequestLogs(query))
}

func (s *server) handleRuntimeLogs(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if s.opts.RuntimeLogs == nil {
		s.unavailable(w)
		return
	}
	query := r.URL.Query()
	source := strings.TrimSpace(query.Get("source"))
	if source == "" {
		source = "host"
	}
	limit := parseLimit(strings.TrimSpace(query.Get("n")), 200, 5000)
	lines := s.opts.RuntimeLogs(source, limit)
	if lines == nil {
		lines = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"source": source,
		"lines":  lines,
		"count":  len(lines),
	})
}
