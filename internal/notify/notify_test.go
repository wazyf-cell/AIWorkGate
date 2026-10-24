package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func testEvent() Event {
	return Event{
		Title: "签到完成",
		Body:  "插件 x 签到完成：成功 2，失败 0",
		Kind:  KindCheckinDone,
		At:    time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC),
	}
}

// TestGenericRequestShape checks the generic JSON contract.
func TestGenericRequestShape(t *testing.T) {
	var gotPath, gotMethod, gotType string
	var got Event
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod, gotType = r.URL.Path, r.Method, r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sender, err := NewSender(ChannelGeneric, server.URL+"/hook")
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	if err := sender.Send(context.Background(), testEvent()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotPath != "/hook" || gotMethod != http.MethodPost {
		t.Fatalf("request = %s %s, want POST /hook", gotMethod, gotPath)
	}
	if !strings.HasPrefix(gotType, "application/json") {
		t.Fatalf("content-type = %q", gotType)
	}
	if got.Title != "签到完成" || got.Kind != KindCheckinDone || !got.At.Equal(testEvent().At) {
		t.Fatalf("payload = %+v", got)
	}
}

// TestBarkPathShape checks <base>/<title>/<body>.
func TestBarkPathShape(t *testing.T) {
	var gotPath, gotMethod string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
	}))
	defer server.Close()

	sender, err := NewSender(ChannelBark, server.URL+"/device-key")
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	if err := sender.Send(context.Background(), testEvent()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	want := "/device-key/签到完成/插件 x 签到完成：成功 2，失败 0"
	if gotPath != want || gotMethod != http.MethodPost {
		t.Fatalf("request = %s %s, want POST %s", gotMethod, gotPath, want)
	}
}

// TestBarkQueryShape checks the query form used when the base ends with "/".
func TestBarkQueryShape(t *testing.T) {
	var gotPath, title, body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		title = r.URL.Query().Get("title")
		body = r.URL.Query().Get("body")
	}))
	defer server.Close()

	sender, err := NewSender(ChannelBark, server.URL+"/")
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	if err := sender.Send(context.Background(), testEvent()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotPath != "/" || title != "签到完成" || body != testEvent().Body {
		t.Fatalf("request path=%q title=%q body=%q", gotPath, title, body)
	}
}

// TestServerChanShape checks the title/desp form and markdown line breaks.
func TestServerChanShape(t *testing.T) {
	var form url.Values
	var gotType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotType = r.Header.Get("Content-Type")
		_ = r.ParseForm()
		form = r.PostForm
	}))
	defer server.Close()

	sender, err := NewSender(ChannelServerChan, server.URL+"/SCT123.send")
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	event := testEvent()
	event.Body = "第一行\n第二行"
	if err := sender.Send(context.Background(), event); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.HasPrefix(gotType, "application/x-www-form-urlencoded") {
		t.Fatalf("content-type = %q", gotType)
	}
	if form.Get("title") != event.Title {
		t.Fatalf("title = %q", form.Get("title"))
	}
	if form.Get("desp") != "第一行  \n第二行" {
		t.Fatalf("desp = %q", form.Get("desp"))
	}
}

// TestWeComShape checks the group-robot text payload.
func TestWeComShape(t *testing.T) {
	var payload struct {
		MsgType string            `json:"msgtype"`
		Text    map[string]string `json:"text"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode body: %v", err)
		}
	}))
	defer server.Close()

	sender, err := NewSender(ChannelWeCom, server.URL+"/cgi-bin/webhook/send?key=SECRET")
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	if err := sender.Send(context.Background(), testEvent()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if payload.MsgType != "text" {
		t.Fatalf("msgtype = %q", payload.MsgType)
	}
	if payload.Text["content"] != "签到完成\n"+testEvent().Body {
		t.Fatalf("content = %q", payload.Text["content"])
	}
}

// TestRedactURL masks the path, device key and query token.
func TestRedactURL(t *testing.T) {
	cases := map[string]string{
		"https://api.day.app/deviceKey123":              "https://api.day.app/***",
		"https://sctapi.ftqq.com/SCT123.send":           "https://sctapi.ftqq.com/***",
		"https://qyapi.weixin.qq.com/hook/send?key=abc": "https://qyapi.weixin.qq.com/***",
	}
	for raw, want := range cases {
		if got := RedactURL(raw); got != want {
			t.Fatalf("RedactURL(%q) = %q, want %q", raw, got, want)
		}
	}
	if got := RedactURL("not a url"); strings.Contains(got, "not a url") {
		t.Fatalf("invalid URL echoed: %q", got)
	}
}

// TestErrorsNeverContainEndpoint covers both a non-2xx answer and a transport
// failure; neither may echo the webhook path or token.
func TestErrorsNeverContainEndpoint(t *testing.T) {
	const secret = "SCT-SUPER-SECRET"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad token "+secret, http.StatusInternalServerError)
	}))
	endpoint := server.URL + "/" + secret + ".send?token=" + secret

	sender, err := NewSender(ChannelServerChan, endpoint)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	sendErr := sender.Send(context.Background(), testEvent())
	if sendErr == nil {
		t.Fatal("Send() accepted HTTP 500")
	}
	if strings.Contains(sendErr.Error(), secret) || strings.Contains(sendErr.Error(), "/") {
		t.Fatalf("error leaked the endpoint: %v", sendErr)
	}
	if !strings.Contains(sendErr.Error(), "HTTP 500") {
		t.Fatalf("error should name the status: %v", sendErr)
	}
	server.Close()

	// Transport failure: the *url.Error text embeds the URL, which must be
	// unwrapped and masked.
	dead, err := NewSender(ChannelBark, "http://127.0.0.1:1/"+secret)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	deadErr := dead.Send(context.Background(), testEvent())
	if deadErr == nil {
		t.Fatal("Send() to a closed port returned nil")
	}
	if strings.Contains(deadErr.Error(), secret) {
		t.Fatalf("transport error leaked the device key: %v", deadErr)
	}
}

// TestNewSenderValidation rejects a missing or non-http endpoint without
// echoing it.
func TestNewSenderValidation(t *testing.T) {
	if _, err := NewSender(ChannelGeneric, "   "); err == nil {
		t.Fatal("empty endpoint accepted")
	}
	_, err := NewSender(ChannelGeneric, "ftp://example.test/SECRET")
	if err == nil {
		t.Fatal("ftp endpoint accepted")
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("validation error leaked the endpoint: %v", err)
	}
}

// waitFor polls until cond is true or the test times out.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

// TestDispatcherPolicy checks the master switch, the subscription filter, the
// throttle and the once-per-day checkin_done dedupe.
func TestDispatcherPolicy(t *testing.T) {
	var mu sync.Mutex
	var received []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var event Event
		_ = json.Unmarshal(body, &event)
		mu.Lock()
		received = append(received, event.Kind)
		mu.Unlock()
	}))
	defer server.Close()

	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(received)
	}

	dispatcher := NewDispatcher(DispatcherOptions{
		Endpoint: func() string { return server.URL + "/hook" },
		Settings: Settings{
			Enabled:     false,
			Channel:     ChannelGeneric,
			Events:      []string{KindCheckinFail, KindCheckinDone},
			MinInterval: time.Hour,
		},
	})
	dispatcher.Start()
	defer dispatcher.Stop()

	dispatcher.Emit("p1", testEvent())
	time.Sleep(50 * time.Millisecond)
	if count() != 0 {
		t.Fatalf("disabled dispatcher delivered %d events", count())
	}

	dispatcher.Update(Settings{
		Enabled:     true,
		Channel:     ChannelGeneric,
		Events:      []string{KindCheckinFail, KindCheckinDone},
		MinInterval: time.Hour,
	})
	dispatcher.Emit("p1", testEvent())
	waitFor(t, func() bool { return count() == 1 })

	// Unsubscribed kind is dropped.
	dispatcher.Emit("p1", Event{Kind: KindQuotaLow, Title: "t", Body: "b"})
	// Same kind+plugin inside the throttle window is dropped.
	dispatcher.Emit("p1", Event{Kind: KindCheckinDone, Title: "again", Body: "b"})
	// A different plugin has its own bucket.
	dispatcher.Emit("p2", testEvent())
	waitFor(t, func() bool { return count() == 2 })

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 || received[0] != KindCheckinDone || received[1] != KindCheckinDone {
		t.Fatalf("received = %v", received)
	}
}

// TestDispatcherRecordsState verifies the last-sent stamp and redacted error.
func TestDispatcherRecordsState(t *testing.T) {
	var fail bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail {
			http.Error(w, "nope", http.StatusBadGateway)
		}
	}))
	defer server.Close()

	dispatcher := NewDispatcher(DispatcherOptions{
		Endpoint: func() string { return server.URL + "/SECRET-KEY" },
		Settings: Settings{Enabled: true, Channel: ChannelGeneric, Events: []string{KindQuotaLow}, MinInterval: time.Millisecond},
	})
	dispatcher.Start()
	defer dispatcher.Stop()

	dispatcher.Emit("", Event{Kind: KindQuotaLow, Title: "额度不足", Body: "剩余 1"})
	waitFor(t, func() bool {
		sent, _ := dispatcher.State()
		return !sent.IsZero()
	})

	fail = true
	dispatcher.Emit("", Event{Kind: KindQuotaLow, Title: "额度不足", Body: "剩余 0"})
	waitFor(t, func() bool {
		_, lastErr := dispatcher.State()
		return lastErr != ""
	})
	_, lastErr := dispatcher.State()
	if strings.Contains(lastErr, "SECRET-KEY") {
		t.Fatalf("last error leaked the endpoint: %q", lastErr)
	}
}

// TestDispatcherTestBypassesSwitch proves the admin test button works with the
// master switch off.
func TestDispatcherTestBypassesSwitch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer server.Close()

	dispatcher := NewDispatcher(DispatcherOptions{
		Endpoint: func() string { return server.URL + "/hook" },
		Settings: Settings{Enabled: false, Channel: ChannelGeneric},
	})
	if err := dispatcher.Test(context.Background()); err != nil {
		t.Fatalf("Test: %v", err)
	}
	sent, lastErr := dispatcher.State()
	if sent.IsZero() || lastErr != "" {
		t.Fatalf("state = %v, %q", sent, lastErr)
	}

	// Without a URL the explicit test still succeeds: the desktop toast is the
	// delivery (see toast_test.go for the toast assertions).
	empty := NewDispatcher(DispatcherOptions{Settings: Settings{Channel: ChannelGeneric}})
	if err := empty.Test(context.Background()); err != nil {
		t.Fatalf("Test without endpoint = %v", err)
	}
}

// TestDispatcherQueueOverflowNeverBlocks fills the queue and proves Emit
// returns immediately instead of applying back-pressure to a check-in run.
func TestDispatcherQueueOverflowNeverBlocks(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	defer server.Close()

	dispatcher := NewDispatcher(DispatcherOptions{
		Endpoint:  func() string { return server.URL + "/hook" },
		Settings:  Settings{Enabled: true, Channel: ChannelGeneric, Events: []string{KindQuotaLow}, MinInterval: time.Millisecond},
		QueueSize: 1,
	})
	dispatcher.Start()
	defer func() {
		close(release)
		dispatcher.Stop()
	}()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 5; i++ {
			dispatcher.Emit(fmt.Sprintf("p%d", i), Event{Kind: KindQuotaLow, Title: "t", Body: "b"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Emit blocked on a full queue")
	}
}
