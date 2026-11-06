//go:build !windows

package main

import "errors"

// On a non-Windows host there is no DPAPI. The plugin still builds (so the
// module can be vetted anywhere), but it refuses to persist credentials rather
// than writing them in the clear.
func sealKind() string { return "none" }

func seal([]byte) ([]byte, error) {
	return nil, errors.New("zcode: DPAPI is only available on Windows; refusing to store credentials in the clear")
}

func open([]byte) ([]byte, error) {
	return nil, errors.New("zcode: DPAPI is only available on Windows")
}
