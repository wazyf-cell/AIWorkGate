package main

// A minimal RFC 6455 websocket client.
//
// The relay needs exactly one socket operation: connect to the sandbox's
// /v1/client/ws, send one JSON text frame (auth.inject), and wait for the
// server's JSON answer. A full websocket library would be a new module
// dependency, and this plugin has none by design, so the handshake and the
// frame codec live here: text frames only, client masking, ping/pong, close.
//
// Scope limits, stated so nobody assumes more:
//   - no extensions, no subprotocols, no compression (the reference uses none);
//   - continuation frames are reassembled, but only text payloads are returned;
//   - binary frames are ignored (the protocol carries JSON).

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const (
	wsOpContinuation = 0x0
	wsOpText         = 0x1
	wsOpBinary       = 0x2
	wsOpClose        = 0x8
	wsOpPing         = 0x9
	wsOpPong         = 0xA
)

// wsConn is one established client connection.
type wsConn struct {
	conn net.Conn
	rd   *bufio.Reader
}

// wsDial performs the opening handshake and returns the framed connection.
func wsDial(ctx context.Context, rawURL string) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("ws: parse url: %w", err)
	}
	switch u.Scheme {
	case "ws", "wss":
	default:
		return nil, fmt.Errorf("ws: unsupported scheme %q", u.Scheme)
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		if u.Scheme == "wss" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	raw, err := dialer.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("ws: dial %s: %w", host, err)
	}
	if u.Scheme == "wss" {
		tlsConn := tls.Client(raw, &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = raw.Close()
			return nil, fmt.Errorf("ws: tls handshake: %w", err)
		}
		raw = tlsConn
	}

	var keyBytes [16]byte
	if _, err := rand.Read(keyBytes[:]); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("ws: cannot generate handshake key: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(keyBytes[:])
	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if deadline, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(deadline)
	} else {
		_ = raw.SetDeadline(time.Now().Add(30 * time.Second))
	}
	if _, err := io.WriteString(raw, req); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("ws: write handshake: %w", err)
	}
	br := bufio.NewReader(raw)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("ws: read handshake: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = raw.Close()
		return nil, fmt.Errorf("ws: handshake refused with HTTP %d", resp.StatusCode)
	}
	want := wsAccept(key)
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != want {
		_ = raw.Close()
		return nil, fmt.Errorf("ws: Sec-WebSocket-Accept mismatch")
	}
	_ = raw.SetDeadline(time.Time{})
	return &wsConn{conn: raw, rd: br}, nil
}

// wsAccept computes the RFC 6455 accept token.
func wsAccept(key string) string {
	sum := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// WriteJSON sends v as one masked text frame.
func (c *wsConn) WriteJSON(v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.writeFrame(wsOpText, payload)
}

func (c *wsConn) writeFrame(opcode byte, payload []byte) error {
	var header [14]byte
	header[0] = 0x80 | opcode // FIN + opcode
	n := len(payload)
	var hlen int
	switch {
	case n < 126:
		header[1] = 0x80 | byte(n)
		hlen = 2
	case n <= 0xFFFF:
		header[1] = 0x80 | 126
		binary.BigEndian.PutUint16(header[2:], uint16(n))
		hlen = 4
	default:
		header[1] = 0x80 | 127
		binary.BigEndian.PutUint64(header[2:], uint64(n))
		hlen = 10
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return fmt.Errorf("ws: mask: %w", err)
	}
	copy(header[hlen:], mask[:])
	hlen += 4
	masked := make([]byte, n)
	for i := 0; i < n; i++ {
		masked[i] = payload[i] ^ mask[i%4]
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	if _, err := c.conn.Write(header[:hlen]); err != nil {
		return err
	}
	_, err := c.conn.Write(masked)
	return err
}

// ReadJSON waits for one text message and decodes it into v, answering pings on
// the way. deadline bounds the whole wait.
func (c *wsConn) ReadJSON(v any, deadline time.Time) error {
	for {
		opcode, payload, err := c.readMessage(deadline)
		if err != nil {
			return err
		}
		switch opcode {
		case wsOpText:
			return json.Unmarshal(payload, v)
		case wsOpBinary:
			continue
		default:
			continue
		}
	}
}

// readMessage returns one complete (possibly reassembled) message.
func (c *wsConn) readMessage(deadline time.Time) (byte, []byte, error) {
	if err := c.conn.SetReadDeadline(deadline); err != nil {
		return 0, nil, err
	}
	var messageOp byte
	var payload []byte
	for {
		fin, opcode, chunk, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch opcode {
		case wsOpPing:
			_ = c.writeFrame(wsOpPong, chunk)
			continue
		case wsOpPong:
			continue
		case wsOpClose:
			_ = c.writeFrame(wsOpClose, nil)
			return 0, nil, io.EOF
		case wsOpText, wsOpBinary:
			messageOp = opcode
			payload = append(payload, chunk...)
		case wsOpContinuation:
			payload = append(payload, chunk...)
		}
		if fin {
			return messageOp, payload, nil
		}
	}
}

func (c *wsConn) readFrame() (fin bool, opcode byte, payload []byte, err error) {
	var head [2]byte
	if _, err = io.ReadFull(c.rd, head[:]); err != nil {
		return false, 0, nil, err
	}
	fin = head[0]&0x80 != 0
	opcode = head[0] & 0x0F
	masked := head[1]&0x80 != 0
	length := uint64(head[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.rd, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.rd, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	if length > 8<<20 {
		return false, 0, nil, fmt.Errorf("ws: frame of %d bytes exceeds the 8 MiB limit", length)
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(c.rd, mask[:]); err != nil {
			return false, 0, nil, err
		}
	}
	payload = make([]byte, length)
	if _, err = io.ReadFull(c.rd, payload); err != nil {
		return false, 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return fin, opcode, payload, nil
}

// Close sends a close frame (best effort) and closes the socket.
func (c *wsConn) Close() {
	_ = c.writeFrame(wsOpClose, nil)
	_ = c.conn.Close()
}

// toWSBase converts an http(s) relay base to its ws(s) form.
func toWSBase(base string) string {
	switch {
	case strings.HasPrefix(base, "https://"):
		return "wss://" + base[len("https://"):]
	case strings.HasPrefix(base, "http://"):
		return "ws://" + base[len("http://"):]
	}
	return base
}
