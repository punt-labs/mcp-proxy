package reconnect_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/punt-labs/mcp-proxy/internal/debuglog"
	"github.com/punt-labs/mcp-proxy/internal/reconnect"
	"github.com/punt-labs/mcp-proxy/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hangableConn wraps a real Conn and can be told to stop responding to pings.
type hangableConn struct {
	reconnect.Conn
	mu   sync.Mutex
	hung bool
}

func (c *hangableConn) Hang() {
	c.mu.Lock()
	c.hung = true
	c.mu.Unlock()
}

func (c *hangableConn) Ping(ctx context.Context) error {
	c.mu.Lock()
	hung := c.hung
	c.mu.Unlock()
	if hung {
		// Block until context expires, simulating an unresponsive daemon.
		<-ctx.Done()
		return ctx.Err()
	}
	return c.Conn.Ping(ctx)
}

func dialMock(d *testutil.MockDaemon) reconnect.DialFunc {
	return func(ctx context.Context) (reconnect.Conn, error) {
		url := d.URL() + "?session_key=0"
		conn, _, err := websocket.Dial(ctx, url, nil)
		if err != nil {
			return nil, err
		}
		conn.SetReadLimit(1024 * 1024)
		return conn, nil
	}
}

// waitForConnCount polls until the mock daemon has accepted n connections.
func waitForConnCount(d *testutil.MockDaemon, n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if d.ConnCount() >= n {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// waitForLines polls until buf contains at least n newlines.
func waitForLines(buf *testutil.SafeBuffer, n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Count(buf.String(), "\n") >= n {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func TestBasicRequestResponse(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	stdinR, stdinW := io.Pipe()
	stdout := &testutil.SafeBuffer{}
	logger := debuglog.NewTestLogger(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- reconnect.Run(ctx, stdinR, stdout, dialMock(d), logger.Logger)
	}()

	require.True(t, waitForConnCount(d, 1, 2*time.Second), "timed out waiting for connection")

	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"ping","id":1}`)
	require.True(t, waitForLines(stdout, 1, 2*time.Second), "timed out waiting for response")
	assert.JSONEq(t, `{"jsonrpc":"2.0","method":"ping","id":1}`, strings.TrimSpace(stdout.String()))

	stdinW.Close()
	err := <-done
	assert.NoError(t, err)
}

func TestReconnect(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	stdinR, stdinW := io.Pipe()
	stdout := &testutil.SafeBuffer{}
	logger := debuglog.NewTestLogger(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- reconnect.Run(ctx, stdinR, stdout, dialMock(d), logger.Logger)
	}()

	// First connection: send a message.
	require.True(t, waitForConnCount(d, 1, 2*time.Second))
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"ping","id":1}`)
	require.True(t, waitForLines(stdout, 1, 2*time.Second))

	// Force disconnect.
	d.CloseConn()

	// Wait for reconnection.
	require.True(t, waitForConnCount(d, 2, 5*time.Second), "timed out waiting for reconnect")

	// Second connection: send another message.
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"ping","id":2}`)
	require.True(t, waitForLines(stdout, 2, 2*time.Second))

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	require.Len(t, lines, 2)
	assert.JSONEq(t, `{"jsonrpc":"2.0","method":"ping","id":1}`, lines[0])
	assert.JSONEq(t, `{"jsonrpc":"2.0","method":"ping","id":2}`, lines[1])

	stdinW.Close()
	err := <-done
	assert.NoError(t, err)
}

func TestMultipleReconnects(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	stdinR, stdinW := io.Pipe()
	stdout := &testutil.SafeBuffer{}
	logger := debuglog.NewTestLogger(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- reconnect.Run(ctx, stdinR, stdout, dialMock(d), logger.Logger)
	}()

	for cycle := range 3 {
		connN := cycle + 1
		require.True(t, waitForConnCount(d, connN, 10*time.Second), "timed out waiting for connection %d", connN)

		msg := fmt.Sprintf(`{"jsonrpc":"2.0","method":"ping","id":%d}`, connN)
		fmt.Fprintln(stdinW, msg)
		require.True(t, waitForLines(stdout, connN, 2*time.Second), "timed out waiting for response %d", connN)

		if cycle < 2 {
			d.CloseConn()
		}
	}

	stdinW.Close()
	err := <-done
	assert.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	assert.Len(t, lines, 3)
}

func TestStdinEOF(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	stdinR, stdinW := io.Pipe()
	stdout := &testutil.SafeBuffer{}
	logger := debuglog.NewTestLogger(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- reconnect.Run(ctx, stdinR, stdout, dialMock(d), logger.Logger)
	}()

	require.True(t, waitForConnCount(d, 1, 2*time.Second))
	stdinW.Close()

	err := <-done
	assert.NoError(t, err, "stdin EOF should produce nil error")
}

func TestInitialDialRetry(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	stdinR, stdinW := io.Pipe()
	stdout := &testutil.SafeBuffer{}
	logger := debuglog.NewTestLogger(t)

	failCount := 0
	dial := func(ctx context.Context) (reconnect.Conn, error) {
		if failCount < 2 {
			failCount++
			return nil, fmt.Errorf("simulated dial failure %d", failCount)
		}
		return dialMock(d)(ctx)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- reconnect.Run(ctx, stdinR, stdout, dial, logger.Logger)
	}()

	require.True(t, waitForConnCount(d, 1, 8*time.Second), "timed out waiting for connection after dial retries")

	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"ping","id":1}`)
	require.True(t, waitForLines(stdout, 1, 2*time.Second))

	stdinW.Close()
	err := <-done
	assert.NoError(t, err)
}

func TestContextCancellation(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	stdinR, _ := io.Pipe()
	stdout := &testutil.SafeBuffer{}
	logger := debuglog.NewTestLogger(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

	done := make(chan error, 1)
	go func() {
		done <- reconnect.Run(ctx, stdinR, stdout, dialMock(d), logger.Logger)
	}()

	require.True(t, waitForConnCount(d, 1, 2*time.Second))
	cancel()

	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

func TestMessagePreservation(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	stdinR, stdinW := io.Pipe()
	stdout := &testutil.SafeBuffer{}
	logger := debuglog.NewTestLogger(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- reconnect.Run(ctx, stdinR, stdout, dialMock(d), logger.Logger)
	}()

	require.True(t, waitForConnCount(d, 1, 2*time.Second))

	// Send first message and confirm it arrives.
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"ping","id":1}`)
	require.True(t, waitForLines(stdout, 1, 2*time.Second))

	// Queue a message and immediately disconnect.
	// The message should survive the reconnect.
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"ping","id":2}`)
	// Give tiny window for the write to enter the channel.
	time.Sleep(50 * time.Millisecond)
	d.CloseConn()

	// Wait for reconnect.
	require.True(t, waitForConnCount(d, 2, 5*time.Second))

	// The second message should eventually arrive.
	require.True(t, waitForLines(stdout, 2, 3*time.Second), "message lost during reconnect")

	stdinW.Close()
	err := <-done
	assert.NoError(t, err)
}

func TestPingTimeout_TriggersReconnect(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	stdinR, stdinW := io.Pipe()
	stdout := &testutil.SafeBuffer{}
	logger := debuglog.NewTestLogger(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var currentConn *hangableConn
	var connMu sync.Mutex

	dial := func(dialCtx context.Context) (reconnect.Conn, error) {
		conn, err := dialMock(d)(dialCtx)
		if err != nil {
			return nil, err
		}
		hc := &hangableConn{Conn: conn}
		connMu.Lock()
		currentConn = hc
		connMu.Unlock()
		return hc, nil
	}

	cfg := reconnect.Config{
		PingInterval: 100 * time.Millisecond,
		PongTimeout:  50 * time.Millisecond,
	}

	done := make(chan error, 1)
	go func() {
		done <- reconnect.RunWithConfig(ctx, stdinR, stdout, dial, cfg, logger.Logger)
	}()

	// Wait for first connection and ensure currentConn is assigned.
	require.True(t, waitForConnCount(d, 1, 2*time.Second), "timed out waiting for connection")
	require.Eventually(t, func() bool {
		connMu.Lock()
		defer connMu.Unlock()
		return currentConn != nil
	}, 2*time.Second, 10*time.Millisecond, "timed out waiting for currentConn assignment")

	// Send a message to verify the connection works.
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"ping","id":1}`)
	require.True(t, waitForLines(stdout, 1, 2*time.Second))

	// Make the daemon appear hung — pings will block until timeout.
	connMu.Lock()
	currentConn.Hang()
	connMu.Unlock()

	// Proxy should detect the hang and reconnect.
	require.True(t, waitForConnCount(d, 2, 5*time.Second), "proxy should reconnect after ping timeout")

	// New connection should work.
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"ping","id":2}`)
	require.True(t, waitForLines(stdout, 2, 2*time.Second), "second message should arrive after reconnect")

	stdinW.Close()
	err := <-done
	assert.NoError(t, err)
}

// mcpHandler returns a MockDaemon handler that responds to MCP lifecycle
// messages and echoes everything else. version is embedded in the
// initialize response's serverInfo.
func mcpHandler(version func() string) func([]byte) []byte {
	return func(msg []byte) []byte {
		var env struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
		}
		if json.Unmarshal(msg, &env) != nil {
			return msg // echo
		}
		switch env.Method {
		case "initialize":
			return []byte(fmt.Sprintf(
				`{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-03-26","capabilities":{},"serverInfo":{"name":"mock","version":"%s"}}}`,
				env.ID, version()))
		case "notifications/initialized":
			return nil // notification — no response
		default:
			return msg // echo
		}
	}
}

func TestReconnect_HandshakeReplay(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()
	d.Handler = mcpHandler(func() string { return "1.0" })

	stdinR, stdinW := io.Pipe()
	stdout := &testutil.SafeBuffer{}
	logger := debuglog.NewTestLogger(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- reconnect.Run(ctx, stdinR, stdout, dialMock(d), logger.Logger)
	}()

	require.True(t, waitForConnCount(d, 1, 2*time.Second))

	// MCP handshake: initialize request → response on stdout.
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"initialize","id":0,"params":{}}`)
	require.True(t, waitForLines(stdout, 1, 2*time.Second), "timed out waiting for initialize response")

	// MCP handshake: notifications/initialized — produces no stdout output.
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	time.Sleep(50 * time.Millisecond)

	// Normal tool call on first connection.
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"tools/call","id":1}`)
	require.True(t, waitForLines(stdout, 2, 2*time.Second), "timed out waiting for tools/call echo")

	prevLen := len(d.Received())

	// Force disconnect and wait for reconnect.
	d.CloseConn()
	require.True(t, waitForConnCount(d, 2, 5*time.Second), "timed out waiting for reconnect")

	// Tool call on second connection.
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"tools/call","id":2}`)
	require.True(t, waitForLines(stdout, 3, 2*time.Second), "timed out waiting for post-reconnect tools/call")

	// Verify daemon received replayed handshake + the new tool call.
	msgs := d.ReceivedSince(prevLen)
	require.Len(t, msgs, 3, "expected initialize + initialized + tools/call after reconnect")
	assert.Contains(t, string(msgs[0]), `"initialize"`)
	assert.Contains(t, string(msgs[1]), `"notifications/initialized"`)
	assert.Contains(t, string(msgs[2]), `"tools/call"`)

	// Verify stdout: 3 lines total. Line 0 = initialize response, lines 1-2 = tools/call echoes.
	// The replayed initialize response is swallowed — no duplicate.
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	require.Len(t, lines, 3)
	assert.Contains(t, lines[0], `"serverInfo"`)
	assert.Contains(t, lines[1], `"tools/call"`)
	assert.Contains(t, lines[2], `"tools/call"`)

	stdinW.Close()
	runErr := <-done
	assert.NoError(t, runErr)
}

func TestReconnect_SwallowsReplayedResponse(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	var connN atomic.Int32
	d.Handler = mcpHandler(func() string {
		n := connN.Add(1)
		return fmt.Sprintf("%d.0", n)
	})

	stdinR, stdinW := io.Pipe()
	stdout := &testutil.SafeBuffer{}
	logger := debuglog.NewTestLogger(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- reconnect.Run(ctx, stdinR, stdout, dialMock(d), logger.Logger)
	}()

	require.True(t, waitForConnCount(d, 1, 2*time.Second))

	// MCP handshake.
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"initialize","id":0,"params":{}}`)
	require.True(t, waitForLines(stdout, 1, 2*time.Second))
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	time.Sleep(50 * time.Millisecond)

	// Disconnect and reconnect — daemon will respond with version "2.0".
	d.CloseConn()
	require.True(t, waitForConnCount(d, 2, 5*time.Second))

	// Send a tool call to confirm the connection works.
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"tools/call","id":1}`)
	require.True(t, waitForLines(stdout, 2, 2*time.Second))

	// stdout should contain server version "1.0" (original) but NOT "2.0" (replayed, swallowed).
	out := stdout.String()
	assert.Contains(t, out, `"version":"1.0"`, "original initialize response should be in stdout")
	assert.NotContains(t, out, `"version":"2.0"`, "replayed initialize response should be swallowed")

	stdinW.Close()
	runErr := <-done
	assert.NoError(t, runErr)
}

func TestReconnect_NoHandshakeCached(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	stdinR, stdinW := io.Pipe()
	stdout := &testutil.SafeBuffer{}
	logger := debuglog.NewTestLogger(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- reconnect.Run(ctx, stdinR, stdout, dialMock(d), logger.Logger)
	}()

	require.True(t, waitForConnCount(d, 1, 2*time.Second))

	// Send a plain ping — no initialize handshake.
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"ping","id":1}`)
	require.True(t, waitForLines(stdout, 1, 2*time.Second))

	// Disconnect and reconnect.
	d.CloseConn()
	require.True(t, waitForConnCount(d, 2, 5*time.Second))

	// Send another ping on the new connection.
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"ping","id":2}`)
	require.True(t, waitForLines(stdout, 2, 2*time.Second))

	// Daemon should have received only the ping (id=2) after reconnect — no replayed handshake.
	msgs := d.ReceivedSince(1)
	require.Len(t, msgs, 1, "no handshake should be replayed when none was cached")
	assert.Contains(t, string(msgs[0]), `"id":2`)

	// stdout: 2 lines, both echoed pings.
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	require.Len(t, lines, 2)

	stdinW.Close()
	runErr := <-done
	assert.NoError(t, runErr)
}

func TestReconnect_PreservesClientIDs(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()
	d.Handler = mcpHandler(func() string { return "1.0" })

	stdinR, stdinW := io.Pipe()
	stdout := &testutil.SafeBuffer{}
	logger := debuglog.NewTestLogger(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- reconnect.Run(ctx, stdinR, stdout, dialMock(d), logger.Logger)
	}()

	require.True(t, waitForConnCount(d, 1, 2*time.Second))

	// MCP handshake + tool call.
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"initialize","id":0,"params":{}}`)
	require.True(t, waitForLines(stdout, 1, 2*time.Second))
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	time.Sleep(50 * time.Millisecond)
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"tools/call","id":1}`)
	require.True(t, waitForLines(stdout, 2, 2*time.Second))

	// Disconnect, reconnect, send another tool call.
	d.CloseConn()
	require.True(t, waitForConnCount(d, 2, 5*time.Second))
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"tools/call","id":2}`)
	require.True(t, waitForLines(stdout, 3, 2*time.Second))

	// Verify stdout lines preserve client IDs in order: 0, 1, 2.
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	require.Len(t, lines, 3)

	expectedIDs := []json.RawMessage{
		json.RawMessage(`0`),
		json.RawMessage(`1`),
		json.RawMessage(`2`),
	}
	for i, line := range lines {
		var env struct {
			ID json.RawMessage `json:"id"`
		}
		require.NoError(t, json.Unmarshal([]byte(line), &env), "failed to parse stdout line %d", i)
		assert.JSONEq(t, string(expectedIDs[i]), string(env.ID), "line %d id mismatch", i)
	}

	stdinW.Close()
	runErr := <-done
	assert.NoError(t, runErr)
}

func TestHandshake_SniffIgnoresNullID(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()
	d.Handler = mcpHandler(func() string { return "1.0" })

	stdinR, stdinW := io.Pipe()
	stdout := &testutil.SafeBuffer{}
	logger := debuglog.NewTestLogger(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- reconnect.Run(ctx, stdinR, stdout, dialMock(d), logger.Logger)
	}()

	require.True(t, waitForConnCount(d, 1, 2*time.Second))

	// Send initialize with no "id" field — malformed JSON-RPC.
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"initialize","params":{}}`)
	// The handler will try to respond, but with null id. Wait a beat for it.
	time.Sleep(100 * time.Millisecond)

	// Send a normal tool call to confirm the connection works.
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"tools/call","id":1}`)
	require.True(t, waitForLines(stdout, 1, 2*time.Second), "timed out waiting for tools/call echo")

	prevLen := len(d.Received())

	// Force disconnect and wait for reconnect.
	d.CloseConn()
	require.True(t, waitForConnCount(d, 2, 5*time.Second), "timed out waiting for reconnect")

	// Send another tool call on the new connection.
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"tools/call","id":2}`)
	require.True(t, waitForLines(stdout, 2, 2*time.Second), "timed out waiting for post-reconnect tools/call")

	// Verify daemon received only the new tool call — no replayed handshake.
	msgs := d.ReceivedSince(prevLen)
	require.Len(t, msgs, 1, "no handshake should be replayed when initialize had no id")
	assert.Contains(t, string(msgs[0]), `"tools/call"`)

	stdinW.Close()
	runErr := <-done
	assert.NoError(t, runErr)
}

func TestHandshake_SniffIgnoresExplicitNullID(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()
	d.Handler = mcpHandler(func() string { return "1.0" })

	stdinR, stdinW := io.Pipe()
	stdout := &testutil.SafeBuffer{}
	logger := debuglog.NewTestLogger(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- reconnect.Run(ctx, stdinR, stdout, dialMock(d), logger.Logger)
	}()

	require.True(t, waitForConnCount(d, 1, 2*time.Second))

	// Send initialize with explicit "id":null — also malformed.
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"initialize","id":null,"params":{}}`)
	time.Sleep(100 * time.Millisecond)

	// Normal tool call.
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"tools/call","id":1}`)
	require.True(t, waitForLines(stdout, 1, 2*time.Second))

	prevLen := len(d.Received())

	// Disconnect and reconnect.
	d.CloseConn()
	require.True(t, waitForConnCount(d, 2, 5*time.Second))

	// Tool call on new connection.
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"tools/call","id":2}`)
	require.True(t, waitForLines(stdout, 2, 2*time.Second))

	// No replayed handshake.
	msgs := d.ReceivedSince(prevLen)
	require.Len(t, msgs, 1, "no handshake should be replayed when initialize had null id")
	assert.Contains(t, string(msgs[0]), `"tools/call"`)

	stdinW.Close()
	runErr := <-done
	assert.NoError(t, runErr)
}

func TestMultipleReconnects_HandshakeReplay(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()
	d.Handler = mcpHandler(func() string { return "1.0" })

	stdinR, stdinW := io.Pipe()
	stdout := &testutil.SafeBuffer{}
	logger := debuglog.NewTestLogger(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- reconnect.Run(ctx, stdinR, stdout, dialMock(d), logger.Logger)
	}()

	require.True(t, waitForConnCount(d, 1, 2*time.Second))

	// MCP handshake on first connection.
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"initialize","id":0,"params":{}}`)
	require.True(t, waitForLines(stdout, 1, 2*time.Second))
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	time.Sleep(50 * time.Millisecond)

	// 3 reconnect cycles, each with a tools/call.
	for i := range 3 {
		d.CloseConn()
		require.True(t, waitForConnCount(d, i+2, 5*time.Second), "timed out waiting for reconnect %d", i+1)

		msg := fmt.Sprintf(`{"jsonrpc":"2.0","method":"tools/call","id":%d}`, i+1)
		fmt.Fprintln(stdinW, msg)
		require.True(t, waitForLines(stdout, i+2, 2*time.Second), "timed out waiting for tools/call response on reconnect %d", i+1)
	}

	// stdout: 1 initialize response + 3 tools/call echoes = 4 lines.
	// No duplicate initialize responses from any of the 3 replays.
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	require.Len(t, lines, 4, "expected 1 initialize response + 3 tools/call echoes")
	assert.Contains(t, lines[0], `"serverInfo"`)
	for i := 1; i <= 3; i++ {
		assert.Contains(t, lines[i], `"tools/call"`, "line %d should be a tools/call echo", i)
	}

	stdinW.Close()
	runErr := <-done
	assert.NoError(t, runErr)
}
