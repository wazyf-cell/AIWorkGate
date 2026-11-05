package market

import (
	"strconv"
	"strings"
)

// Version is the dotted numeric core of a semver string; prerelease/build
// suffixes are carried but never compared (this package only needs "is there a
// newer stable release", where prereleases are intentionally ignored).
type Version struct {
	Major, Minor, Patch int
	Prerelease          string
}

// ParseVersion parses x, x.y or x.y.z with optional -prerelease/+build.
func ParseVersion(s string) (Version, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Version{}, false
	}
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	pre := ""
	if i := strings.IndexByte(s, '-'); i >= 0 {
		pre, s = s[i+1:], s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) > 3 {
		return Version{}, false
	}
	nums := [3]int{}
	for i, p := range parts {
		if p == "" {
			return Version{}, false
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return Version{}, false
		}
		nums[i] = n
	}
	return Version{Major: nums[0], Minor: nums[1], Patch: nums[2], Prerelease: pre}, true
}

// Compare returns -1 (v < o), 0 or 1 (v > o). A version with a prerelease ranks
// below the same core without one, like semver.
func (v Version) Compare(o Version) int {
	if v.Major != o.Major {
		return cmpInt(v.Major, o.Major)
	}
	if v.Minor != o.Minor {
		return cmpInt(v.Minor, o.Minor)
	}
	if v.Patch != o.Patch {
		return cmpInt(v.Patch, o.Patch)
	}
	switch {
	case v.Prerelease == "" && o.Prerelease != "":
		return 1
	case v.Prerelease != "" && o.Prerelease == "":
		return -1
	case v.Prerelease == "" && o.Prerelease == "":
		return 0
	default:
		return strings.Compare(v.Prerelease, o.Prerelease)
	}
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// CompareVersions compares two version strings; an unparsable version behaves
// as -1 (older) so it can never block an update to a real version.
func CompareVersions(a, b string) int {
	va, aok := ParseVersion(a)
	vb, bok := ParseVersion(b)
	if !aok {
		return -1
	}
	if !bok {
		return 1
	}
	return va.Compare(vb)
}

// MatchesConstraint reports whether version satisfies a market constraint.
// "" and "latest" match everything (the caller picks the newest release);
// comparable operators are exact, ^, ~, *, >=, >, <=, <.
func MatchesConstraint(version, constraint string) bool {
	c := strings.TrimSpace(constraint)
	if c == "" || c == "latest" || c == "*" {
		return true
	}
	v, ok := ParseVersion(version)
	if !ok {
		return false
	}
	for _, op := range []string{">=", "<=", ">", "<", "^", "~", "="} {
		if !strings.HasPrefix(c, op) {
			continue
		}
		base, ok := ParseVersion(strings.TrimSpace(c[len(op):]))
		if !ok {
			return false
		}
		switch op {
		case "^":
			return v.Major == base.Major && v.Compare(base) >= 0
		case "~":
			return v.Major == base.Major && v.Minor == base.Minor && v.Compare(base) >= 0
		case ">=":
			return v.Compare(base) >= 0
		case ">":
			return v.Compare(base) > 0
		case "<=":
			return v.Compare(base) <= 0
		case "<":
			return v.Compare(base) < 0
		default: // "="
			return v.Compare(base) == 0
		}
	}
	// Plain dotted version: exact match.
	base, ok := ParseVersion(c)
	if !ok {
		return false
	}
	return v.Compare(base) == 0
}
