package redact

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain gives the package's tests a home of their own, never the
// machine's (magpie 用 internal/testenv 做同级的事，这里用标准库最小实现)：
// HOME/USERPROFILE 与 XDG、Windows 的 APPDATA/LOCALAPPDATA 全部指向一个一次性
// 临时目录，代理环境变量清掉——测试不应当读到或写进运行它的用户的真实文件。
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "aigate-redact-test-home-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(home)
	vars := map[string]string{
		"HOME":            home,
		"USERPROFILE":     home,
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"),
		"XDG_CACHE_HOME":  filepath.Join(home, ".cache"),
		"XDG_DATA_HOME":   filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME":  filepath.Join(home, ".local", "state"),
		"APPDATA":         filepath.Join(home, "AppData", "Roaming"),
		"LOCALAPPDATA":    filepath.Join(home, "AppData", "Local"),
	}
	for k, v := range vars {
		_ = os.Setenv(k, v)
	}
	for _, k := range []string{
		"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy",
		"ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy",
	} {
		_ = os.Unsetenv(k)
	}
	os.Exit(m.Run())
}
