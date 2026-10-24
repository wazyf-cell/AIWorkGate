package app

import (
	"reflect"
	"testing"

	"aiworkgate/internal/model"
)

func TestLoginMethodsFromCapabilitiesIncludesIDEPlugin(t *testing.T) {
	mf := model.Manifest{Capabilities: []string{
		model.CapLoginPasteCode,
		model.CapLoginIDEPlugin,
		model.CapLoginOAuth,
	}}
	got := loginMethodsFromCapabilities(mf)
	want := []string{"oauth", "ide_plugin", "paste_code"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("methods = %v, want %v", got, want)
	}
}
