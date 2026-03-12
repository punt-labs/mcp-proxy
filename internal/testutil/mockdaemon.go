// Package testutil provides test infrastructure for mcp-proxy.
package testutil

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"

	"nhooyr.io/websocket"
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
	d.Server = httptest.NewServer(mux)
	return d
}

// URL returns the WebSocket URL for connecting to this daemon.
func (d *MockDaemon) URL() string {
	return "ws" + d.Server.URL[len("http"):] + "/mcp"
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

	// Goroutine for sending push messages.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-d.Push:
				if err := conn.Write(ctx, websocket.MessageText, msg); err != nil {
					d.mu.Lock()
					d.pushErr = err
					d.mu.Unlock()
					return
				}
			}
		}
	}()

	// Read loop.
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
			if err := conn.Write(ctx, websocket.MessageText, resp); err != nil {
				break
			}
		}
	}

	d.mu.Lock()
	d.disconnected = true
	d.conn = nil
	d.mu.Unlock()
}
