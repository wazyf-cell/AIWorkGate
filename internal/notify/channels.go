package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// channelLabels are the Chinese names used in error text. The channel id is
// never used because it says nothing to the user.
var channelLabels = map[string]string{
	ChannelGeneric:    "通用 Webhook",
	ChannelBark:       "Bark",
	ChannelServerChan: "Server酱",
	ChannelWeCom:      "企业微信",
}

// NewSender validates endpoint and returns the sender for channel.
//
// Endpoint must be an absolute http(s) URL. http is accepted so the admin
// "test" action can target a self-hosted endpoint (and so unit tests can use
// httptest); the settings page still recommends https for public services.
func NewSender(channel, endpoint string) (Sender, error) {
	channel = NormalizeChannel(channel)
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, errors.New("notify: 通知地址未配置")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		// The endpoint itself is a credential; it must not be echoed.
		return nil, errors.New("notify: 通知地址必须是完整的 http(s) URL")
	}
	client := &http.Client{Timeout: Timeout}
	switch channel {
	case ChannelBark:
		return &barkSender{
			channel:  channel,
			endpoint: endpoint,
			base:     strings.TrimRight(endpoint, "/"),
			query:    strings.HasSuffix(endpoint, "/"),
			client:   client,
		}, nil
	case ChannelServerChan:
		return &serverChanSender{channel: channel, endpoint: endpoint, client: client}, nil
	case ChannelWeCom:
		return &weComSender{channel: channel, endpoint: endpoint, client: client}, nil
	default:
		return &genericSender{channel: ChannelGeneric, endpoint: endpoint, client: client}, nil
	}
}

// genericSender POSTs {"title","body","kind","at"} as JSON. It is the shape
// used by the reference product's generic webhook channel
// (src-tauri/src/notify.rs:57-70).
type genericSender struct {
	channel  string
	endpoint string
	client   *http.Client
}

func (s *genericSender) Send(ctx context.Context, event Event) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("notify: 编码通知内容失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return redactedRequestError(s.channel, s.endpoint, err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	return s.do(req)
}

func (s *genericSender) do(req *http.Request) error {
	return doAndCheck(req, s.client, s.channel, s.endpoint)
}

// barkSender POSTs <base>/<title>/<body>. When the configured base ends with
// "/" the query form <base>?title=...&body=... is used instead, mirroring
// Bark's own route pair (src-tauri/src/notify.rs:57-90).
type barkSender struct {
	channel  string
	endpoint string
	base     string
	query    bool
	client   *http.Client
}

func (s *barkSender) Send(ctx context.Context, event Event) error {
	target := s.base + "/" + url.PathEscape(event.Title) + "/" + url.PathEscape(event.Body)
	if s.query {
		target = s.base + "/?title=" + url.QueryEscape(event.Title) + "&body=" + url.QueryEscape(event.Body)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, nil)
	if err != nil {
		return redactedRequestError(s.channel, s.endpoint, err)
	}
	return doAndCheck(req, s.client, s.channel, s.endpoint)
}

// serverChanSender POSTs the form fields title/desp to the configured
// sctapi.ftqq.com sendkey endpoint. Newlines become markdown hard breaks, the
// same transformation A applies (src-tauri/src/notify.rs:115-137).
type serverChanSender struct {
	channel  string
	endpoint string
	client   *http.Client
}

func (s *serverChanSender) Send(ctx context.Context, event Event) error {
	form := url.Values{}
	form.Set("title", event.Title)
	form.Set("desp", strings.ReplaceAll(event.Body, "\n", "  \n"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return redactedRequestError(s.channel, s.endpoint, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return doAndCheck(req, s.client, s.channel, s.endpoint)
}

// weComSender POSTs the group-robot text message shape
// (src-tauri/src/notify.rs:93-112).
type weComSender struct {
	channel  string
	endpoint string
	client   *http.Client
}

func (s *weComSender) Send(ctx context.Context, event Event) error {
	payload, err := json.Marshal(map[string]any{
		"msgtype": "text",
		"text":    map[string]string{"content": event.Title + "\n" + event.Body},
	})
	if err != nil {
		return fmt.Errorf("notify: 编码通知内容失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return redactedRequestError(s.channel, s.endpoint, err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	return doAndCheck(req, s.client, s.channel, s.endpoint)
}

// doAndCheck performs one request and ignores the body. Only the status code
// is reported: an upstream error body could echo the webhook token.
func doAndCheck(req *http.Request, client *http.Client, channel, endpoint string) error {
	resp, err := client.Do(req)
	if err != nil {
		return redactedRequestError(channel, endpoint, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("notify: %s 返回 HTTP %d", channelLabels[channel], resp.StatusCode)
	}
	return nil
}

// redactedRequestError strips the request URL from a transport error. Go's
// *url.Error stringifies as `Post "<url>": <cause>`, which would leak the
// device key / sendkey; only the cause is kept and any remaining copy of the
// endpoint, its path or its query is masked.
func redactedRequestError(channel, endpoint string, err error) error {
	message := err.Error()
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		message = urlErr.Err.Error()
	}
	message = maskEndpoint(message, endpoint)
	return fmt.Errorf("notify: %s 请求失败：%s", channelLabels[channel], message)
}

// maskEndpoint removes every form of the endpoint credential from text.
func maskEndpoint(text, endpoint string) string {
	if endpoint != "" {
		text = strings.ReplaceAll(text, endpoint, RedactURL(endpoint))
	}
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return text
	}
	// PathEscaped variants appear for Bark, whose title/body live in the path.
	if parsed.Path != "" && parsed.Path != "/" {
		text = strings.ReplaceAll(text, parsed.Path, "/***")
		text = strings.ReplaceAll(text, url.PathEscape(parsed.Path), "/***")
	}
	if parsed.RawQuery != "" {
		text = strings.ReplaceAll(text, parsed.RawQuery, "***")
	}
	if parsed.User != nil {
		if secret, ok := parsed.User.Password(); ok && secret != "" {
			text = strings.ReplaceAll(text, secret, "***")
		}
	}
	return text
}
