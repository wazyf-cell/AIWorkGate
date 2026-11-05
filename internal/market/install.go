package market

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"aiworkgate/internal/model"
)

// Size and entry caps, mirroring internal/plugins/install.go (MaxZipBytes,
// maxZipEntries): the same limits apply to a tarball so a malicious package can
// neither fill the disk nor create unbounded numbers of files. Mirrored here
// because this package must not import internal/plugins.
const (
	MaxTarballBytes   = 256 << 20
	MaxExtractedBytes = 1 << 30
	maxTarEntries     = 4096
	maxManifestBytes  = 1 << 20
)

// Options carries the choices of one Prepare call.
type Options struct {
	// Registry is the injected npm client; nil uses the installer's default.
	Registry *Registry
	// ScratchDir is the base for staging directories; empty uses os.TempDir().
	ScratchDir string

	// FinalPluginDir is the directory the plugin will finally live in
	// (<dataDir>\plugins\<id>), used to build the bridge manifest
	// (entry.args --plugin-dir). Required when the package ships no manifest.
	FinalPluginDir string
	// BridgeExecutable is the shared magpie-bridge.exe to deploy into the
	// prepared directory under the generated entry.exe name. Required when a
	// bridge manifest is generated; the host's install path checks entry.exe
	// existence after copy.
	BridgeExecutable string
	// NetworkDomains feeds the generated bridge manifest's
	// permissions.network_domains.
	NetworkDomains []string
	// StartupMode / IdleTimeoutS pass through to the generated bridge manifest.
	StartupMode  string
	IdleTimeoutS int

	// ExpectedSHA256, when non-empty, must equal the sha256 of the downloaded
	// tarball; a mismatch rejects the install.
	ExpectedSHA256 string
}

// InstallRecord is the audit row a prepared install carries. Enabled is always
// false: enabling is a deliberate host-side decision (T3).
type InstallRecord struct {
	Source      string `json:"source"`
	Kind        string `json:"kind"`
	Package     string `json:"package,omitempty"`
	Version     string `json:"version"`
	TarballURL  string `json:"tarball_url,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
	PluginID    string `json:"plugin_id"`
	Enabled     bool   `json:"enabled"`
	InstalledAt int64  `json:"installed_at"`
}

// Prepared is the deliverable of the install pipeline: a self-contained staging
// directory ("待装目录") with manifest.json (and the bridge executable when a
// bridge manifest was generated) plus the record. Call Cleanup to remove Root
// when the host does not take it over.
type Prepared struct {
	Root     string
	Manifest model.Manifest
	Record   InstallRecord
}

// Installer prepares market packages. The zero value works with default
// registry and temp staging; NewInstaller fixes a registry and a scratch base.
type Installer struct {
	registry *Registry
	scratch  string
	now      func() time.Time
}

// NewInstaller builds an Installer with an optional default registry and
// scratch base for staging.
func NewInstaller(reg *Registry, scratch string) *Installer {
	return &Installer{registry: reg, scratch: strings.TrimSpace(scratch), now: time.Now}
}

// Cleanup removes a prepared directory that was not taken over.
func (in *Installer) Cleanup(p *Prepared) error {
	if p == nil || p.Root == "" {
		return nil
	}
	return os.RemoveAll(p.Root)
}

// Prepare runs the whole pipeline for one spec and returns the prepared
// directory + record, or an error before any state is committed.
func (in *Installer) Prepare(ctx context.Context, rawSpec string, opts Options) (*Prepared, error) {
	spec, err := ParseSpec(rawSpec)
	if err != nil {
		return nil, err
	}
	switch spec.Kind {
	case KindGitHub:
		return nil, fmt.Errorf("%w: spec %q", ErrGitHubUnreachable, rawSpec)
	case KindLocal:
		return in.prepareLocal(ctx, spec, opts)
	case KindNPM:
		return in.prepareNPM(ctx, spec, opts)
	default:
		return nil, fmt.Errorf("market: unsupported source kind %v", spec.Kind)
	}
}

// registryOf returns opts.Registry, else the installer default, else the stock
// default registry.
func (in *Installer) registryOf(opts Options) (*Registry, error) {
	if opts.Registry != nil {
		return opts.Registry, nil
	}
	if in.registry != nil {
		return in.registry, nil
	}
	return NewRegistry("", nil)
}

func (in *Installer) stagingBase(opts Options) string {
	if strings.TrimSpace(opts.ScratchDir) != "" {
		return strings.TrimSpace(opts.ScratchDir)
	}
	if in.scratch != "" {
		return in.scratch
	}
	return os.TempDir()
}

func (in *Installer) nowUnix() int64 {
	if in.now != nil {
		return in.now().Unix()
	}
	return time.Now().Unix()
}

func (in *Installer) newStaging(pluginID string, opts Options) (string, error) {
	base := in.stagingBase(opts)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", fmt.Errorf("market: create staging base %q: %w", base, err)
	}
	return os.MkdirTemp(base, "market-"+pluginID+"-")
}
