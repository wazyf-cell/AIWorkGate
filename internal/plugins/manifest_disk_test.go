package plugins

import (
	"os"
	"path/filepath"
	"testing"
)

// The existing manifest tests build their fixtures in memory, so a repository
// manifest could be malformed and no test would notice. This one validates the
// manifests that actually ship, which is what a user installs.
//
// It reads every plugins/<id>/manifest.json in the repository and runs the same
// validation the installer runs. A manifest that fails here would fail at
// install time on a user's machine, not in CI.
func TestRepositoryManifestsAreValid(t *testing.T) {
	root := filepath.Join("..", "..", "plugins")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skip("no plugins directory in this checkout")
		}
		t.Fatalf("read %s: %v", root, err)
	}

	checked := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(root, entry.Name(), "manifest.json")
		if _, statErr := os.Stat(path); statErr != nil {
			continue // a directory without a manifest is not a plugin
		}
		m, err := LoadManifestFile(path)
		if err != nil {
			t.Errorf("%s: load: %v", path, err)
			continue
		}
		if err := ValidateManifest(m); err != nil {
			t.Errorf("%s: validate: %v", path, err)
			continue
		}
		// The directory name is what the host uses as the plugin id, so a
		// mismatch silently installs the plugin under the wrong name.
		if m.ID != entry.Name() {
			t.Errorf("%s: manifest id %q differs from its directory %q",
				path, m.ID, entry.Name())
		}
		// The entry binary must be a safe relative path: an absolute path or a
		// parent traversal would let a manifest point outside its own directory.
		if filepath.IsAbs(m.Entry.Exe) || hasParentDir(m.Entry.Exe) {
			t.Errorf("%s: entry exe %q is not a safe relative path", path, m.Entry.Exe)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no plugin manifests were validated; the path is probably wrong")
	}
	t.Logf("validated %d repository manifest(s)", checked)
}

// hasParentDir reports whether p climbs out of its own directory. filepath.Split
// walks one separator at a time, which is what a cleaned relative path needs; a
// ".." segment anywhere in it is a traversal.
func hasParentDir(p string) bool {
	clean := filepath.Clean(p)
	if clean == ".." {
		return true
	}
	for {
		dir, last := filepath.Split(clean)
		if last == ".." {
			return true
		}
		if dir == "" {
			return false
		}
		next := filepath.Clean(dir)
		if next == clean || next == "." || next == string(filepath.Separator) {
			return false
		}
		clean = next
	}
}
