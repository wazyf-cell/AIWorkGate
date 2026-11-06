package presets

import _ "embed"

// embedded is the preset catalog shipped with the binary. Provider definitions
// live in data, not in the host config (D-27②: the config only carries the
// choices that matter at startup — the mirror, the sandbox user, …).
//
//go:embed presets.json
var embedded []byte
