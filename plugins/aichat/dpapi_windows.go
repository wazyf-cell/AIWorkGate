//go:build windows

package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

// DPAPI (CryptProtectData / CryptUnprotectData) seals the credential file to
// the current Windows user, matching what the host already does for its own
// secrets.dat. The plugin only ever protects and unprotects its own blob; it
// never reads another application's vault.
//
// CRYPTPROTECT_UI_FORBIDDEN keeps a failing call from popping a UI prompt in a
// console child process that has no window.
const cryptProtectUIForbidden = 0x1

type dataBlob struct {
	size uint32
	data *byte
}

var (
	crypt32DLL           = syscall.NewLazyDLL("crypt32.dll")
	kernel32DLL          = syscall.NewLazyDLL("kernel32.dll")
	procCryptProtectData = crypt32DLL.NewProc("CryptProtectData")
	procCryptUnprotect   = crypt32DLL.NewProc("CryptUnprotectData")
	procLocalFree        = kernel32DLL.NewProc("LocalFree")
)

func sealKind() string { return "dpapi-user" }

// seal encrypts plain for the current user.
func seal(plain []byte) ([]byte, error) {
	in := dataBlob{size: uint32(len(plain))}
	if len(plain) > 0 {
		in.data = &plain[0]
	}
	var out dataBlob
	r1, _, err := procCryptProtectData.Call(
		uintptr(unsafe.Pointer(&in)), 0, 0, 0, 0,
		uintptr(cryptProtectUIForbidden), uintptr(unsafe.Pointer(&out)))
	if r1 == 0 {
		if err == nil || err == syscall.Errno(0) {
			err = syscall.GetLastError()
		}
		return nil, fmt.Errorf("CryptProtectData: %w", err)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.data)))
	blob := make([]byte, out.size)
	if out.size > 0 {
		copy(blob, unsafe.Slice(out.data, out.size))
	}
	return blob, nil
}

// open decrypts a blob produced by seal on this user's account.
func open(blob []byte) ([]byte, error) {
	in := dataBlob{size: uint32(len(blob))}
	if len(blob) > 0 {
		in.data = &blob[0]
	}
	var out dataBlob
	r1, _, err := procCryptUnprotect.Call(
		uintptr(unsafe.Pointer(&in)), 0, 0, 0, 0,
		uintptr(cryptProtectUIForbidden), uintptr(unsafe.Pointer(&out)))
	if r1 == 0 {
		if err == nil || err == syscall.Errno(0) {
			err = syscall.GetLastError()
		}
		return nil, fmt.Errorf("CryptUnprotectData: %w", err)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.data)))
	plain := make([]byte, out.size)
	if out.size > 0 {
		copy(plain, unsafe.Slice(out.data, out.size))
	}
	return plain, nil
}
