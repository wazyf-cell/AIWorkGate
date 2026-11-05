package market

import "testing"

func TestParseVersionAndCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0},
		{"1.2.3", "1.2.4", -1},
		{"1.2.3", "1.3.0", -1},
		{"1.9.9", "2.0.0", -1},
		{"2.0.0", "1.99.99", 1},
		{"1.2", "1.2.0", 0},
		{"1", "1.0.0", 0},
		{"2.0.0-rc1", "2.0.0", -1},
		{"2.0.0", "2.0.0-rc1", 1},
	}
	for _, tc := range cases {
		if got := CompareVersions(tc.a, tc.b); got != tc.want {
			t.Fatalf("CompareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
	if CompareVersions("garbage", "1.0.0") != -1 {
		t.Fatal("unparsable version must compare as older")
	}
	if CompareVersions("1.0.0", "garbage") != 1 {
		t.Fatal("an unparsable candidate must never win")
	}
}

func TestMatchesConstraint(t *testing.T) {
	cases := []struct {
		version, constraint string
		want                bool
	}{
		{"1.2.3", "", true},
		{"1.2.3", "latest", true},
		{"1.2.3", "*", true},
		{"1.2.3", "1.2.3", true},
		{"1.2.3", "=1.2.3", true},
		{"1.2.4", "1.2.3", false},
		{"1.5.0", "^1.2.0", true},
		{"1.2.0", "^1.2.0", true},
		{"2.0.0", "^1.2.0", false},
		{"1.2.9", "~1.2.3", true},
		{"1.3.0", "~1.2.3", false},
		{"1.3.0", ">=1.2.0", true},
		{"1.1.0", ">=1.2.0", false},
		{"1.2.0", "<1.3.0", true},
		{"1.3.0", "<1.3.0", false},
		{"1.2.9", "<=1.2.9", true},
		{"1.2.9", ">1.2.9", false},
		{"2.0.0-rc1", "^2.0.0", false},
		{"1.2.3", "banana", false},
	}
	for _, tc := range cases {
		if got := MatchesConstraint(tc.version, tc.constraint); got != tc.want {
			t.Fatalf("MatchesConstraint(%q, %q) = %v, want %v", tc.version, tc.constraint, got, tc.want)
		}
	}
}
