package store

// Model prefs: the human personalisation of one model (P12-2, D-27③④).
//
// Ported semantics from magpie's provider/modelprefs.go (D-27③):
//   - the user-set name is stored SEPARATELY from the vendor list: the vendor
//     list is what the catalog sync brings, the display name is what the user
//     writes, and the two are merged at render time as "自定义名 · 供应商";
//   - the reasoning levels a model keeps are stored too, so a model can be
//     exposed with a subset of its levels;
//   - a refresh of the catalog (ReplaceModelCatalog) never touches this table,
//     so neither a restart nor an upgrade loses a hand-set name or an effort
//     cut.
//
// The table ships with migration v12 (model_prefs). Every field is optional:
// NULL means "not personalised", which keeps the plugin/A reported value in
// force — the same rule model_overrides follows.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"aiworkgate/internal/model"
	"aiworkgate/internal/presets"
)

const (
	// maxPrefsNameRunes bounds a human display name.
	maxPrefsNameRunes = 128
	// maxPrefsVendorRunes bounds a human vendor label.
	maxPrefsVendorRunes = 64
)

// ModelPrefs is the personalisation of one model.
type ModelPrefs struct {
	// DisplayName is the human name. NULL means "keep the reported name".
	DisplayName *string `json:"display_name"`
	// VendorLabel is the human vendor label. NULL means "keep the reported vendor".
	VendorLabel *string `json:"vendor_label"`
	// Efforts is the kept reasoning levels. NULL means "keep all reported
	// levels"; an empty slice means "expose none".
	Efforts *[]string `json:"efforts"`
	// UpdatedAt is set by the store and ignored on write.
	UpdatedAt int64 `json:"updated_at,omitempty"`
}

// Fields lists the personalised field names (admin UI / audit display).
func (p ModelPrefs) Fields() []string {
	out := make([]string, 0, 3)
	if p.DisplayName != nil {
		out = append(out, "display_name")
	}
	if p.VendorLabel != nil {
		out = append(out, "vendor_label")
	}
	if p.Efforts != nil {
		out = append(out, "efforts")
	}
	return out
}

// Empty reports a pref that sets nothing.
func (p ModelPrefs) Empty() bool { return len(p.Fields()) == 0 }

// normalize trims text fields, drops blank values (blank = not personalised)
// and dedupes the effort list. It runs on write so a hand-written row can never
// break the merge.
func (p ModelPrefs) normalize() (ModelPrefs, error) {
	out := p
	out.UpdatedAt = 0

	if out.DisplayName != nil {
		value := strings.TrimSpace(*out.DisplayName)
		if value == "" {
			out.DisplayName = nil
		} else {
			if utf8.RuneCountInString(value) > maxPrefsNameRunes {
				return ModelPrefs{}, fmt.Errorf("store: model prefs: %w: display_name longer than %d characters",
					ErrInvalidArgument, maxPrefsNameRunes)
			}
			out.DisplayName = &value
		}
	}
	if out.VendorLabel != nil {
		value := strings.TrimSpace(*out.VendorLabel)
		if value == "" {
			out.VendorLabel = nil
		} else {
			if utf8.RuneCountInString(value) > maxPrefsVendorRunes {
				return ModelPrefs{}, fmt.Errorf("store: model prefs: %w: vendor_label longer than %d characters",
					ErrInvalidArgument, maxPrefsVendorRunes)
			}
			out.VendorLabel = &value
		}
	}
	if out.Efforts != nil {
		levels := make([]string, 0, len(*out.Efforts))
		seen := make(map[string]bool, len(*out.Efforts))
		for _, raw := range *out.Efforts {
			level := strings.TrimSpace(raw)
			if level == "" || seen[level] {
				continue
			}
			seen[level] = true
			levels = append(levels, level)
		}
		out.Efforts = &levels
	}
	return out, nil
}

// RenderName composes the "自定义名 · 供应商" display name (D-27③). The
// prefs' display name wins; the vendor label fills the right half. When either
// is unset the reported value is used.
func (p ModelPrefs) RenderName(reportedName, reportedVendor string) string {
	display := reportedName
	if p.DisplayName != nil && strings.TrimSpace(*p.DisplayName) != "" {
		display = *p.DisplayName
	}
	vendor := reportedVendor
	if p.VendorLabel != nil && strings.TrimSpace(*p.VendorLabel) != "" {
		vendor = *p.VendorLabel
	}
	return presets.FormatModelName(display, vendor, "")
}

// CutEfforts returns the kept reasoning levels for a reported level list. A
// nil Efforts keeps everything; a set Efforts keeps only the levels the user
// listed, preserving the reported order (D-27④).
func (p ModelPrefs) CutEfforts(reported []string) []string {
	if p.Efforts == nil {
		return append([]string(nil), reported...)
	}
	want := make(map[string]bool, len(*p.Efforts))
	for _, level := range *p.Efforts {
		want[level] = true
	}
	out := make([]string, 0, len(*p.Efforts))
	for _, level := range reported {
		if want[level] {
			out = append(out, level)
		}
	}
	return out
}

// ApplyInfo merges the personalisation into a plugin-reported ModelInfo: the
// rename and the vendor label are applied first, the effort cut replaces the
// reported Thinking list. Unset fields keep the reported value.
func (p ModelPrefs) ApplyInfo(info model.ModelInfo) model.ModelInfo {
	if p.DisplayName != nil {
		info.Name = *p.DisplayName
	}
	if p.VendorLabel != nil {
		info.Vendor = *p.VendorLabel
	}
	info.Thinking = p.CutEfforts(info.Thinking)
	return info
}

// ApplyEntry is ApplyInfo for a stored catalog row.
func (p ModelPrefs) ApplyEntry(entry model.ModelCatalogEntry) model.ModelCatalogEntry {
	if p.DisplayName != nil {
		entry.DisplayName = *p.DisplayName
	}
	if p.VendorLabel != nil {
		entry.Vendor = *p.VendorLabel
	}
	entry.Thinking = p.CutEfforts(entry.Thinking)
	return entry
}

// GetModelPrefs returns one plugin's pref rows keyed by model id. An unknown
// plugin yields an empty map.
func (s *Store) GetModelPrefs(pluginID string) (map[string]ModelPrefs, error) {
	out := make(map[string]ModelPrefs)
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: get model prefs: store is not open")
	}
	pluginID = strings.TrimSpace(pluginID)
	if pluginID == "" {
		return nil, fmt.Errorf("store: get model prefs: %w: plugin_id is required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT model_id, display_name, vendor_label, efforts,
		COALESCE(updated_at, 0)
		FROM model_prefs WHERE plugin_id = ? ORDER BY model_id`, pluginID)
	if err != nil {
		return nil, fmt.Errorf("store: get model prefs: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			modelID string
			entry   ModelPrefs
			name    sql.NullString
			vendor  sql.NullString
			efforts sql.NullString
		)
		if err := rows.Scan(&modelID, &name, &vendor, &efforts, &entry.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: get model prefs: scan: %w", err)
		}
		if name.Valid {
			value := name.String
			entry.DisplayName = &value
		}
		if vendor.Valid {
			value := vendor.String
			entry.VendorLabel = &value
		}
		if efforts.Valid && strings.TrimSpace(efforts.String) != "" {
			var levels []string
			if err := json.Unmarshal([]byte(efforts.String), &levels); err != nil {
				return nil, fmt.Errorf("store: get model prefs: decode efforts for %q: %w", modelID, err)
			}
			entry.Efforts = &levels
		}
		out[modelID] = entry
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: get model prefs: iterate: %w", err)
	}
	return out, nil
}

// PutModelPrefs writes one pref row. A pref with no field set removes the row
// instead, so the model falls back to the reported values.
func (s *Store) PutModelPrefs(pluginID, modelID string, p ModelPrefs) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: put model prefs: store is not open")
	}
	pluginID = strings.TrimSpace(pluginID)
	modelID = strings.TrimSpace(modelID)
	if pluginID == "" || modelID == "" {
		return fmt.Errorf("store: put model prefs: %w: plugin_id and model_id are required", ErrInvalidArgument)
	}
	entry, err := p.normalize()
	if err != nil {
		return err
	}
	if entry.Empty() {
		return s.DeleteModelPrefs(pluginID, modelID)
	}

	var efforts string
	if entry.Efforts != nil {
		data, err := json.Marshal(*entry.Efforts)
		if err != nil {
			return fmt.Errorf("store: put model prefs: encode efforts: %w", err)
		}
		efforts = string(data)
	}

	ctx, cancel := s.ctx()
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO model_prefs
		(plugin_id, model_id, display_name, vendor_label, efforts, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(plugin_id, model_id) DO UPDATE SET
			display_name = excluded.display_name,
			vendor_label = excluded.vendor_label,
			efforts = excluded.efforts,
			updated_at = excluded.updated_at`,
		pluginID, modelID, nullStr(derefString(entry.DisplayName)), nullStr(derefString(entry.VendorLabel)),
		nullStr(efforts), nowUnix()); err != nil {
		return fmt.Errorf("store: put model prefs: %w", err)
	}
	return nil
}

// DeleteModelPrefs removes one pref row. Deleting a missing row is not an
// error: the admin API stays idempotent.
func (s *Store) DeleteModelPrefs(pluginID, modelID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: delete model prefs: store is not open")
	}
	pluginID = strings.TrimSpace(pluginID)
	modelID = strings.TrimSpace(modelID)
	if pluginID == "" || modelID == "" {
		return fmt.Errorf("store: delete model prefs: %w: plugin_id and model_id are required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM model_prefs WHERE plugin_id = ? AND model_id = ?`, pluginID, modelID); err != nil {
		return fmt.Errorf("store: delete model prefs: %w", err)
	}
	return nil
}
