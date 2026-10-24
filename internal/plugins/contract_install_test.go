package plugins

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"aiworkgate/internal/model"
)

// TestContractTemplateZipInstalls pins docs/插件生成契约.md: an archive laid out
// exactly as the packer produces it (manifest.json and the entry binary at the
// zip root) must install unchanged. The manifest is read from the real template
// so the document, the template and the installer cannot drift apart.
func TestContractTemplateZipInstalls(t *testing.T) {
	manifestPath := filepath.Join("..", "..", "examples", "plugin-template", "manifest.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skip("plugin template is not present in this checkout")
		}
		t.Fatalf("read template manifest: %v", err)
	}

	var m model.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("template manifest is not valid JSON: %v", err)
	}
	if err := ValidateManifest(m); err != nil {
		t.Fatalf("template manifest fails the installer's validation: %v", err)
	}
	if m.ID != "hello" {
		t.Fatalf("template id = %q, want hello (the contract example uses hello)", m.ID)
	}

	zipPath := makeZip(t, []zipEntry{
		{name: "manifest.json", body: string(raw)},
		{name: m.Entry.Exe, body: string(dummyExe), mode: 0o755},
	})

	mgr := newTestManager(t)
	if err := mgr.InstallZip(zipPath); err != nil {
		t.Fatalf("contract-shaped archive was rejected: %v", err)
	}
	if _, ok := mgr.Get(m.ID); !ok {
		t.Fatal("plugin is not installed")
	}
	dir := mgr.installDir(m.ID)
	if !isRegularFile(filepath.Join(dir, "manifest.json")) {
		t.Error("manifest.json was not copied to the install root")
	}
	if !isRegularFile(filepath.Join(dir, filepath.FromSlash(m.Entry.Exe))) {
		t.Errorf("entry %q was not copied to the install root", m.Entry.Exe)
	}
}
