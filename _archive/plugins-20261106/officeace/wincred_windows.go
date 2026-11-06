//go:build windows

package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

// OfficeAce 2026-10 builds (providerStateRef = wincred://...) keep the OAuth
// provider state in the Windows Credential Manager instead of storage.sqlite.
// CredReadW is a read-only lookup: the plugin never writes the vault.
const credTypeGeneric = 1

// readWinCredBlob is the seam tests replace so WinCred itself is never touched.
var readWinCredBlob = winCredRead

// winCredential mirrors CREDENTIALW (advapi32). Field order and alignment are
// load-bearing: the kernel writes this layout directly.
type winCredential struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        syscall.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

var (
	advapi32     = syscall.NewLazyDLL("advapi32.dll")
	procCredRead = advapi32.NewProc("CredReadW")
	procCredFree = advapi32.NewProc("CredFree")
)

func winCredRead(target string) ([]byte, error) {
	name, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return nil, fmt.Errorf("凭据名称无法转换为 UTF-16: %w", err)
	}
	var cred *winCredential
	r1, _, callErr := procCredRead.Call(
		uintptr(unsafe.Pointer(name)), credTypeGeneric, 0, uintptr(unsafe.Pointer(&cred)))
	if r1 == 0 {
		if callErr == nil {
			callErr = syscall.GetLastError()
		}
		return nil, callErr
	}
	defer procCredFree.Call(uintptr(unsafe.Pointer(cred)))
	if cred == nil || cred.CredentialBlob == nil || cred.CredentialBlobSize == 0 {
		return nil, fmt.Errorf("凭据内容为空")
	}
	out := make([]byte, cred.CredentialBlobSize)
	copy(out, unsafe.Slice(cred.CredentialBlob, cred.CredentialBlobSize))
	return out, nil
}
