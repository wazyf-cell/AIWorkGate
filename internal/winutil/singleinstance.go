//go:build windows

package winutil

import (
	"errors"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// procCreateMutexW is called through the raw syscall table because the
// generated x/sys wrapper drops the last-error value when the call succeeds,
// and ERROR_ALREADY_EXISTS is only reported through GetLastError.
var procCreateMutexW = windows.NewLazySystemDLL("kernel32.dll").NewProc("CreateMutexW")

// AcquireSingleInstance creates (or opens) the named mutex `name` and reports
// whether another process already holds it.
//
// alreadyRunning is true when the mutex already existed, i.e. a previous
// instance is alive; the caller should then activate the existing UI and exit.
// release closes this process' handle exactly once and is always non-nil when
// err is nil, including the alreadyRunning case.
//
// Use a session-local name (e.g. `Local\AIWorkGate`) so the check does not
// leak across fast-user-switching sessions.
func AcquireSingleInstance(name string) (release func(), alreadyRunning bool, err error) {
	if strings.TrimSpace(name) == "" {
		return nil, false, errors.New("winutil: single-instance mutex name is empty")
	}
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, false, err
	}

	handle, _, callErr := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(namePtr)))
	if handle == 0 {
		if callErr != nil && !errors.Is(callErr, windows.ERROR_SUCCESS) {
			return nil, false, callErr
		}
		return nil, false, errors.New("winutil: CreateMutexW returned a NULL handle")
	}

	alreadyRunning = errors.Is(callErr, windows.ERROR_ALREADY_EXISTS)

	h := windows.Handle(handle)
	var once sync.Once
	release = func() {
		once.Do(func() {
			_ = windows.CloseHandle(h)
		})
	}
	return release, alreadyRunning, nil
}
