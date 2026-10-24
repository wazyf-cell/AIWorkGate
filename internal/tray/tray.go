//go:build windows

// Package tray wraps github.com/getlantern/systray with the AIWorkGate tray
// menu: open the admin page, pause or resume every plugin, quit.
//
// systray owns an OS-thread message loop; Run blocks in it and Quit is the only
// way out. Menu construction lives in onReady so it can only run after systray
// has a window, and the labels themselves come from the pure Labels function
// so unit tests never need a message loop.
package tray

import (
	"sync"
	"sync/atomic"

	"github.com/getlantern/systray"
)

const (
	defaultTitle   = "AIWorkGate"
	defaultTooltip = "AIWorkGate 本地网关"
)

// Actions are the host callbacks behind the menu items. Nil callbacks are
// ignored. Each item has its own goroutine, so a slow callback only delays
// further clicks on that same item.
type Actions struct {
	OpenUI    func()
	PauseAll  func()
	ResumeAll func()
	Quit      func()
}

type app struct {
	actions Actions

	mu         sync.Mutex
	paused     bool
	pauseItem  *systray.MenuItem
	resumeItem *systray.MenuItem
}

var (
	current  atomic.Pointer[app]
	running  atomic.Bool
	quitOnce sync.Once
)

// Run shows the tray icon and blocks until Quit is called, either from the
// menu or by the host. Empty title/tooltip fall back to defaults.
//
// Call it once, from the process main goroutine: systray binds to that thread
// and a second Run cannot be cleaned up afterwards. After Run returns the
// host's shutdown path continues.
func Run(title, tooltip string, a Actions) {
	if title == "" {
		title = defaultTitle
	}
	if tooltip == "" {
		tooltip = defaultTooltip
	}

	ap := &app{actions: a}
	current.Store(ap)

	running.Store(true)
	systray.Run(func() { ap.onReady(title, tooltip) }, nil)
	running.Store(false)
	current.CompareAndSwap(ap, nil)
}

// SetPaused updates the menu so it reflects whether every plugin is paused.
// Safe to call from any goroutine and before Run (the state is applied when
// the menu appears); it never invokes Actions.
func SetPaused(paused bool) {
	if ap := current.Load(); ap != nil {
		ap.setPaused(paused)
	}
}

// Quit stops the tray and invokes Actions.Quit. It is idempotent and safe to
// call when the tray never started.
//
// Actions.Quit runs first and must only signal shutdown (close a channel, send
// on a context); waiting for Run to return deadlocks because Run is still
// pumping messages at that moment.
func Quit() {
	quitOnce.Do(func() {
		if ap := current.Load(); ap != nil {
			call(ap.actions.Quit)
		}
		if running.Load() {
			systray.Quit()
		}
	})
}

func (a *app) onReady(title, tooltip string) {
	systray.SetIcon(IconBytes())
	systray.SetTitle(title) // no-op on Windows, kept for parity
	systray.SetTooltip(tooltip)

	l := Labels(false)
	openItem := systray.AddMenuItem(l.OpenUI, "在默认浏览器中打开管理页")
	pauseItem := systray.AddMenuItem(l.Pause, "停止运行所有插件")
	resumeItem := systray.AddMenuItem(l.Resume, "重新启动已停用的插件")
	systray.AddSeparator()
	quitItem := systray.AddMenuItem(l.Quit, "退出 AIWorkGate")

	a.mu.Lock()
	a.pauseItem, a.resumeItem = pauseItem, resumeItem
	a.applyLabelsLocked(Labels(a.paused))
	a.mu.Unlock()

	go func() {
		for range openItem.ClickedCh {
			call(a.actions.OpenUI)
		}
	}()
	go func() {
		for range pauseItem.ClickedCh {
			a.setPaused(true)
			call(a.actions.PauseAll)
		}
	}()
	go func() {
		for range resumeItem.ClickedCh {
			a.setPaused(false)
			call(a.actions.ResumeAll)
		}
	}()
	go func() {
		for range quitItem.ClickedCh {
			Quit()
		}
	}()
}

// setPaused records the state and pushes it to the menu; it is also the place
// the host's SetPaused lands before the menu exists.
func (a *app) setPaused(paused bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.paused = paused
	a.applyLabelsLocked(Labels(paused))
}

// applyLabelsLocked must be called with a.mu held.
func (a *app) applyLabelsLocked(l MenuLabels) {
	if a.pauseItem == nil || a.resumeItem == nil {
		return
	}
	a.pauseItem.SetTitle(l.Pause)
	a.resumeItem.SetTitle(l.Resume)
	setEnabled(a.pauseItem, l.PauseEnabled)
	setEnabled(a.resumeItem, l.ResumeEnabled)
}

func setEnabled(item *systray.MenuItem, enabled bool) {
	if enabled {
		item.Enable()
		return
	}
	item.Disable()
}

func call(fn func()) {
	if fn != nil {
		fn()
	}
}
