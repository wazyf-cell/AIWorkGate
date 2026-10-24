//go:build !windows

package main

import "fmt"

// readWinCredBlob is stubbed off Windows: OfficeAce itself only ships for
// Windows, so this path only exists to keep the package buildable in tests.
var readWinCredBlob = func(target string) ([]byte, error) {
	return nil, fmt.Errorf("Windows 凭据管理器仅在 Windows 上可用")
}
