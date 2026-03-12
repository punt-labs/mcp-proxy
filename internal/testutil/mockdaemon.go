// Package testutil provides test infrastructure for mcp-proxy.
package testutil

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"

	"github.com/coder/websocket"
)

// MockDaemon is an httptest.Server that upgrades to WebSocket at /mcp.
// It supports configurable response handling and unsolicited push messages.
type MockDaemon struct {
	Server *httptest.Server

	// Handler processes each received message and returns a response.
	// If nil, messages are echoed back unchanged.
	Handler func([]byte) []byte

	// Push is a channel for sending unsolicited messages to the connected client.
	// Write a message to this channel and the daemon will send it.
	Push chan []byte

	mu           sync.Mutex
	sessionKey   string
	authHeader   string
	received     [][]byte
	connected    bool
	disconnected bool
	conn         *websocket.Conn
	connCount    int
	acceptErr    error
	pushErr      error
}

// NewMockDaemon creates and starts a mock daemon server.
func NewMockDaemon() *MockDaemon {
	d := &MockDaemon{
		Push: make(chan []byte, 100),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", d.handleWebSocket)
	mux.HandleFunc("/hook", d.handleHook)
	d.Server = httptest.NewServer(mux)
	return d
}

// URL returns the WebSocket URL for connecting to this daemon's MCP endpoint.
func (d *MockDaemon) URL() string {
	return "ws" + d.Server.URL[len("http"):] + "/mcp"
}

// BaseURL returns the base WebSocket URL without any path.
// Used for hook relay tests where the proxy appends "/hook" itself.
func (d *MockDaemon) BaseURL() string {
	return "ws" + d.Server.URL[len("http"):]
}

// HookURL returns the WebSocket URL for connecting to this daemon's hook endpoint.
func (d *MockDaemon) HookURL() string {
	return "ws" + d.Server.URL[len("http"):] + "/hook"
}

// SessionKey returns the session_key received on the last connection upgrade.
func (d *MockDaemon) SessionKey() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sessionKey
}

// AuthHeader returns the Authorization header received on the last connection upgrade.
func (d *MockDaemon) AuthHeader() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.authHeader
}

// Received returns a copy of all messages received by the daemon.
func (d *MockDaemon) Received() [][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([][]byte, len(d.received))
	copy(out, d.received)
	return out
}

// Connected returns whether a client has connected.
func (d *MockDaemon) Connected() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.connected
}

// Disconnected returns whether the client has disconnected.
func (d *MockDaemon) Disconnected() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.disconnected
}

// AcceptErr returns the last WebSocket accept error, if any.
// Useful for diagnosing test timeouts caused by failed upgrades.
func (d *MockDaemon) AcceptErr() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.acceptErr
}

// PushErr returns the last push write error, if any.
// Useful for diagnosing test timeouts when a push message was never delivered.
func (d *MockDaemon) PushErr() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pushErr
}

// ConnCount returns the total number of WebSocket connections accepted.
func (d *MockDaemon) ConnCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.connCount
}

// Close shuts down the mock daemon.
func (d *MockDaemon) Close() {
	d.CloseConn()
	d.Server.Close()
}

// CloseConn forcibly closes the active WebSocket connection (if any)
// without shutting down the server.
func (d *MockDaemon) CloseConn() {
	d.mu.Lock()
	c := d.conn
	d.mu.Unlock()
	if c != nil {
		c.Close(websocket.StatusGoingAway, "daemon shutting down")
	}
}

func (d *MockDaemon) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	d.sessionKey = r.URL.Query().Get("session_key")
	d.authHeader = r.Header.Get("Authorization")
	d.acceptErr = nil
	d.pushErr = nil
	d.mu.Unlock()

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		d.mu.Lock()
		d.acceptErr = err
		d.mu.Unlock()
		return
	}
	conn.SetReadLimit(1024 * 1024) // 1MB to match proxy
	defer conn.Close(websocket.StatusNormalClosure, "")

	d.mu.Lock()
	d.connected = true
	d.conn = conn
	d.connCount++
	d.mu.Unlock()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// All outbound messages go through this channel so there is only one
	// concurrent writer on the conn (coder/websocket guarantees one reader
	// + one writer, not two writers).
	writes := make(chan []byte, 100)

	// Writer goroutine: sole writer on conn.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-writes:
				if err := conn.Write(ctx, websocket.MessageText, msg); err != nil {
					if ctx.Err() != nil {
						return
					}
					d.mu.Lock()
					d.pushErr = err
					d.mu.Unlock()
					cancel() // unblock all goroutines on this connection
					return
				}
			}
		}
	}()

	// Push forwarder: routes external push messages into the writes channel.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-d.Push:
				select {
				case writes <- msg:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	// Read loop: reads from conn, sends responses via writes channel.
readLoop:
	for {
		_, msg, err := conn.Read(ctx)
		if err != nil {
			break
		}

		d.mu.Lock()
		d.received = append(d.received, msg)
		handler := d.Handler
		d.mu.Unlock()

		var resp []byte
		if handler != nil {
			resp = handler(msg)
		} else {
			resp = msg // echo
		}

		if resp != nil {
			select {
			case writes <- resp:
			case <-ctx.Done():
				break readLoop
			}
		}
	}

	d.mu.Lock()
	d.disconnected = true
	d.conn = nil
	d.mu.Unlock()
}

// handleHook implements the /hook endpoint for one-shot hook relay connections.
// It reads exactly one message, optionally responds (if the message is a JSON-RPC
// request with an id), then closes cleanly.
func (d *MockDaemon) handleHook(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	d.sessionKey = r.URL.Query().Get("session_key")
	d.authHeader = r.Header.Get("Authorization")
	d.acceptErr = nil
	d.mu.Unlock()

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		d.mu.Lock()
		d.acceptErr = err
		d.mu.Unlock()
		return
	}
	conn.SetReadLimit(1024 * 1024)
	defer conn.Close(websocket.StatusNormalClosure, "")

	d.mu.Lock()
	d.connected = true
	d.conn = conn
	d.connCount++
	d.mu.Unlock()

	defer func() {
		d.mu.Lock()
		d.disconnected = true
		d.conn = nil
		d.mu.Unlock()
	}()

	// Read exactly one message.
	_, msg, err := conn.Read(r.Context())
	if err != nil {
		return
	}

	d.mu.Lock()
	d.received = append(d.received, msg)
	handler := d.Handler
	d.mu.Unlock()

	// Check if it's a request (has "id") or notification (no "id").
	var envelope struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(msg, &envelope); err != nil || len(envelope.ID) == 0 || string(envelope.ID) == "null" {
		// Notification — no response needed. Clean close.
		return
	}

	// Request — send response via handler or echo.
	var resp []byte
	if handler != nil {
		resp = handler(msg)
	} else {
		resp = msg
	}
	if resp != nil {
		_ = conn.Write(r.Context(), websocket.MessageText, resp)
	}
}
