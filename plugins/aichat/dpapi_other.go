//go:build !windows

package main

// sealKind names the degraded protection used off Windows. The plugin only
// ships for windows (manifest "os": ["windows"]); this build exists solely so
// `go build`, `go vet` and `go test` can run on a Linux CI box, where DPAPI
// does not exist.
func sealKind() string { return "plaintext-file-mode" }

// seal stores the payload as-is; store.go still writes it with mode 0600 inside
// a 0700 directory, which is the documented fallback when DPAPI is unavailable.
func seal(plain []byte) ([]byte, error) { return plain, nil }

// open is the identity of seal in the fallback build.
func open(blob []byte) ([]byte, error) { return blob, nil }
