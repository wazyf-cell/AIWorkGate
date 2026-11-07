// Package market is the P2 plugin-market base: three package sources (npm, a
// GitHub branch-archive source, a local directory), an npm search wrapper, an
// install pipeline that ends in a prepared directory plus an install record, and
// an update check that reports from→to version diffs.
//
// # Where the pipeline ends
//
// Prepare produces a "待装目录" (a staging directory) containing the extracted
// payload, a manifest.json (the package's own when it ships one, otherwise a
// bridge manifest built by internal/opchost.GenerateBridgeManifest), the shared
// bridge executable deployed next to entry.exe, and an InstallRecord carrying
// source/version/SHA256. Nothing is moved into <dataDir>\plugins\<id> and
// nothing is enabled: Enabled is always false (T3, mirrors the host's own
// InstallOpts zero value). The host's install flow brings the prepared root in
// (see the wiring note below) and remains the only party that touches the
// installed tree, the settings row or the supervisor.
//
// # Sources
//
//   - npm: registry.npmjs.org by default, registry.npmmirror.com via
//     RegistryMirror (Registry base URL is a constructor argument, so any mirror
//     works). A bare name, @scope/name, @scope/name@constraint or npm:… spec
//     resolves through the registry metadata, downloads the version's tarball,
//     records its SHA256 and extracts it.
//   - GitHub: github:owner/repo (or gh:, or a github.com URL) parses into a
//     GitHub spec and installs the branch archive tarball
//     (github.com/<owner>/<repo>/archive/refs/heads/<branch>.tar.gz, D-04).
//     github:owner/repo@branch selects the branch; without one main is tried
//     first and master second. Transport failures wrap ErrGitHubUnreachable
//     (with the "configure a proxy" hint), a 404 on every tried branch wraps
//     ErrGitHubNotFound, and a corrupt archive fails at extraction with the
//     tarball error. The default transport honours the environment proxy, so
//     the same proxy rule that reaches the npm registry reaches github.com.
//   - local: an existing directory (or file:…/…). No network: the payload is
//     copied into the prepared root and a bridge manifest is generated when the
//     directory ships no manifest.json.
//
// # Security
//
// Tarballs are capped (MaxTarballBytes) before download completes, extraction
// rejects absolute paths, ".." escapes, symlinks and non-regular files, applies
// the same entry and total-size caps the host's zip installer uses
// (internal/plugins/install.go extractZip, mirrored here because this package
// must not depend on internal/plugins), and the SHA256 of the downloaded bytes
// is recorded and, when Options.ExpectedSHA256 pins a value, verified.
//
// # Network injection
//
// Every HTTP call goes through Registry, whose *http.Client and base URL are
// constructor arguments; unit tests drive it with httptest fake registries. The
// default transport honours the environment proxy (http.ProxyFromEnvironment),
// so a machine with a working system proxy reaches GitHub-class hosts.
//
// # Wiring note (what still needs the host, reported for coordination)
//
// internal/plugins/install.go has no exported path for "install this prepared
// directory, disabled": InstallDir enables the plugin and InstallZipOpts only
// takes a zip. The market flow wants a new exported wrapper, e.g.
// InstallDirOpts(srcDir, InstallOpts{Enabled:false}), delegating to the existing
// inspectInstallSource + installRoot (install.go:43-96, :136-218) after this
// package has prepared the tree; nothing in the supervisor or the manifest
// contract needs to change. Details in the change log / task receipt.
package market
