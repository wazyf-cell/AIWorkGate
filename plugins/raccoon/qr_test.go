package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// qrMatrixDigest is the sha256 of the matrix rendered as rows of '0'/'1'.
func qrMatrixDigest(t *testing.T, text string) (int, string) {
	t.Helper()
	m, err := buildQRMatrix(text)
	if err != nil {
		t.Fatalf("buildQRMatrix(%q): %v", text, err)
	}
	var b strings.Builder
	for y := range m {
		for x := range m[y] {
			if m[y][x] {
				b.WriteByte('1')
			} else {
				b.WriteByte('0')
			}
		}
		if y != len(m)-1 {
			b.WriteByte('\n')
		}
	}
	sum := sha256.Sum256([]byte(b.String()))
	return len(m), hex.EncodeToString(sum[:])
}

// TestQRMatrixMatchesReference pins the encoder to the reference implementation
// (raccoon-qr.ts, extracted from pack.js and run under node on 2026-10-22).
// A byte-identical matrix is the only evidence that the QR a user scans carries
// the payload we think it does.
func TestQRMatrixMatchesReference(t *testing.T) {
	cases := []struct {
		text string
		size int
		want string
	}{
		// version 1: the shortest payload worth encoding ("hello-raccoon").
		{"hello-raccoon", 21, "2c362938376c62a9bf05eb27eae214fda36bb4a384b6de3636362ca45c8d4b00"},
		// version 7: the real /login/mp deep link with a fixed code.
		{"https://xiaohuanxiong.com/login/mp?code=0123456789abcdef0123456789abcdef&appname=%E5%95%86%E6%B1%A4%E5%B0%8F%E6%B5%A3%E7%86%8A%E5%AE%98%E7%BD%91", 49, "774915f2411f7181d74b7d60fcb1c7e7c5cef8fc1263c1ec8a1e56f7186798b0"},
		// version 7 with a different code: proves the data codewords flow through.
		{"https://xiaohuanxiong.com/login/mp?code=9d2aa34ad7eb2df600117b6c588e0db5&appname=%E5%95%86%E6%B1%A4%E5%B0%8F%E6%B5%A3%E7%86%8A%E5%AE%98%E7%BD%91", 49, "7086e995bcb62fb242ed8c8b5b35b1e817933f2b24a6f3ec2174639441666bfb"},
	}
	for _, c := range cases {
		size, got := qrMatrixDigest(t, c.text)
		if size != c.size || got != c.want {
			t.Errorf("matrix for %q = size %d sha %s, want size %d sha %s", c.text, size, got, c.size, c.want)
		}
	}
}

// TestQRSVGIsInlineImage checks the page gets a self-contained SVG (no external
// request, no data: URL the host UI would have to trust).
func TestQRSVGIsInlineImage(t *testing.T) {
	svg, err := renderQRSVG("hello-raccoon", 240, 4)
	if err != nil {
		t.Fatalf("renderQRSVG: %v", err)
	}
	if !strings.HasPrefix(svg, "<svg") || !strings.HasSuffix(svg, "</svg>") {
		t.Fatalf("svg shape = %q", truncate(svg, 60))
	}
	if got := strings.Count(svg, "M"); got < 20 {
		t.Fatalf("svg has %d dark module runs, want a real matrix", got)
	}
}

func TestQROverlongPayloadIsAnError(t *testing.T) {
	if _, err := buildQRMatrix(strings.Repeat("x", 214)); err == nil {
		t.Fatal("buildQRMatrix accepted a payload beyond version 10 capacity")
	}
}
