package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

func loadState(dir string) (*State, error) {
	path := filepath.Join(dir, stateFileName)
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &State{Version: 1}, nil
		}
		return nil, fmt.Errorf("read state: %w", err)
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if st.Version == 0 {
		st.Version = 1
	}
	return &st, nil
}

func loadAccounts(dir string) ([]*Account, error) {
	path := filepath.Join(dir, accountsFileName)
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []*Account{}, nil
		}
		return nil, fmt.Errorf("read accounts: %w", err)
	}
	plain, err := dpapiUnprotect(b)
	if err != nil {
		return nil, fmt.Errorf("decrypt %s: %w", path, err)
	}
	var f accountsFile
	if err := json.Unmarshal(plain, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if f.Accounts == nil {
		f.Accounts = []*Account{}
	}
	for _, a := range f.Accounts {
		if a.Identity.SessionID == "" {
			a.Identity = newIdentity(0)
		}
		if a.Identity.ChatID == "" {
			a.Identity.ChatID = randomHex(16)
		}
		if a.Identity.Source == "" {
			a.Identity.Source = "derived"
		}
		if a.Quota.TodayCheckedIn && !sameDay(a.Quota.CheckedInAt, time.Now()) {
			a.Quota.TodayCheckedIn = false
			a.Quota.CheckedInAt = 0
		}
	}
	return f.Accounts, nil
}

// writeFileAtomic writes data through a temp file in the same directory and
// renames it into place.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func saveAccounts(dir string, accounts []*Account) error {
	plain, err := json.MarshalIndent(accountsFile{Version: 1, Accounts: accounts}, "", "  ")
	if err != nil {
		return err
	}
	enc, err := dpapiProtect(plain)
	if err != nil {
		return fmt.Errorf("encrypt accounts: %w", err)
	}
	return writeFileAtomic(filepath.Join(dir, accountsFileName), enc)
}

func saveState(dir string, st *State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, stateFileName), b)
}
