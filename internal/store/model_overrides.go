package store

// Human override layer for the model catalog (plan 2026-10-21 §3).
//
// Ported from A's unified_catalog.rs L1 meta layer (MIT, 朱天伟): the
// `TraeModelMeta` struct plus `load_meta` / `meta_set` / `meta_clear`
// (unified_catalog.rs:165-210) give every editable field an "unspecified"
// state, and `trae_entry_of` (unified_catalog.rs:332-381) resolves one entry
// through the L1 override -> L2 plugin report -> L3 static fallback chain.
// The rules carried over here are the important ones:
//
//   - a NULL column means "no human value", so the value reported by the
//     plugin (or A) stays in force; an empty string is therefore stored as
//     NULL, never as an override;
//   - the override layer is written only by the admin editor; catalog sync
//     (ReplaceModelCatalog) never touches this table, so a refresh can never
//     clobber a hand-edited field;
//   - clearing the row restores the automatic chain immediately.
//
// The table itself ships with migration v4 (model_overrides); this file only
// reads and writes it.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"aiworkgate/internal/model"
)

const (
	// maxOverrideNameRunes bounds a human display name; it matches the width
	// the admin UI accepts so both layers reject the same input.
	maxOverrideNameRunes = 128
	// maxOverrideVendorRunes bounds a human vendor name.
	maxOverrideVendorRunes = 64
)

// ModelOverride is the human-maintained metadata of one catalog model. Every
// field is optional: nil means "not overridden" and the plugin/A reported
// value stays in force (A: `Option<T>` + `None = 该字段未人工指定`).
type ModelOverride struct {
	DisplayName   *string   `json:"display_name"`
	Vendor        *string   `json:"vendor"`
	ContextLength *int      `json:"context_length"`
	Vision        *bool     `json:"vision"`
	Thinking      *[]string `json:"thinking"`
	Multiplier    *float64  `json:"multiplier"`
	Free          *bool     `json:"free"`
	// UpdatedAt is the last write time. It is set by the store and ignored on
	// write, so clients cannot fake the timestamp.
	UpdatedAt int64 `json:"updated_at,omitempty"`
}

// Fields lists the overridden field names in the order the admin UI shows
// them. The result is what `overridden` carries to the browser.
func (o ModelOverride) Fields() []string {
	out := make([]string, 0, 7)
	if o.DisplayName != nil {
		out = append(out, "display_name")
	}
	if o.Vendor != nil {
		out = append(out, "vendor")
	}
	if o.ContextLength != nil {
		out = append(out, "context_length")
	}
	if o.Vision != nil {
		out = append(out, "vision")
	}
	if o.Thinking != nil {
		out = append(out, "thinking")
	}
	if o.Multiplier != nil {
		out = append(out, "multiplier")
	}
	if o.Free != nil {
		out = append(out, "free")
	}
	return out
}

// Empty reports an override that sets no field at all.
func (o ModelOverride) Empty() bool { return len(o.Fields()) == 0 }

// ApplyInfo replaces every overridden field of a plugin-reported ModelInfo.
// Fields without an override keep the reported value, which is the L1 -> L2
// step of A's resolution chain.
func (o ModelOverride) ApplyInfo(info model.ModelInfo) model.ModelInfo {
	if o.DisplayName != nil {
		info.Name = *o.DisplayName
	}
	if o.Vendor != nil {
		info.Vendor = *o.Vendor
	}
	if o.ContextLength != nil {
		info.ContextLength = *o.ContextLength
	}
	if o.Vision != nil {
		info.Vision = *o.Vision
	}
	if o.Thinking != nil {
		info.Thinking = append([]string(nil), (*o.Thinking)...)
	}
	if o.Multiplier != nil {
		info.Multiplier = *o.Multiplier
	}
	if o.Free != nil {
		info.Free = *o.Free
	}
	return info
}

// ApplyEntry is ApplyInfo for a stored catalog row. The resource page payload
// and the scheduler snapshot both use ModelCatalogEntry, so keeping the merge
// in one place lets either surface adopt it without re-deriving the rules.
func (o ModelOverride) ApplyEntry(entry model.ModelCatalogEntry) model.ModelCatalogEntry {
	if o.DisplayName != nil {
		entry.DisplayName = *o.DisplayName
	}
	if o.Vendor != nil {
		entry.Vendor = *o.Vendor
	}
	if o.ContextLength != nil {
		entry.ContextLength = *o.ContextLength
	}
	if o.Vision != nil {
		entry.Vision = *o.Vision
	}
	if o.Thinking != nil {
		entry.Thinking = append([]string(nil), (*o.Thinking)...)
	}
	if o.Multiplier != nil {
		entry.Multiplier = *o.Multiplier
	}
	if o.Free != nil {
		entry.Free = *o.Free
	}
	return entry
}

// normalize trims text fields, drops blank values (blank = not overridden) and
// rejects values the admin API would never accept. It is applied on write so a
// hand-written row can never break the merge.
func (o ModelOverride) normalize() (ModelOverride, error) {
	out := o
	out.UpdatedAt = 0

	if out.DisplayName != nil {
		value := strings.TrimSpace(*out.DisplayName)
		if value == "" {
			out.DisplayName = nil
		} else {
			if utf8.RuneCountInString(value) > maxOverrideNameRunes {
				return ModelOverride{}, fmt.Errorf("store: model override: %w: display_name longer than %d characters",
					ErrInvalidArgument, maxOverrideNameRunes)
			}
			out.DisplayName = &value
		}
	}
	if out.Vendor != nil {
		value := strings.TrimSpace(*out.Vendor)
		if value == "" {
			out.Vendor = nil
		} else {
			if utf8.RuneCountInString(value) > maxOverrideVendorRunes {
				return ModelOverride{}, fmt.Errorf("store: model override: %w: vendor longer than %d characters",
					ErrInvalidArgument, maxOverrideVendorRunes)
			}
			out.Vendor = &value
		}
	}
	if out.ContextLength != nil && *out.ContextLength <= 0 {
		return ModelOverride{}, fmt.Errorf("store: model override: %w: context_length must be positive",
			ErrInvalidArgument)
	}
	if out.Multiplier != nil {
		value := *out.Multiplier
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return ModelOverride{}, fmt.Errorf("store: model override: %w: multiplier must be a finite value >= 0",
				ErrInvalidArgument)
		}
	}
	if out.Thinking != nil {
		levels := make([]string, 0, len(*out.Thinking))
		seen := make(map[string]bool, len(*out.Thinking))
		for _, raw := range *out.Thinking {
			level := strings.TrimSpace(raw)
			if level == "" || seen[level] {
				continue
			}
			seen[level] = true
			levels = append(levels, level)
		}
		if len(levels) == 0 {
			out.Thinking = nil
		} else {
			out.Thinking = &levels
		}
	}
	return out, nil
}

// GetModelOverrides returns one plugin's override rows keyed by model id. An
// unknown plugin yields an empty map.
func (s *Store) GetModelOverrides(pluginID string) (map[string]ModelOverride, error) {
	out := make(map[string]ModelOverride)
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store: get model overrides: store is not open")
	}
	pluginID = strings.TrimSpace(pluginID)
	if pluginID == "" {
		return nil, fmt.Errorf("store: get model overrides: %w: plugin_id is required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT model,
		display_name, vendor, context_length, vision, thinking, multiplier, free,
		COALESCE(updated_at, 0)
		FROM model_overrides WHERE plugin_id = ? ORDER BY model`, pluginID)
	if err != nil {
		return nil, fmt.Errorf("store: get model overrides: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			modelID  string
			entry    ModelOverride
			name     sql.NullString
			vendor   sql.NullString
			ctxLen   sql.NullInt64
			vision   sql.NullInt64
			thinking sql.NullString
			mult     sql.NullFloat64
			free     sql.NullInt64
		)
		if err := rows.Scan(&modelID, &name, &vendor, &ctxLen, &vision, &thinking, &mult, &free,
			&entry.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: get model overrides: scan: %w", err)
		}
		if name.Valid {
			value := name.String
			entry.DisplayName = &value
		}
		if vendor.Valid {
			value := vendor.String
			entry.Vendor = &value
		}
		if ctxLen.Valid {
			value := int(ctxLen.Int64)
			entry.ContextLength = &value
		}
		if vision.Valid {
			value := vision.Int64 != 0
			entry.Vision = &value
		}
		if thinking.Valid && strings.TrimSpace(thinking.String) != "" {
			var levels []string
			if err := json.Unmarshal([]byte(thinking.String), &levels); err != nil {
				return nil, fmt.Errorf("store: get model overrides: decode thinking for %q: %w", modelID, err)
			}
			entry.Thinking = &levels
		}
		if mult.Valid {
			value := mult.Float64
			entry.Multiplier = &value
		}
		if free.Valid {
			value := free.Int64 != 0
			entry.Free = &value
		}
		out[modelID] = entry
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: get model overrides: iterate: %w", err)
	}
	return out, nil
}

// PutModelOverride writes one override row. An override with no field set
// removes the row instead (the "整条清空" case of the admin API), so the model
// falls back to the reported values.
func (s *Store) PutModelOverride(pluginID, modelID string, o ModelOverride) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: put model override: store is not open")
	}
	pluginID = strings.TrimSpace(pluginID)
	modelID = strings.TrimSpace(modelID)
	if pluginID == "" || modelID == "" {
		return fmt.Errorf("store: put model override: %w: plugin_id and model are required", ErrInvalidArgument)
	}
	entry, err := o.normalize()
	if err != nil {
		return err
	}
	if entry.Empty() {
		return s.DeleteModelOverride(pluginID, modelID)
	}

	var thinking string
	if entry.Thinking != nil {
		data, err := json.Marshal(*entry.Thinking)
		if err != nil {
			return fmt.Errorf("store: put model override: encode thinking: %w", err)
		}
		thinking = string(data)
	}

	ctx, cancel := s.ctx()
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO model_overrides
		(plugin_id, model, display_name, vendor, context_length, vision, thinking, multiplier, free, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(plugin_id, model) DO UPDATE SET
			display_name = excluded.display_name,
			vendor = excluded.vendor,
			context_length = excluded.context_length,
			vision = excluded.vision,
			thinking = excluded.thinking,
			multiplier = excluded.multiplier,
			free = excluded.free,
			updated_at = excluded.updated_at`,
		pluginID, modelID, nullStr(derefString(entry.DisplayName)), nullStr(derefString(entry.Vendor)),
		optInt(entry.ContextLength), optBool(entry.Vision), nullStr(thinking),
		optFloat(entry.Multiplier), optBool(entry.Free), nowUnix()); err != nil {
		return fmt.Errorf("store: put model override: %w", err)
	}
	return nil
}

// DeleteModelOverride removes one override row. Deleting a row that does not
// exist is not an error: the admin API stays idempotent.
func (s *Store) DeleteModelOverride(pluginID, modelID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: delete model override: store is not open")
	}
	pluginID = strings.TrimSpace(pluginID)
	modelID = strings.TrimSpace(modelID)
	if pluginID == "" || modelID == "" {
		return fmt.Errorf("store: delete model override: %w: plugin_id and model are required", ErrInvalidArgument)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM model_overrides WHERE plugin_id = ? AND model = ?`, pluginID, modelID); err != nil {
		return fmt.Errorf("store: delete model override: %w", err)
	}
	return nil
}

// derefString returns "" for a nil pointer so nullStr stores NULL.
func derefString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
