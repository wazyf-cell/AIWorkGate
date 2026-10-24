package model

import "testing"

// A (the AI Work 助手) publishes its catalog with its own spellings:
// display / rate / supports_image / supported_efforts, and no vendor. Decoding
// straight into ModelInfo's canonical tags dropped every one of those, which is
// why the catalog page showed vendor/multiplier/vision/thinking empty while
// context_length (the one name that happened to match) came through.
func TestParseModelListAcceptsAUpstreamSpellings(t *testing.T) {
	body := []byte(`{"data":[
		{"id":"glm-5.2","display":"GLM-5.2","rate":0.78,
		 "context_length":200000,"supports_image":true,
		 "supported_efforts":["low","medium","high","xhigh","max"]},
		{"id":"deepseek-v4.1-flash","display":"DeepSeek-V4.1-Flash","rate":0.06,
		 "context_length":200000,"supports_image":true,"efforts":[]}
	]}`)

	got, err := ParseModelList(body)
	if err != nil {
		t.Fatalf("ParseModelList: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("entries = %d, want 2", len(got))
	}

	glm := got[0]
	if glm.Name != "GLM-5.2" {
		t.Fatalf("name = %q, want display to be used", glm.Name)
	}
	if glm.Multiplier != 0.78 {
		t.Fatalf("multiplier = %v, want rate mapped", glm.Multiplier)
	}
	if !glm.Vision {
		t.Fatal("vision = false, want supports_image mapped")
	}
	if glm.ContextLength != 200000 {
		t.Fatalf("context = %d, want 200000", glm.ContextLength)
	}
	if len(glm.Thinking) != 5 {
		t.Fatalf("thinking = %v, want the five supported_efforts levels", glm.Thinking)
	}

	// An explicitly empty tier list stays empty: the model reports no levels, so
	// the UI must show "not published" rather than an invented set.
	if len(got[1].Thinking) != 0 {
		t.Fatalf("thinking = %v, want empty for an empty efforts array", got[1].Thinking)
	}
	// A publishes no vendor at all, which must not leak a placeholder.
	if got[1].Vendor != "" {
		t.Fatalf("vendor = %q, want empty", got[1].Vendor)
	}
}

// The canonical spellings plugins use must keep working, and a model that reports
// both spellings must not lose its levels.
func TestParseModelListKeepsCanonicalSpellings(t *testing.T) {
	body := []byte(`{"models":[
		{"id":"m1","name":"Canonical","multiplier":1.5,"vendor":"ACME",
		 "thinking":["low","high"],"supports_image":true,"free":true}
	]}`)
	got, err := ParseModelList(body)
	if err != nil {
		t.Fatalf("ParseModelList: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("entries = %d, want 1", len(got))
	}
	m := got[0]
	if m.Name != "Canonical" || m.Multiplier != 1.5 || m.Vendor != "ACME" || !m.Vision || !m.Free {
		t.Fatalf("canonical fields lost: %+v", m)
	}
	if len(m.Thinking) != 2 {
		t.Fatalf("thinking = %v, want the canonical list", m.Thinking)
	}
}

// A bare array is a third accepted shape.
func TestParseModelListAcceptsBareArray(t *testing.T) {
	got, err := ParseModelList([]byte(`[{"id":"a"},{"id":"b"}]`))
	if err != nil {
		t.Fatalf("ParseModelList: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("entries = %d, want 2", len(got))
	}
}

// Non-JSON must be an error, not an empty list. Silently returning nothing would
// present a broken upstream as "no catalog metadata", hiding the outage.
func TestParseModelListRejectsNonJSON(t *testing.T) {
	if _, err := ParseModelList([]byte("not json")); err == nil {
		t.Fatal("ParseModelList accepted a non-JSON body; a decode failure must not look like an empty catalog")
	}
}

// Ids without a usable name fall back to the id, and empty ids are skipped.
func TestParseModelListSkipsEmptyIDs(t *testing.T) {
	got, err := ParseModelList([]byte(`{"data":[{"id":""},{"id":"  x  "}]}`))
	if err != nil {
		t.Fatalf("ParseModelList: %v", err)
	}
	if len(got) != 1 || got[0].ID != "x" {
		t.Fatalf("entries = %+v, want only the trimmed id", got)
	}
}
