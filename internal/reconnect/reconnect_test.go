package reconnect_test

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/punt-labs/mcp-proxy/internal/debuglog"
	"github.com/punt-labs/mcp-proxy/internal/reconnect"
	"github.com/punt-labs/mcp-proxy/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/coder/websocket"
)

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
		require.True(t, waitForConnCount(d, connN, 5*time.Second), "timed out waiting for connection %d", connN)

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
