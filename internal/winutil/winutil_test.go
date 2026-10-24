//go:build windows

package winutil

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"testing"
	"time"
)

// uniqueSuffix keeps registry values and mutex names from colliding with real
// entries or with a parallel test run.
func uniqueSuffix(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func TestAcquireSingleInstance(t *testing.T) {
	name := `Local\AIWorkGate-test-` + uniqueSuffix(t)

	release1, already1, err := AcquireSingleInstance(name)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if release1 == nil {
		t.Fatal("first acquire returned a nil release")
	}
	if already1 {
		t.Fatal("first acquire reported alreadyRunning=true")
	}

	release2, already2, err := AcquireSingleInstance(name)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if release2 == nil {
		t.Fatal("second acquire returned a nil release")
	}
	if !already2 {
		t.Fatal("second acquire in the same process must report alreadyRunning=true")
	}
	release2()
	release2() // release must be safe to call more than once

	// The first handle still owns the mutex, so a third acquire sees it too.
	release3, already3, err := AcquireSingleInstance(name)
	if err != nil {
		t.Fatalf("third acquire: %v", err)
	}
	if !already3 {
		t.Fatal("third acquire must report alreadyRunning=true while the first handle is open")
	}
	release3()

	release1()
	release1()

	release4, already4, err := AcquireSingleInstance(name)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if already4 {
		t.Fatal("mutex must be gone once every handle was closed")
	}
	release4()

	if _, _, err := AcquireSingleInstance("   "); err == nil {
		t.Fatal("empty mutex name must return an error")
	}
}

func TestAutostartLifecycle(t *testing.T) {
	name := "AIWorkGateTest-" + uniqueSuffix(t)
	exe := `C:\Program Files\AIWorkGate\aiworkgate.exe`

	t.Cleanup(func() { _ = RemoveAutostart(name) })

	if path, found, err := AutostartPath(name); err != nil {
		t.Fatalf("read before set: %v", err)
	} else if found {
		t.Fatalf("unexpected pre-existing autostart %q", path)
	}

	if err := SetAutostart(name, exe); err != nil {
		t.Fatalf("SetAutostart: %v", err)
	}

	path, found, err := AutostartPath(name)
	if err != nil {
		t.Fatalf("AutostartPath: %v", err)
	}
	if !found {
		t.Fatal("AutostartPath did not find the entry just written")
	}
	if path != exe {
		t.Fatalf("AutostartPath = %q, want %q", path, exe)
	}

	entries, err := ListRunEntries()
	if err != nil {
		t.Fatalf("ListRunEntries: %v", err)
	}
	raw, ok := entries[name]
	if !ok {
		t.Fatalf("ListRunEntries does not contain %q", name)
	}
	if want := `"` + exe + `"`; raw != want {
		t.Fatalf("stored Run value = %q, want quoted %q", raw, want)
	}

	// Updating replaces the value instead of appending a second entry.
	exe2 := `C:\Tools\AIWorkGate\aiworkgate.exe`
	if err := SetAutostart(name, exe2); err != nil {
		t.Fatalf("SetAutostart update: %v", err)
	}
	if path, found, err := AutostartPath(name); err != nil {
		t.Fatalf("AutostartPath after update: %v", err)
	} else if !found || path != exe2 {
		t.Fatalf("after update AutostartPath = %q (found=%v), want %q", path, found, exe2)
	}

	// A caller-supplied quoted path is rejected instead of being stored as-is:
	// quotes could otherwise append extra command-line arguments.
	if err := SetAutostart(name, `"`+exe+`" --evil`); err == nil {
		t.Fatal("SetAutostart accepted a quoted path with arguments")
	}

	if err := RemoveAutostart(name); err != nil {
		t.Fatalf("RemoveAutostart: %v", err)
	}
	if err := RemoveAutostart(name); err != nil {
		t.Fatalf("RemoveAutostart must be idempotent: %v", err)
	}

	if path, found, err := AutostartPath(name); err != nil {
		t.Fatalf("read after remove: %v", err)
	} else if found || path != "" {
		t.Fatalf("entry still present after remove: %q (found=%v)", path, found)
	}

	if entries, err := ListRunEntries(); err != nil {
		t.Fatalf("ListRunEntries after remove: %v", err)
	} else if _, ok := entries[name]; ok {
		t.Fatal("ListRunEntries still contains the removed entry")
	}
}

func TestParseRunCommand(t *testing.T) {
	tests := []struct {
		raw  string
		exe  string
		args []string
	}{
		{`"C:\Program Files\A\A.exe"`, `C:\Program Files\A\A.exe`, nil},
		{`"C:\Program Files\A\A.exe" --autostart -v`, `C:\Program Files\A\A.exe`, []string{"--autostart", "-v"}},
		{`  C:\A\A.exe  `, `C:\A\A.exe`, nil},
		{`C:\A\A.exe -x`, `C:\A\A.exe`, []string{"-x"}},
		{`"C:\A\A.exe`, `C:\A\A.exe`, nil},
		{``, ``, nil},
		{`   `, ``, nil},
	}
	for _, tc := range tests {
		exe, args := parseRunCommand(tc.raw)
		if exe != tc.exe {
			t.Errorf("parseRunCommand(%q) exe = %q, want %q", tc.raw, exe, tc.exe)
		}
		if len(args) != len(tc.args) {
			t.Errorf("parseRunCommand(%q) args = %v, want %v", tc.raw, args, tc.args)
			continue
		}
		for i := range args {
			if args[i] != tc.args[i] {
				t.Errorf("parseRunCommand(%q) args = %v, want %v", tc.raw, args, tc.args)
				break
			}
		}
	}
}

func TestValidateURL(t *testing.T) {
	valid := []string{
		"http://127.0.0.1:7865/",
		"https://example.com/path?a=b&c=d#frag",
		"  https://example.com/x  ",
	}
	for _, in := range valid {
		got, err := ValidateURL(in)
		if err != nil {
			t.Errorf("ValidateURL(%q) error: %v", in, err)
			continue
		}
		if got == "" {
			t.Errorf("ValidateURL(%q) returned an empty URL", in)
		}
	}

	invalid := []string{
		"",
		"   ",
		"example.com",
		"http://",
		"ftp://example.com/x",
		"file:///C:/Windows/System32/calc.exe",
		"javascript:alert(1)",
		"https://exa mple.com/",
		`https://example.com/"quoted"`,
	}
	for _, in := range invalid {
		if _, err := ValidateURL(in); err == nil {
			t.Errorf("ValidateURL(%q) accepted a non-http(s)/malformed URL", in)
		}
	}
}

func TestOpenBrowserRejectsNonHTTP(t *testing.T) {
	// Validation must run before anything is launched, so no browser opens here.
	for _, in := range []string{"file:///C:/Windows/System32/calc.exe", "javascript:alert(1)", "", "not a url"} {
		if err := OpenBrowser(in); err == nil {
			t.Errorf("OpenBrowser(%q) accepted an unsupported URL", in)
		}
	}
}
