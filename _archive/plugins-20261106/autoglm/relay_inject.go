package main

// Relay device-online handshake (the P2-2 gap).
//
// The relay channel only routes agent events to a device that announced itself
// on the sandbox's websocket. The reference (autoclaw/chat.go openDeviceWS) is
// the authority for the protocol:
//
//	URL    {ws base}/v1/client/ws?device_id=<sandbox id>&access_token=<token>
//	send   {"type":"auth.inject","token":<token>,
//	        "clientMetadata":{"client_type":"web"},"userId":...,"userName":...}
//	wait   {"type":"auth.inject.ok"} (or auth.inject.error → the run must fail)
//
// A failure here is a warning, not a hard stop: the reference falls back to the
// HTTP bridge with the same semantics ("fallback to http bridge"). The socket
// is kept open for the whole run because the relay addresses events to the
// online connection.

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// deviceWSWait bounds the auth.inject acknowledgement wait (reference: 25 s).
const deviceWSWait = 25 * time.Second

// openDeviceWS puts the account's sandbox device online and returns the closer.
func (u *upstream) openDeviceWS(a *account) (func(), error) {
	snap := a.snapshot()
	base := snap.SandboxEndpoint
	if base == "" || !strings.Contains(base, "/proxy/") {
		return nil, fmt.Errorf("no relay proxy endpoint (sandbox %q)", snap.SandboxEndpoint)
	}
	bare := relayBaseFromProxy(base)
	token := stripBearer(snap.AccessToken)
	wsURL := fmt.Sprintf("%s/v1/client/ws?device_id=%s&access_token=%s",
		toWSBase(bare), snap.SandboxID, token)
	ctx, cancel := context.WithTimeout(context.Background(), deviceWSWait)
	defer cancel()
	conn, err := wsDial(ctx, wsURL)
	if err != nil {
		return nil, err
	}
	frame := map[string]any{
		"type":           "auth.inject",
		"token":          token,
		"clientMetadata": map[string]any{"client_type": "web"},
	}
	if snap.UserID != "" {
		frame["userId"] = snap.UserID
	}
	if snap.UserName != "" {
		frame["userName"] = snap.UserName
	}
	if err := conn.WriteJSON(frame); err != nil {
		conn.Close()
		return nil, fmt.Errorf("auth.inject write: %w", err)
	}
	deadline := time.Now().Add(deviceWSWait)
	for time.Now().Before(deadline) {
		var reply map[string]any
		if err := conn.ReadJSON(&reply, deadline); err != nil {
			conn.Close()
			return nil, fmt.Errorf("auth.inject wait: %w", err)
		}
		switch toString(reply["type"]) {
		case "auth.inject.ok":
			return conn.Close, nil
		case "auth.inject.error":
			conn.Close()
			return nil, fmt.Errorf("auth.inject failed: %s", truncStr(toString(reply), 300))
		}
	}
	conn.Close()
	return nil, fmt.Errorf("auth.inject timeout after %s", deviceWSWait)
}
