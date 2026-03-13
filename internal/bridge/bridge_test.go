package bridge_test

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/punt-labs/mcp-proxy/internal/bridge"
	"github.com/punt-labs/mcp-proxy/internal/debuglog"
	"github.com/punt-labs/mcp-proxy/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/coder/websocket"
)

// testLogger returns a debug logger that writes to the test log.
// Log output appears only when tests fail or with -v.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return debuglog.NewTestLogger(t).Logger
}

// waitForOutput polls stdoutBuf until it contains at least n lines or timeout.
func waitForOutput(buf *testutil.SafeBuffer, n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s := strings.TrimSpace(buf.String())
		if s != "" && len(strings.Split(s, "\n")) >= n {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func TestBridge_RequestResponse(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		handler func([]byte) []byte
		want    string
	}{
		{
			name:  "simple request echoed back",
			input: `{"jsonrpc":"2.0","method":"ping","id":1}`,
			want:  `{"jsonrpc":"2.0","method":"ping","id":1}`,
		},
		{
			name:  "notification (no id)",
			input: `{"jsonrpc":"2.0","method":"notifications/initialized"}`,
			want:  `{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		},
		{
			name:  "custom response",
			input: `{"jsonrpc":"2.0","method":"tools/list","id":2}`,
			handler: func([]byte) []byte {
				return []byte(`{"jsonrpc":"2.0","result":{"tools":[]},"id":2}`)
			},
			want: `{"jsonrpc":"2.0","result":{"tools":[]},"id":2}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := testutil.NewMockDaemon()
			defer d.Close()
			if tt.handler != nil {
				d.Handler = tt.handler
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			conn, _, err := websocket.Dial(ctx, d.URL(), nil)
			require.NoError(t, err)

			stdinR, stdinW, stdoutBuf, stdoutW := testutil.StdioPair()

			done := make(chan error, 1)
			go func() {
				done <- bridge.Run(ctx, stdinR, stdoutW, conn, testLogger(t))
			}()

			// Send request and wait for response before closing stdin.
			fmt.Fprintln(stdinW, tt.input)
			require.True(t, waitForOutput(stdoutBuf, 1, 3*time.Second), "timed out waiting for response")
			stdinW.Close()

			err = <-done
			assert.NoError(t, err)
			assert.Equal(t, tt.want+"\n", stdoutBuf.String())
		})
	}
}

func TestBridge_MultipleMessages(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, d.URL(), nil)
	require.NoError(t, err)

	stdinR, stdinW, stdoutBuf, stdoutW := testutil.StdioPair()

	done := make(chan error, 1)
	go func() {
		done <- bridge.Run(ctx, stdinR, stdoutW, conn, testLogger(t))
	}()

	for i := range 5 {
		fmt.Fprintf(stdinW, `{"jsonrpc":"2.0","method":"ping","id":%d}`+"\n", i+1)
	}
	require.True(t, waitForOutput(stdoutBuf, 5, 3*time.Second), "timed out waiting for 5 responses")
	stdinW.Close()

	err = <-done
	assert.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(stdoutBuf.String()), "\n")
	assert.Len(t, lines, 5)
}

func TestBridge_BidirectionalPush(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	// Handler returns nil for requests — no echo. We test push only.
	d.Handler = func(msg []byte) []byte { return nil }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, d.URL(), nil)
	require.NoError(t, err)

	stdinR, stdinW, stdoutBuf, stdoutW := testutil.StdioPair()

	done := make(chan error, 1)
	go func() {
		done <- bridge.Run(ctx, stdinR, stdoutW, conn, testLogger(t))
	}()

	// Send a request to ensure connection is live before pushing.
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"init","id":1}`)
	require.Eventually(t, func() bool {
		return len(d.Received()) >= 1
	}, 3*time.Second, 5*time.Millisecond, "timed out waiting for daemon to receive init")

	push := `{"jsonrpc":"2.0","method":"tools/list_changed"}`
	d.Push <- []byte(push)
	require.True(t, waitForOutput(stdoutBuf, 1, 3*time.Second), "timed out waiting for push")

	stdinW.Close()
	err = <-done
	assert.NoError(t, err)
	assert.Contains(t, stdoutBuf.String(), push)
}

func TestBridge_ConcurrentTraffic(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, d.URL(), nil)
	require.NoError(t, err)

	stdinR, stdinW, stdoutBuf, stdoutW := testutil.StdioPair()

	done := make(chan error, 1)
	go func() {
		done <- bridge.Run(ctx, stdinR, stdoutW, conn, testLogger(t))
	}()

	const numRequests = 50
	const numPushes = 20

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range numRequests {
			fmt.Fprintf(stdinW, `{"jsonrpc":"2.0","method":"ping","id":%d}`+"\n", i+1)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(10 * time.Millisecond)
		for range numPushes {
			d.Push <- []byte(`{"jsonrpc":"2.0","method":"notification"}`)
		}
	}()

	wg.Wait()
	// Wait for at least the echoed requests.
	require.True(t, waitForOutput(stdoutBuf, numRequests, 5*time.Second), "timed out waiting for responses")
	stdinW.Close()

	err = <-done
	assert.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(stdoutBuf.String()), "\n")
	assert.GreaterOrEqual(t, len(lines), numRequests)
}

func TestBridge_StdinEOFCleanShutdown(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, d.URL(), nil)
	require.NoError(t, err)

	stdinR, stdinW, _, stdoutW := testutil.StdioPair()

	done := make(chan error, 1)
	go func() {
		done <- bridge.Run(ctx, stdinR, stdoutW, conn, testLogger(t))
	}()

	stdinW.Close()

	err = <-done
	assert.NoError(t, err, "stdin EOF should result in clean shutdown")
}

func TestBridge_DaemonDisconnect(t *testing.T) {
	d := testutil.NewMockDaemon()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, d.URL(), nil)
	require.NoError(t, err)

	stdinR, stdinW, stdoutBuf, stdoutW := testutil.StdioPair()

	done := make(chan error, 1)
	go func() {
		done <- bridge.Run(ctx, stdinR, stdoutW, conn, testLogger(t))
	}()

	// Let connection establish and verify echo works.
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"ping","id":1}`)
	require.True(t, waitForOutput(stdoutBuf, 1, 3*time.Second), "timed out waiting for echo")

	// Kill the daemon — the reader goroutine should detect the disconnect
	// and close stdin to unblock the scanner.
	d.Close()

	select {
	case <-done:
		// Bridge should exit (with or without error — the daemon went away).
		// Not asserting error vs nil because the exact behavior depends on
		// timing of httptest.Server.Close() vs conn.Read().
	case <-time.After(3 * time.Second):
		stdinW.Close() // unblock if stuck
		t.Fatal("bridge did not exit after daemon disconnect")
	}
}

func TestBridge_LargeMessage(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, d.URL(), nil)
	require.NoError(t, err)

	stdinR, stdinW, stdoutBuf, stdoutW := testutil.StdioPair()

	done := make(chan error, 1)
	go func() {
		done <- bridge.Run(ctx, stdinR, stdoutW, conn, testLogger(t))
	}()

	payload := strings.Repeat("x", 100*1024)
	msg := fmt.Sprintf(`{"jsonrpc":"2.0","method":"big","params":{"data":"%s"},"id":1}`, payload)
	fmt.Fprintln(stdinW, msg)
	require.True(t, waitForOutput(stdoutBuf, 1, 5*time.Second), "timed out waiting for large message response")
	stdinW.Close()

	err = <-done
	assert.NoError(t, err)
	assert.Equal(t, msg+"\n", stdoutBuf.String())
}

func TestBridge_MaxMessageSize(t *testing.T) {
	const maxMessageSize = 1024 * 1024 // 1MB, matches bridge.go and Z spec maxMessageSize

	tests := []struct {
		name      string
		size      int
		wantError bool
	}{
		{"exactly 1MB payload succeeds", maxMessageSize - 100, false}, // subtract JSON envelope overhead
		{"over 1MB payload fails", maxMessageSize + 1, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := testutil.NewMockDaemon()
			defer d.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			conn, _, err := websocket.Dial(ctx, d.URL(), nil)
			require.NoError(t, err)

			stdinR, stdinW, stdoutBuf, stdoutW := testutil.StdioPair()

			done := make(chan error, 1)
			go func() {
				done <- bridge.Run(ctx, stdinR, stdoutW, conn, testLogger(t))
			}()

			payload := strings.Repeat("x", tt.size)
			fmt.Fprintln(stdinW, payload)

			if tt.wantError {
				// The scanner should reject this line — it exceeds the buffer.
				// Bridge should exit with a scanner error or the write should fail.
				time.Sleep(200 * time.Millisecond)
				stdinW.Close()
				err = <-done
				// Scanner reports "token too long" for lines exceeding buffer size.
				assert.Error(t, err, "message exceeding 1MB should cause an error")
			} else {
				require.True(t, waitForOutput(stdoutBuf, 1, 5*time.Second), "timed out waiting for 1MB message response")
				stdinW.Close()
				err = <-done
				assert.NoError(t, err)
				assert.Equal(t, payload+"\n", stdoutBuf.String())
			}
		})
	}
}

func TestBridge_RapidFire(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, d.URL(), nil)
	require.NoError(t, err)

	stdinR, stdinW, stdoutBuf, stdoutW := testutil.StdioPair()

	done := make(chan error, 1)
	go func() {
		done <- bridge.Run(ctx, stdinR, stdoutW, conn, testLogger(t))
	}()

	const count = 1000
	for i := range count {
		fmt.Fprintf(stdinW, `{"jsonrpc":"2.0","method":"ping","id":%d}`+"\n", i+1)
	}
	require.True(t, waitForOutput(stdoutBuf, count, 15*time.Second), "timed out waiting for %d responses", count)
	stdinW.Close()

	err = <-done
	assert.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(stdoutBuf.String()), "\n")
	assert.Len(t, lines, count, "all %d messages should be received", count)
}

func TestBridge_EmptyLines(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, d.URL(), nil)
	require.NoError(t, err)

	stdinR, stdinW, stdoutBuf, stdoutW := testutil.StdioPair()

	done := make(chan error, 1)
	go func() {
		done <- bridge.Run(ctx, stdinR, stdoutW, conn, testLogger(t))
	}()

	fmt.Fprintln(stdinW, "")
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"ping","id":1}`)
	fmt.Fprintln(stdinW, "")
	fmt.Fprintln(stdinW, "")
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"ping","id":2}`)
	require.True(t, waitForOutput(stdoutBuf, 2, 3*time.Second), "timed out waiting for responses")
	stdinW.Close()

	err = <-done
	assert.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(stdoutBuf.String()), "\n")
	assert.Len(t, lines, 2, "empty lines should be skipped")
}
