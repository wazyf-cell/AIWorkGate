//go:build !windows

package hosts

// ReadSystemProxy is a no-op outside Windows: the host itself targets Windows,
// and tests on other platforms only need the parser.
func ReadSystemProxy() (SystemProxy, error) {
	return SystemProxy{}, nil
}
