package market

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// UpdatePolicy is the stored strategy of an update check. The structure lands
// now; automatic execution is deliberately not implemented yet (plan §P2 item
// 4: updates default to prompt and only apply after hash-verified download).
type UpdatePolicy string

const (
	UpdatePolicyPrompt UpdatePolicy = "prompt"
	UpdatePolicyAuto   UpdatePolicy = "auto"
)

// UpdateInfo is the from→to version-diff report of one update check.
type UpdateInfo struct {
	PluginID   string       `json:"plugin_id"`
	Package    string       `json:"package"`
	From       string       `json:"from"`
	To         string       `json:"to"`
	Available  bool         `json:"available"`
	TarballURL string       `json:"tarball_url,omitempty"`
	Integrity  string       `json:"integrity,omitempty"`
	Policy     UpdatePolicy `json:"policy"`
	CheckedAt  int64        `json:"checked_at"`
}

// CheckUpdate asks the registry for the newest stable release of pkg and
// compares it with the installed current version. Prereleases never count as an
// update. Available reports whether an upgrade exists; To is always the newest
// stable release, so the diff (from→to) is visible even when nothing is
// available yet.
func CheckUpdate(ctx context.Context, reg *Registry, pkg, current string, policy UpdatePolicy) (UpdateInfo, error) {
	info := UpdateInfo{
		Package:   strings.TrimSpace(pkg),
		From:      strings.TrimSpace(current),
		To:        strings.TrimSpace(current),
		Policy:    policy,
		CheckedAt: time.Now().Unix(),
	}
	if id, err := PluginIDFor(info.Package); err == nil {
		info.PluginID = id
	}
	if reg == nil {
		var err error
		reg, err = NewRegistry("", nil)
		if err != nil {
			return info, err
		}
	}
	if info.Package == "" {
		return info, fmt.Errorf("market: update check: package name is required")
	}
	meta, err := reg.Metadata(ctx, info.Package)
	if err != nil {
		return info, err
	}
	latest, err := maxVersion(meta.Versions)
	if err != nil {
		return info, err
	}
	info.To = latest
	if v, ok := meta.DistTags["latest"]; ok && v != "" {
		if _, ok := meta.Versions[v]; ok {
			info.To = v
		}
	}
	info.Available = CompareVersions(info.To, info.From) > 0
	if pv, ok := meta.Versions[info.To]; ok {
		info.TarballURL = pv.Dist.Tarball
		info.Integrity = pv.Dist.Integrity
	}
	return info, nil
}
