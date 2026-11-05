package market

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// prepareNPM runs download -> sha256 -> extract -> manifest for an npm source.
func (in *Installer) prepareNPM(ctx context.Context, spec Spec, opts Options) (*Prepared, error) {
	reg, err := in.registryOf(opts)
	if err != nil {
		return nil, err
	}
	meta, err := reg.Metadata(ctx, spec.Name)
	if err != nil {
		return nil, err
	}
	version, err := resolveVersion(meta, spec.Constraint)
	if err != nil {
		return nil, err
	}
	pv, ok := meta.Versions[version]
	if !ok {
		return nil, fmt.Errorf("market: version %q of %q is missing from registry metadata", version, spec.Name)
	}
	if strings.TrimSpace(pv.Dist.Tarball) == "" {
		return nil, fmt.Errorf("market: %q@%s has no tarball in registry metadata", spec.Name, version)
	}

	data, err := reg.Fetch(ctx, pv.Dist.Tarball, MaxTarballBytes)
	if err != nil {
		return nil, err
	}
	sha := sha256Hex(data)
	if opts.ExpectedSHA256 != "" && !strings.EqualFold(sha, opts.ExpectedSHA256) {
		return nil, fmt.Errorf("market: %q@%s tarball sha256 mismatch: got %s, want %s", spec.Name, version, sha, opts.ExpectedSHA256)
	}

	pluginID, err := PluginIDFor(spec.Name)
	if err != nil {
		return nil, err
	}
	staging, err := in.newStaging(pluginID, opts)
	if err != nil {
		return nil, err
	}
	okSoFar := false
	defer func() {
		if !okSoFar {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := extractTarball(data, staging); err != nil {
		return nil, err
	}
	payloadRoot, err := findPayloadRoot(staging)
	if err != nil {
		return nil, err
	}
	manifest, generated, err := in.ensureManifest(payloadRoot, manifestSeed{
		Spec: spec, PluginID: pluginID, Version: version, Opts: opts,
	})
	if err != nil {
		return nil, err
	}
	if generated {
		if err := deployBridgeExecutable(opts, payloadRoot, manifest); err != nil {
			return nil, err
		}
	}
	if err := writeManifestFile(payloadRoot, manifest); err != nil {
		return nil, err
	}

	record := InstallRecord{
		Source:      spec.String(),
		Kind:        spec.Kind.String(),
		Package:     spec.Name,
		Version:     manifest.Version,
		TarballURL:  pv.Dist.Tarball,
		SHA256:      sha,
		PluginID:    manifest.ID,
		Enabled:     false,
		InstalledAt: in.nowUnix(),
	}
	if record.Version == "" {
		record.Version = version
	}
	okSoFar = true
	return &Prepared{Root: payloadRoot, Manifest: manifest, Record: record}, nil
}

// prepareLocal copies a local directory into staging and applies the same
// manifest policy (package manifest kept, otherwise a generated bridge one).
// No network, no tarball, no sha256.
func (in *Installer) prepareLocal(_ context.Context, spec Spec, opts Options) (*Prepared, error) {
	info, err := os.Stat(spec.Dir)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("market: local source %q is not an existing directory", spec.Dir)
	}
	base := filepath.Base(filepath.Clean(spec.Dir))
	pluginID, err := PluginIDFor(base)
	if err != nil {
		return nil, fmt.Errorf("market: local source %q: %w", spec.Dir, err)
	}
	staging, err := in.newStaging(pluginID, opts)
	if err != nil {
		return nil, err
	}
	okSoFar := false
	defer func() {
		if !okSoFar {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := copyTree(spec.Dir, staging); err != nil {
		return nil, err
	}
	payloadRoot, err := findPayloadRoot(staging)
	if err != nil {
		return nil, err
	}
	manifest, generated, err := in.ensureManifest(payloadRoot, manifestSeed{
		Spec: spec, PluginID: pluginID, Version: "", Opts: opts,
	})
	if err != nil {
		return nil, err
	}
	if generated {
		if err := deployBridgeExecutable(opts, payloadRoot, manifest); err != nil {
			return nil, err
		}
		if err := writeManifestFile(payloadRoot, manifest); err != nil {
			return nil, err
		}
	}
	version := manifest.Version
	if version == "" {
		version = "0.0.0-local"
	}
	record := InstallRecord{
		Source:      spec.String(),
		Kind:        spec.Kind.String(),
		Version:     version,
		PluginID:    manifest.ID,
		Enabled:     false,
		InstalledAt: in.nowUnix(),
	}
	okSoFar = true
	return &Prepared{Root: payloadRoot, Manifest: manifest, Record: record}, nil
}

// resolveVersion picks the version to install: dist-tags.latest (or the maximum
// version) for an empty/"latest"/"*" constraint, otherwise the highest version
// satisfying the constraint.
func resolveVersion(meta PackageMetadata, constraint string) (string, error) {
	c := strings.TrimSpace(constraint)
	if c == "" || c == "latest" || c == "*" {
		if v, ok := meta.DistTags["latest"]; ok && v != "" {
			if _, ok := meta.Versions[v]; ok {
				return v, nil
			}
		}
		return maxVersion(meta.Versions)
	}
	var best string
	for v := range meta.Versions {
		if !MatchesConstraint(v, c) {
			continue
		}
		if best == "" || CompareVersions(v, best) > 0 {
			best = v
		}
	}
	if best == "" {
		return "", fmt.Errorf("market: %q has no version satisfying %q", meta.Name, c)
	}
	return best, nil
}

func maxVersion(versions map[string]PackageVersion) (string, error) {
	if len(versions) == 0 {
		return "", errors.New("market: package has no versions")
	}
	var best string
	for v := range versions {
		if best == "" || CompareVersions(v, best) > 0 {
			best = v
		}
	}
	return best, nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
