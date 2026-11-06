//go:build windows

package main

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

// DPAPI (CryptProtectData / CryptUnprotectData) with user scope. The plugin
// only ever stores the encrypted blob; tokens and secret keys must never be
// written in plaintext.
var (
	crypt32DLL             = syscall.NewLazyDLL("crypt32.dll")
	kernel32DLL            = syscall.NewLazyDLL("kernel32.dll")
	procCryptProtectData   = crypt32DLL.NewProc("CryptProtectData")
	procCryptUnprotectData = crypt32DLL.NewProc("CryptUnprotectData")
	procLocalFree          = kernel32DLL.NewProc("LocalFree")
)

const cryptProtectUIForbidden = 0x1

type dataBlob struct {
	cbData uint32
	pbData *byte
	keep   []byte // pins Go memory used as the input buffer
	owned  bool   // pbData was allocated by DPAPI and must be LocalFree'd
}

func newBlob(data []byte) (*dataBlob, error) {
	if len(data) == 0 {
		return &dataBlob{}, nil
	}
	return &dataBlob{cbData: uint32(len(data)), pbData: &data[0], keep: data}, nil
}

func (b *dataBlob) bytes() []byte {
	if b == nil || b.pbData == nil || b.cbData == 0 {
		return nil
	}
	return unsafe.Slice(b.pbData, b.cbData)
}

func (b *dataBlob) free() {
	if b != nil && b.owned && b.pbData != nil {
		_, _, _ = procLocalFree.Call(uintptr(unsafe.Pointer(b.pbData)))
		b.pbData = nil
		b.cbData = 0
		b.owned = false
	}
}

func dpapiProtect(plain []byte) ([]byte, error) {
	in, err := newBlob(plain)
	if err != nil {
		return nil, err
	}
	desc, _ := syscall.UTF16PtrFromString("AIWorkGate CodeArts credentials")
	var out dataBlob
	ok, _, callErr := procCryptProtectData.Call(
		uintptr(unsafe.Pointer(in)),
		uintptr(unsafe.Pointer(desc)),
		0, 0, 0,
		cryptProtectUIForbidden,
		uintptr(unsafe.Pointer(&out)),
	)
	runtime.KeepAlive(in)
	runtime.KeepAlive(desc)
	if ok == 0 {
		if callErr != syscall.Errno(0) {
			return nil, fmt.Errorf("CryptProtectData: %w", callErr)
		}
		return nil, errors.New("CryptProtectData failed")
	}
	out.owned = true
	defer out.free()
	return append([]byte(nil), out.bytes()...), nil
}

func dpapiUnprotect(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) == 0 {
		return nil, errors.New("empty DPAPI blob")
	}
	in, err := newBlob(ciphertext)
	if err != nil {
		return nil, err
	}
	var out dataBlob
	var desc *uint16
	ok, _, callErr := procCryptUnprotectData.Call(
		uintptr(unsafe.Pointer(in)),
		uintptr(unsafe.Pointer(&desc)),
		0, 0, 0,
		cryptProtectUIForbidden,
		uintptr(unsafe.Pointer(&out)),
	)
	runtime.KeepAlive(in)
	if ok == 0 {
		if callErr != syscall.Errno(0) {
			return nil, fmt.Errorf("CryptUnprotectData: %w", callErr)
		}
		return nil, errors.New("CryptUnprotectData failed")
	}
	out.owned = true
	defer out.free()
	if desc != nil {
		_, _, _ = procLocalFree.Call(uintptr(unsafe.Pointer(desc)))
	}
	return append([]byte(nil), out.bytes()...), nil
}
