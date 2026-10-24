package notify

import (
	"fmt"
	"strings"
	"sync"
)

// toastAppName brands the Windows toast. It is also the AppUserModelId the
// toast is grouped under, so it must stay stable between builds.
const toastAppName = "AIWorkGate"

// desktopNotify is the desktop-toast seam. Tests replace it so a policy test
// never puts a real notification on the user's screen.
var desktopNotify = sendDesktopToast

// toastMu serialises toast calls: the delivery worker and the explicit test
// action can both raise one, and the Windows toast backend is global state.
var toastMu sync.Mutex

// sendToast raises one desktop toast. It is only ever called from the delivery
// worker or from Test, never from Emit, so a slow toast backend cannot block a
// check-in run. The failure is returned instead of logged so the caller can
// record it; it never fails the webhook delivery.
func (d *Dispatcher) sendToast(event Event) error {
	title := strings.TrimSpace(event.Title)
	if title == "" {
		title = toastAppName
	}
	body := strings.TrimSpace(event.Body)

	toastMu.Lock()
	err := desktopNotify(title, body)
	toastMu.Unlock()
	if err != nil {
		return fmt.Errorf("notify: 桌面通知发送失败：%w", err)
	}
	return nil
}
