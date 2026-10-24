package secrets

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// DPAPI flags: user scope (no CRYPTPROTECT_LOCAL_MACHINE) and no UI prompts.
const dpapiFlags = windows.CRYPTPROTECT_UI_FORBIDDEN

// protect seals plain with DPAPI in the current user's scope.
func protect(plain []byte) ([]byte, error) {
	if len(plain) == 0 {
		return nil, fmt.Errorf("secrets: refusing to encrypt an empty blob")
	}
	in := windows.DataBlob{Size: uint32(len(plain)), Data: &plain[0]}
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, nil, 0, nil, dpapiFlags, &out); err != nil {
		return nil, fmt.Errorf("secrets: CryptProtectData: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return copyDataBlob(out), nil
}

// unprotect opens a DPAPI blob produced by protect for the current user.
func unprotect(sealed []byte) ([]byte, error) {
	if len(sealed) == 0 {
		return nil, fmt.Errorf("secrets: empty sealed blob")
	}
	in := windows.DataBlob{Size: uint32(len(sealed)), Data: &sealed[0]}
	var out windows.DataBlob
	var description *uint16
	if err := windows.CryptUnprotectData(&in, &description, nil, 0, nil, dpapiFlags, &out); err != nil {
		return nil, fmt.Errorf("secrets: CryptUnprotectData: %w", err)
	}
	if description != nil {
		windows.LocalFree(windows.Handle(unsafe.Pointer(description)))
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return copyDataBlob(out), nil
}

// copyDataBlob copies a DPAPI-owned buffer before it is freed.
func copyDataBlob(b windows.DataBlob) []byte {
	if b.Size == 0 || b.Data == nil {
		return []byte{}
	}
	out := make([]byte, int(b.Size))
	copy(out, unsafe.Slice(b.Data, int(b.Size)))
	return out
}
