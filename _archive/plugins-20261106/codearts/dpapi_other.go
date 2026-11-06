//go:build !windows

package main

import "errors"

// The CodeArts plugin only ships for Windows. Keeping this stub makes
// `go vet ./...` on other platforms fail fast with a clear message instead of
// silently storing credentials without DPAPI.
var errDPAPIUnsupported = errors.New("DPAPI is only available on Windows")

func dpapiProtect([]byte) ([]byte, error)   { return nil, errDPAPIUnsupported }
func dpapiUnprotect([]byte) ([]byte, error) { return nil, errDPAPIUnsupported }
