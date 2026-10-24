package notify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMain replaces the desktop toast with a no-op so running the policy tests
// never puts a real notification on the user's screen. The toast tests below
// install their own recording stub.
func TestMain(m *testing.M) {
	desktopNotify = func(string, string) error { return nil }
	os.Exit(m.Run())
}

type toastCall struct{ title, body string }

// toastSpy is a recording replacement for desktopNotify.
type toastSpy struct {
	mu    sync.Mutex
	calls []toastCall
	err   error
}

func (s *toastSpy) notify(title, body string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, toastCall{title: title, body: body})
	return s.err
}

func (s *toastSpy) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *toastSpy) last() (toastCall, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		return toastCall{}, false
	}
	return s.calls[len(s.calls)-1], true
}

// stubToast installs a recording toast function for one test.
func stubToast(t *testing.T) *toastSpy {
	t.Helper()
	spy := &toastSpy{}
	previous := desktopNotify
	desktopNotify = spy.notify
	t.Cleanup(func() { desktopNotify = previous })
	return spy
}

// TestToastDeliveredForEnabledSubscribedEvent covers the headline behaviour:
// notifications enabled, an allowed event, no webhook URL, one toast.
func TestToastDeliveredForEnabledSubscribedEvent(t *testing.T) {
	spy := stubToast(t)
	dispatcher := NewDispatcher(DispatcherOptions{
		Endpoint: func() string { return "" },
		Settings: Settings{
			Enabled:     true,
			Channel:     ChannelGeneric,
			Events:      []string{KindCheckinDone},
			MinInterval: time.Hour,
		},
	})
	dispatcher.Start()
	defer dispatcher.Stop()

	dispatcher.Emit("p1", testEvent())
	waitFor(t, func() bool { return spy.count() == 1 })

	call, _ := spy.last()
	if call.title != testEvent().Title || call.body != testEvent().Body {
		t.Fatalf("toast = %+v, want title/body from the event", call)
	}
	sent, lastErr := dispatcher.State()
	if sent.IsZero() || lastErr != "" {
		t.Fatalf("state = %v, %q; want a successful delivery", sent, lastErr)
	}
}

// TestToastSkippedWhenDisabledOrUnsubscribed proves the toast path keeps the
// existing master switch, subscription filter and throttle semantics.
func TestToastSkippedWhenDisabledOrUnsubscribed(t *testing.T) {
	spy := stubToast(t)
	dispatcher := NewDispatcher(DispatcherOptions{
		Endpoint: func() string { return "" },
		Settings: Settings{
			Enabled:     false,
			Channel:     ChannelGeneric,
			Events:      []string{KindCheckinFail},
			MinInterval: time.Hour,
		},
	})
	dispatcher.Start()
	defer dispatcher.Stop()

	dispatcher.Emit("p1", testEvent())
	time.Sleep(50 * time.Millisecond)
	if spy.count() != 0 {
		t.Fatalf("disabled dispatcher raised %d toasts", spy.count())
	}

	dispatcher.Update(Settings{
		Enabled:     true,
		Channel:     ChannelGeneric,
		Events:      []string{KindCheckinFail},
		MinInterval: time.Hour,
	})
	// Unsubscribed kind.
	dispatcher.Emit("p1", testEvent())
	time.Sleep(50 * time.Millisecond)
	if spy.count() != 0 {
		t.Fatalf("unsubscribed kind raised %d toasts", spy.count())
	}

	dispatcher.Emit("p1", Event{Kind: KindCheckinFail, Title: "失败", Body: "b"})
	waitFor(t, func() bool { return spy.count() == 1 })

	// Same (kind, plugin) inside the throttle window is dropped.
	dispatcher.Emit("p1", Event{Kind: KindCheckinFail, Title: "再次失败", Body: "b"})
	time.Sleep(50 * time.Millisecond)
	if spy.count() != 1 {
		t.Fatalf("throttled event raised a toast: %d calls", spy.count())
	}

	// A different plugin has its own throttle bucket.
	dispatcher.Emit("p2", Event{Kind: KindCheckinFail, Title: "失败", Body: "b"})
	waitFor(t, func() bool { return spy.count() == 2 })
}

// TestToastAndWebhookBothDelivered proves the two channels are not exclusive.
func TestToastAndWebhookBothDelivered(t *testing.T) {
	spy := stubToast(t)
	var mu sync.Mutex
	webhooks := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		mu.Lock()
		webhooks++
		mu.Unlock()
	}))
	defer server.Close()

	dispatcher := NewDispatcher(DispatcherOptions{
		Endpoint: func() string { return server.URL + "/hook" },
		Settings: Settings{
			Enabled:     true,
			Channel:     ChannelGeneric,
			Events:      []string{KindQuotaLow},
			MinInterval: time.Hour,
		},
	})
	dispatcher.Start()
	defer dispatcher.Stop()

	dispatcher.Emit("", Event{Kind: KindQuotaLow, Title: "额度不足", Body: "剩余 1"})
	waitFor(t, func() bool { return spy.count() == 1 })
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return webhooks == 1
	})
}

// TestToastFailureNeverFailsDispatch proves a broken toast backend is recorded
// and logged but neither stops the webhook nor surfaces as a dispatch failure.
func TestToastFailureNeverFailsDispatch(t *testing.T) {
	spy := stubToast(t)
	spy.err = errors.New("toast backend unavailable")

	offline := NewDispatcher(DispatcherOptions{
		Settings: Settings{
			Enabled:     true,
			Channel:     ChannelGeneric,
			Events:      []string{KindQuotaLow},
			MinInterval: time.Hour,
		},
	})
	offline.Start()
	defer offline.Stop()

	// Emit has no return value and must stay non-blocking even when the toast
	// fails; the failure is recorded in State.
	offline.Emit("", Event{Kind: KindQuotaLow, Title: "额度不足", Body: "剩余 1"})
	waitFor(t, func() bool {
		_, lastErr := offline.State()
		return lastErr != ""
	})
	_, lastErr := offline.State()
	if lastErr == "" || !strings.Contains(lastErr, "toast backend unavailable") {
		t.Fatalf("toast failure not recorded: %q", lastErr)
	}

	// With a working webhook the same toast failure still leaves the webhook
	// delivery successful.
	var webhooks int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		webhooks++
	}))
	defer server.Close()
	online := NewDispatcher(DispatcherOptions{
		Endpoint: func() string { return server.URL + "/hook" },
		Settings: Settings{
			Enabled:     true,
			Channel:     ChannelGeneric,
			Events:      []string{KindQuotaLow},
			MinInterval: time.Millisecond,
		},
	})
	online.Start()
	defer online.Stop()

	online.Emit("", Event{Kind: KindQuotaLow, Title: "额度不足", Body: "剩余 0"})
	waitFor(t, func() bool {
		sent, err := online.State()
		return !sent.IsZero() && err != ""
	})
	if webhooks != 1 {
		t.Fatalf("webhook deliveries = %d, want 1", webhooks)
	}
	if _, err := online.State(); !strings.Contains(err, "toast backend unavailable") {
		t.Fatalf("toast failure not recorded alongside the webhook: %q", err)
	}
}

// TestTestEventAlwaysToasts covers the Settings "send test" action: it must
// give feedback with no URL configured and still push when one is.
func TestTestEventAlwaysToasts(t *testing.T) {
	spy := stubToast(t)
	dispatcher := NewDispatcher(DispatcherOptions{
		Endpoint: func() string { return "" },
		Settings: Settings{Channel: ChannelGeneric},
	})
	if err := dispatcher.Test(context.Background()); err != nil {
		t.Fatalf("Test without endpoint = %v", err)
	}
	waitFor(t, func() bool { return spy.count() == 1 })
	call, _ := spy.last()
	if call.title != "AIWorkGate 测试通知" {
		t.Fatalf("toast title = %q", call.title)
	}
	sent, lastErr := dispatcher.State()
	if sent.IsZero() || lastErr != "" {
		t.Fatalf("state = %v, %q; want a successful test", sent, lastErr)
	}

	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits++
	}))
	defer server.Close()
	withURL := NewDispatcher(DispatcherOptions{
		Endpoint: func() string { return server.URL + "/hook" },
		Settings: Settings{Channel: ChannelGeneric},
	})
	if err := withURL.Test(context.Background()); err != nil {
		t.Fatalf("Test with endpoint = %v", err)
	}
	if spy.count() != 2 || hits != 1 {
		t.Fatalf("toasts = %d, webhook = %d; want both", spy.count(), hits)
	}

	// When the toast is the only channel and it fails, the test reports it.
	spy.err = errors.New("toast backend unavailable")
	if err := dispatcher.Test(context.Background()); err == nil || !strings.Contains(err.Error(), "桌面通知") {
		t.Fatalf("Test with failing toast = %v", err)
	}
}

// TestToastTitleFallback keeps the toast readable when an event has no title.
func TestToastTitleFallback(t *testing.T) {
	spy := stubToast(t)
	dispatcher := NewDispatcher(DispatcherOptions{
		Endpoint: func() string { return "" },
		Settings: Settings{
			Enabled:     true,
			Channel:     ChannelGeneric,
			Events:      []string{KindAccountDead},
			MinInterval: time.Hour,
		},
	})
	dispatcher.Start()
	defer dispatcher.Stop()

	dispatcher.Emit("", Event{Kind: KindAccountDead})
	waitFor(t, func() bool { return spy.count() == 1 })
	if call, _ := spy.last(); call.title != toastAppName {
		t.Fatalf("toast title = %q, want %q", call.title, toastAppName)
	}
}
