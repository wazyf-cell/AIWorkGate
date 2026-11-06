//go:build !windows

// Headless tray for non-Windows hosts: the admin UI is the only control
// surface, so Run returns immediately and Quit is a no-op. Keeping the same
// API means the host code compiles unchanged on Linux.

package tray

import "sync"

type Actions struct {
	OpenUI    func()
	PauseAll  func()
	ResumeAll func()
	Quit      func()
}

var (
	quitCh   = make(chan struct{})
	quitOnce sync.Once
)

type MenuLabels struct {
	OpenUI        string
	Pause         string
	Resume        string
	Quit          string
	PauseEnabled  bool
	ResumeEnabled bool
}

func Labels(paused bool) MenuLabels {
	if paused {
		return MenuLabels{OpenUI: "打开管理页", Pause: "全部插件已暂停", Resume: "恢复全部插件", Quit: "退出", ResumeEnabled: true}
	}
	return MenuLabels{OpenUI: "打开管理页", Pause: "暂停全部插件", Resume: "恢复全部插件", Quit: "退出", PauseEnabled: true}
}

// Run blocks like the Windows tray loop, so the host keeps serving. Quit
// (signal handler / shutdown) releases it.
func Run(string, string, Actions) { <-quitCh }

func SetPaused(bool) {}

func Quit() { quitOnce.Do(func() { close(quitCh) }) }

func IconBytes() []byte { return nil }
