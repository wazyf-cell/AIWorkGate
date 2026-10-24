package app

// Custom upstream connections: one (endpoint, key, protocol) pair owning many
// models. The key stays in the DPAPI store; SQLite only holds the endpoint.
// Models created before this layer existed are merged into connections once on
// startup by base_url + key.

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"aiworkgate/internal/store"
	"aiworkgate/internal/webui"
)

func normalizeConnectionBase(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

// connectionKey returns the DPAPI-stored key of one connection.
func (a *App) connectionKey(id string) (string, bool) {
	if a.secrets == nil || strings.TrimSpace(id) == "" {
		return "", false
	}
	return a.secrets.GetExtra(store.CustomConnectionSecretKey(id))
}

// listCustomConnections returns the stored rows with key_set filled in.
func (a *App) listCustomConnections() ([]store.CustomConnection, error) {
	if a.store == nil {
		return nil, errors.New("app: custom connection store is not available")
	}
	rows, err := a.store.ListCustomConnections()
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []store.CustomConnection{}
	}
	for i := range rows {
		_, rows[i].KeySet = a.connectionKey(rows[i].ID)
	}
	return rows, nil
}

// findCustomConnectionByCredential returns the connection whose endpoint and
// exact key match. The protocol is deliberately not part of the identity: the
// same endpoint+key cannot be two connections.
func (a *App) findCustomConnectionByCredential(baseURL, key string) (store.CustomConnection, bool, error) {
	rows, err := a.store.ListCustomConnections()
	if err != nil {
		return store.CustomConnection{}, false, err
	}
	for _, row := range rows {
		stored, _ := a.connectionKey(row.ID)
		if row.BaseURL == baseURL && stored == key {
			return row, true, nil
		}
	}
	return store.CustomConnection{}, false, nil
}

func (a *App) createCustomConnection(input webui.CustomConnectionInput) (store.CustomConnection, bool, error) {
	if a.store == nil || a.secrets == nil {
		return store.CustomConnection{}, false, errors.New("app: custom connection services are not available")
	}
	protocol, ok := store.NormalizeCustomProtocol(input.Protocol)
	if !ok {
		return store.CustomConnection{}, false, fmt.Errorf("%w: 不支持的协议 %q", store.ErrInvalidArgument, input.Protocol)
	}
	base := normalizeConnectionBase(input.BaseURL)
	key := strings.TrimSpace(input.APIKey)
	existing, found, err := a.findCustomConnectionByCredential(base, key)
	if err != nil {
		return store.CustomConnection{}, false, err
	}
	if found {
		if existing.Protocol != protocol {
			return store.CustomConnection{}, false, fmt.Errorf("%w: 同地址同 Key 的接入已存在（协议 %s），不能以 %s 重复添加",
				webui.ErrCustomConnectionExists, existing.Protocol, protocol)
		}
		existing.KeySet = true
		return existing, false, nil
	}

	row, err := a.store.UpsertCustomConnection(store.CustomConnection{
		Label:    connectionLabel(input.Label, base),
		Protocol: protocol,
		BaseURL:  base,
		Enabled:  input.Enabled,
	})
	if err != nil {
		return store.CustomConnection{}, false, err
	}
	if err := a.secrets.SetExtra(store.CustomConnectionSecretKey(row.ID), key); err != nil {
		_, _, _ = a.store.DeleteCustomConnection(row.ID)
		return store.CustomConnection{}, false, err
	}
	row.KeySet = true
	a.logger.Info("app", "custom connection created", map[string]any{
		"id": row.ID, "protocol": row.Protocol, "base_url": row.BaseURL,
	})
	return row, true, nil
}

func (a *App) updateCustomConnection(id string, patch webui.CustomConnectionPatch) (store.CustomConnection, error) {
	if a.store == nil || a.secrets == nil {
		return store.CustomConnection{}, errors.New("app: custom connection services are not available")
	}
	row, found, err := a.store.GetCustomConnection(id)
	if err != nil {
		return store.CustomConnection{}, err
	}
	if !found {
		return store.CustomConnection{}, store.ErrNotFound
	}
	if patch.Label != nil {
		row.Label = strings.TrimSpace(*patch.Label)
	}
	if patch.Protocol != nil {
		protocol, ok := store.NormalizeCustomProtocol(*patch.Protocol)
		if !ok {
			return store.CustomConnection{}, fmt.Errorf("%w: 不支持的协议 %q", store.ErrInvalidArgument, *patch.Protocol)
		}
		row.Protocol = protocol
	}
	if patch.BaseURL != nil {
		row.BaseURL = normalizeConnectionBase(*patch.BaseURL)
	}
	if patch.Enabled != nil {
		row.Enabled = *patch.Enabled
	}
	if row.Label == "" {
		row.Label = connectionLabel("", row.BaseURL)
	}

	key, _ := a.connectionKey(row.ID)
	newKey := key
	if patch.APIKey != nil {
		newKey = strings.TrimSpace(*patch.APIKey)
	}
	other, found, err := a.findCustomConnectionByCredential(row.BaseURL, newKey)
	if err != nil {
		return store.CustomConnection{}, err
	}
	if found && other.ID != row.ID {
		return store.CustomConnection{}, fmt.Errorf("%w: 同地址同 Key 的接入已存在（%s）",
			webui.ErrCustomConnectionExists, other.Label)
	}

	row, err = a.store.UpsertCustomConnection(row)
	if err != nil {
		return store.CustomConnection{}, err
	}
	if patch.APIKey != nil {
		if err := a.secrets.SetExtra(store.CustomConnectionSecretKey(row.ID), newKey); err != nil {
			return store.CustomConnection{}, err
		}
	}
	_, row.KeySet = a.connectionKey(row.ID)
	a.logger.Info("app", "custom connection updated", map[string]any{
		"id": row.ID, "protocol": row.Protocol, "enabled": row.Enabled,
	})
	return row, nil
}

func (a *App) deleteCustomConnection(id string) (bool, error) {
	if a.store == nil {
		return false, errors.New("app: custom connection store is not available")
	}
	modelIDs, found, err := a.store.DeleteCustomConnection(id)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	if a.secrets != nil {
		_ = a.secrets.SetExtra(store.CustomConnectionSecretKey(strings.TrimSpace(id)), "")
		for _, modelID := range modelIDs {
			_ = a.secrets.SetExtra(store.CustomModelSecretKey(modelID), "")
		}
	}
	a.logger.Info("app", "custom connection deleted", map[string]any{
		"id": strings.TrimSpace(id), "models": len(modelIDs),
	})
	return true, nil
}

func connectionLabel(label, baseURL string) string {
	label = strings.TrimSpace(label)
	if label != "" {
		return label
	}
	if parsed, err := url.Parse(baseURL); err == nil && parsed.Hostname() != "" {
		return parsed.Hostname()
	}
	return "自定义接入"
}

// migrateLegacyCustomModels groups rows written before connections existed by
// (base_url, key) and attaches them to one connection per group. It is
// idempotent: once a row has a connection_id it is no longer selected. A
// failure leaves the legacy rows untouched for the next start.
func (a *App) migrateLegacyCustomModels() {
	if a.store == nil || a.secrets == nil {
		return
	}
	rows, err := a.store.ListLegacyCustomModels()
	if err != nil || len(rows) == 0 {
		return
	}
	type groupKey struct{ base, key string }
	groups := map[groupKey][]store.CustomModel{}
	for _, row := range rows {
		key, _ := a.secrets.GetExtra(store.CustomModelSecretKey(row.ID))
		base := normalizeConnectionBase(row.BaseURL)
		groups[groupKey{base: base, key: key}] = append(groups[groupKey{base: base, key: key}], row)
	}
	merged := 0
	for key, models := range groups {
		conn, found, err := a.findCustomConnectionByCredential(key.base, key.key)
		if err != nil {
			a.logger.Warn("app", "legacy custom model migration skipped", map[string]any{"error": err.Error()})
			break
		}
		if !found {
			conn, err = a.store.UpsertCustomConnection(store.CustomConnection{
				Label: connectionLabel("", key.base), Protocol: store.ProtocolOpenAI,
				BaseURL: key.base, Enabled: true,
			})
			if err != nil {
				a.logger.Warn("app", "legacy custom connection creation failed", map[string]any{"error": err.Error()})
				break
			}
			if err := a.secrets.SetExtra(store.CustomConnectionSecretKey(conn.ID), key.key); err != nil {
				a.logger.Warn("app", "legacy custom connection key migration failed", map[string]any{"error": err.Error()})
				break
			}
		}
		ok := true
		for _, model := range models {
			if err := a.store.AttachCustomModelConnection(model.ID, conn.ID); err != nil {
				a.logger.Warn("app", "legacy custom model attach failed", map[string]any{"error": err.Error()})
				ok = false
				break
			}
			merged++
		}
		if ok {
			// The connection now owns the credential; drop the per-row copy so
			// rotating one key cannot leave a stale duplicate behind.
			for _, model := range models {
				_ = a.secrets.SetExtra(store.CustomModelSecretKey(model.ID), "")
			}
		}
	}
	if merged > 0 {
		a.logger.Info("app", "legacy custom models migrated", map[string]any{
			"models": merged, "connections": len(groups),
		})
	}
}
