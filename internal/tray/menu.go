//go:build windows

package tray

// Menu titles. Kept as constants so tests can assert on them without starting
// systray.
const (
	TitleOpenUI    = "打开管理页"
	TitlePauseAll  = "暂停全部插件"
	TitlePausedAll = "全部插件已暂停"
	TitleResumeAll = "恢复全部插件"
	TitleQuit      = "退出"
)

// MenuLabels is the complete menu text and enabled state for one tray state.
type MenuLabels struct {
	OpenUI        string
	Pause         string
	Resume        string
	Quit          string
	PauseEnabled  bool
	ResumeEnabled bool
}

// Labels returns the menu layout for the given paused state. It is a pure
// function: the tray sets these titles on its menu items and the host can use
// it to render the same state elsewhere.
//
// When paused, the pause item becomes the status line "全部插件已暂停" and is
// disabled; the resume item is only clickable while paused.
func Labels(paused bool) MenuLabels {
	l := MenuLabels{
		OpenUI:        TitleOpenUI,
		Pause:         TitlePauseAll,
		Resume:        TitleResumeAll,
		Quit:          TitleQuit,
		PauseEnabled:  true,
		ResumeEnabled: false,
	}
	if paused {
		l.Pause = TitlePausedAll
		l.PauseEnabled = false
		l.ResumeEnabled = true
	}
	return l
}
