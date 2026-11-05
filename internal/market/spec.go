package market

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Kind identifies the source family a spec parsed into.
type Kind int

const (
	// KindNPM is an npm registry package.
	KindNPM Kind = iota
	// KindLocal is an existing directory.
	KindLocal
	// KindGitHub is a github:owner/repo spec, currently refused with
	// ErrGitHubUnreachable (see the package doc).
	KindGitHub
)

func (k Kind) String() string {
	switch k {
	case KindNPM:
		return "npm"
	case KindLocal:
		return "local"
	case KindGitHub:
		return "github"
	default:
		return "unknown"
	}
}

// ErrGitHubUnreachable is returned for any operation on a GitHub spec. The box
// currently has no reachable proxy for github.com, and failing loud with a
// fixable message beats hanging or silently doing nothing.
var ErrGitHubUnreachable = errors.New("market: github source is unreachable (configure a proxy)")

// pluginIDPattern mirrors the host's plugin id rule (internal/plugins/
// manifest.go pluginIDPattern): any package a market installs must end up with
// an id the supervisor accepts.
var pluginIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,31}$`)

// Spec is one parsed market reference.
type Spec struct {
	Raw  string
	Kind Kind

	// npm: package name (@scope/name or name) and an optional version
	// constraint ("", "latest", "1.2.3", "^1.2.3", "~1.2.3", ">=1.2", "*").
	Name       string
	Constraint string

	// local: the directory path.
	Dir string

	// github: owner/repo.
	Owner string
	Repo  string
}

func (s Spec) String() string {
	if s.Raw != "" {
		return s.Raw
	}
	switch s.Kind {
	case KindNPM:
		return s.Name
	case KindLocal:
		return s.Dir
	case KindGitHub:
		return "github:" + s.Owner + "/" + s.Repo
	default:
		return ""
	}
}

// ParseSpec classifies a market reference. The rules are ordered: an existing
// directory wins, then the explicit prefixes, then a github.com URL, then
// everything else is treated as an npm name (bare, scoped, npm:…).
func ParseSpec(raw string) (Spec, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Spec{}, errors.New("market: empty plugin spec")
	}

	// Local directory (path form).
	if p := raw; !strings.HasPrefix(p, "npm:") && !strings.HasPrefix(p, "github:") && !strings.HasPrefix(p, "gh:") && !looksLikeURL(p) {
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			return Spec{Raw: raw, Kind: KindLocal, Dir: filepath.Clean(p)}, nil
		}
	}
	// file:… local form.
	if rest, ok := strings.CutPrefix(raw, "file:"); ok {
		dir := strings.Trim(strings.TrimSpace(rest), `"'`)
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			return Spec{}, fmt.Errorf("market: local source %q is not an existing directory", dir)
		}
		return Spec{Raw: raw, Kind: KindLocal, Dir: filepath.Clean(dir)}, nil
	}

	// Explicit GitHub prefixes.
	if rest, ok := strings.CutPrefix(raw, "github:"); ok {
		return githubSpec(raw, rest)
	}
	if rest, ok := strings.CutPrefix(raw, "gh:"); ok {
		return githubSpec(raw, rest)
	}
	// github.com URL.
	if looksLikeURL(raw) {
		if u, err := parseHost(raw); err == nil && (u == "github.com" || strings.HasSuffix(u, ".github.com")) {
			rest := raw
			if i := strings.Index(rest, "//"); i >= 0 {
				rest = rest[i+2:]
			}
			if j := strings.IndexByte(rest, '/'); j >= 0 {
				rest = rest[j+1:]
			}
			parts := strings.Split(strings.Trim(rest, "/"), "/")
			if len(parts) >= 2 {
				return Spec{Raw: raw, Kind: KindGitHub, Owner: parts[0], Repo: strings.TrimSuffix(parts[1], ".git")}, nil
			}
			return Spec{}, fmt.Errorf("market: github spec %q must be github:owner/repo", raw)
		}
		return Spec{}, fmt.Errorf("market: unsupported URL spec %q (use an npm spec, a local path, or github:owner/repo)", raw)
	}

	// npm: prefix, then anything else is an npm name.
	if rest, ok := strings.CutPrefix(raw, "npm:"); ok {
		return npmSpec(raw, rest)
	}
	return npmSpec(raw, raw)
}

func githubSpec(raw, rest string) (Spec, error) {
	rest = strings.Trim(strings.TrimSpace(rest), "/")
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return Spec{}, fmt.Errorf("market: github spec %q must be github:owner/repo", raw)
	}
	return Spec{
		Raw:   raw,
		Kind:  KindGitHub,
		Owner: strings.TrimSpace(parts[0]),
		Repo:  strings.TrimSuffix(strings.TrimSpace(parts[1]), ".git"),
	}, nil
}

// npmSpec splits "name" or "@scope/name@constraint" into name and constraint.
// The version separator is the last "@" that is not the leading scope marker,
// and the scope marker itself is preserved in the package name.
func npmSpec(raw, ref string) (Spec, error) {
	ref = strings.TrimSpace(ref)
	name, constraint := ref, ""
	if i := strings.LastIndex(ref, "@"); i > 0 {
		name, constraint = ref[:i], strings.TrimSpace(ref[i+1:])
	}
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, " \t\r\n") {
		return Spec{}, fmt.Errorf("market: npm spec %q has an invalid package name", raw)
	}
	if strings.HasPrefix(name, "@") {
		if slash := strings.IndexByte(name, '/'); slash <= 1 || slash == len(name)-1 || strings.ContainsAny(name[slash+1:], " /@") {
			return Spec{}, fmt.Errorf("market: npm spec %q has an invalid scoped name %q", raw, name)
		}
	} else if strings.Contains(name, "/") {
		return Spec{}, fmt.Errorf("market: npm spec %q has an invalid name %q", raw, name)
	}
	return Spec{Raw: raw, Kind: KindNPM, Name: name, Constraint: constraint}, nil
}

func looksLikeURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// parseHost is a tiny host extractor for spec URLs; it never performs a lookup.
func parseHost(s string) (string, error) {
	rest := s
	if i := strings.Index(rest, "//"); i >= 0 {
		rest = rest[i+2:]
	}
	host := rest
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	if host == "" {
		return "", errors.New("market: no host")
	}
	return strings.ToLower(host), nil
}

// PluginIDFor derives a plugin id from a package name: lowercase, keep
// [a-z0-9-], fold everything else to "-", trim runs and edges, cap at 32. The
// host's manifest id rule is ^[a-z0-9][a-z0-9-]{1,31}$.
// PluginIDFor derives a plugin id from a package name. The magpie plugin id
// convention is the unscoped package name (opencode-trae-auth), so a scoped
// name keeps only the segment after "/"; other characters fold to "-", and a
// name still longer than the host's 32-char limit gets a deterministic sha256
// suffix instead of a silent truncation that would collide.
func PluginIDFor(pkg string) (string, error) {
	base := strings.TrimSpace(pkg)
	if strings.HasPrefix(base, "@") {
		if i := strings.LastIndexByte(base, '/'); i >= 0 {
			base = base[i+1:]
		}
	}
	folded := foldID(base)
	if len(folded) > 32 {
		sum := sha256.Sum256([]byte(strings.ToLower(base)))
		suffix := hex.EncodeToString(sum[:4])
		folded = strings.Trim(folded[:32-len(suffix)-1], "-") + "-" + suffix
	}
	if !pluginIDPattern.MatchString(folded) {
		return "", fmt.Errorf("market: package %q does not map to a valid plugin id", pkg)
	}
	return folded, nil
}

// foldID lowercases and folds every non-[a-z0-9-] run into a single dash.
func foldID(base string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(base) {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		switch {
		case ok:
			b.WriteRune(r)
			lastDash = false
		default:
			// dash, underscore, dot, slash, spaces, unicode: fold to a dash
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
