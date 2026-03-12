package bridge_test

// Partition tests derived from Z specification (docs/mcp-proxy.tex) using TTF tactics.
// These cover gaps identified by /z-spec:partition — partitions not already exercised
// by bridge_test.go.

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/punt-labs/mcp-proxy/internal/bridge"
	"github.com/punt-labs/mcp-proxy/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"nhooyr.io/websocket"
)

// Partition 10: Shutdown via context cancellation (models causeSignal).
// The Z spec says cause? = causeSignal → exitCode' = 1.
// In Go, SIGINT/SIGTERM cancel the context passed to bridge.Run.
func TestPartition10_ShutdownViaContextCancel(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn, _, err := websocket.Dial(ctx, d.URL(), nil)
	require.NoError(t, err)

	stdinR, stdinW, stdoutBuf, stdoutW := testutil.StdioPair()
	defer stdinW.Close()

	done := make(chan error, 1)
	go func() {
		done <- bridge.Run(ctx, stdinR, stdoutW, conn, testLogger(t))
	}()

	// Verify bridge is live.
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"ping","id":1}`)
	require.True(t, waitForOutput(stdoutBuf, 1, 3*time.Second), "bridge should be forwarding")

	// Cancel context (simulates signal delivery).
	cancel()

	select {
	case err = <-done:
		// Bridge should exit. Context cancellation is not a clean EOF,
		// so the bridge may return an error (daemon read fails) or nil
		// (scanner exits on closed stdin). Either is acceptable — the
		// important thing is it exits promptly.
	case <-time.After(3 * time.Second):
		t.Fatal("bridge did not exit after context cancellation")
	}
}

// Partition 11: Shutdown via broken stdout pipe (models causeError).
// The Z spec says cause? = causeError → exitCode' = 1.
// In Go, a broken stdout pipe causes the reader goroutine's write to fail.
func TestPartition11_ShutdownViaBrokenStdout(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, d.URL(), nil)
	require.NoError(t, err)

	stdinR, stdinW, _, _ := testutil.StdioPair()
	defer stdinW.Close()

	// Use a pipe for stdout so we can break it.
	stdoutR, stdoutW := io.Pipe()
	// Drain stdout reader to avoid blocking.
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := stdoutR.Read(buf); err != nil {
				return
			}
		}
	}()

	done := make(chan error, 1)
	go func() {
		done <- bridge.Run(ctx, stdinR, stdoutW, conn, testLogger(t))
	}()

	// Let connection establish.
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"ping","id":1}`)
	time.Sleep(100 * time.Millisecond)

	// Break the stdout pipe — the reader goroutine's Fprintf will fail.
	stdoutW.Close()

	// Send another message so the daemon echoes and the reader tries to write.
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"ping","id":2}`)

	select {
	case err = <-done:
		assert.Error(t, err, "broken stdout should cause bridge error")
		assert.Contains(t, err.Error(), "writing to stdout")
	case <-time.After(3 * time.Second):
		t.Fatal("bridge did not exit after broken stdout")
	}
}

// Partition 17: No forwarding after stdin EOF.
// The Z spec rejects ForwardMessage when stdinStatus ≠ chanActive.
// In Go, once stdin EOF is reached, the scanner goroutine exits and
// no further messages are forwarded.
func TestPartition17_NoForwardAfterStdinEOF(t *testing.T) {
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

	// Send one message, verify echo.
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"ping","id":1}`)
	require.True(t, waitForOutput(stdoutBuf, 1, 3*time.Second))

	// Close stdin — bridge should exit.
	stdinW.Close()
	err = <-done
	assert.NoError(t, err)

	// After Run returns, the daemon should have received exactly 1 message.
	received := d.Received()
	assert.Len(t, received, 1, "no messages should be forwarded after stdin EOF")
}

// Partition 18: No forwarding after daemon disconnect.
// The Z spec rejects ForwardMessage when daemonStatus ≠ chanActive.
// In Go, once the daemon disconnects, the reader goroutine cancels
// the context, which causes the scanner's conn.Write to fail.
func TestPartition18_NoForwardAfterDaemonDisconnect(t *testing.T) {
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

	// Verify bridge is live.
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"ping","id":1}`)
	require.True(t, waitForOutput(stdoutBuf, 1, 3*time.Second))

	countBefore := len(d.Received())

	// Kill the daemon.
	d.Close()

	// Bridge should exit.
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		stdinW.Close()
		t.Fatal("bridge did not exit after daemon disconnect")
	}

	// After bridge exits, no further messages were forwarded.
	// (We can't send more because the daemon is gone, but we verify
	// the count didn't increase after disconnect.)
	assert.Equal(t, countBefore, len(d.Received()),
		"no messages should be forwarded after daemon disconnect")
	stdinW.Close()
}

// Partition 22: No receive after daemon disconnect.
// The Z spec rejects ReceiveMessage when daemonStatus ≠ chanActive.
// In Go, once the daemon disconnects, conn.Read returns an error
// and the reader goroutine exits — no further messages reach stdout.
func TestPartition22_NoReceiveAfterDaemonDisconnect(t *testing.T) {
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

	// Verify bridge is live.
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"ping","id":1}`)
	require.True(t, waitForOutput(stdoutBuf, 1, 3*time.Second))

	outputBefore := stdoutBuf.String()

	// Kill the daemon.
	d.Close()

	// Bridge should exit.
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		stdinW.Close()
		t.Fatal("bridge did not exit after daemon disconnect")
	}

	// No new output appeared on stdout after daemon disconnect.
	assert.Equal(t, outputBefore, stdoutBuf.String(),
		"no messages should be received after daemon disconnect")
	stdinW.Close()
}

// Hardening: Partial stdin line followed by EOF.
// bufio.Scanner returns the last line even without a trailing newline.
// The bridge should forward it to the daemon.
func TestHardening_PartialLineBeforeEOF(t *testing.T) {
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

	// Write a complete line, verify echo.
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"ping","id":1}`)
	require.True(t, waitForOutput(stdoutBuf, 1, 3*time.Second))

	// Write partial line without trailing newline, then close stdin.
	// bufio.Scanner returns this as a valid token on EOF.
	fmt.Fprint(stdinW, `{"jsonrpc":"2.0","method":"ping","id":2}`)
	stdinW.Close()

	err = <-done
	assert.NoError(t, err)

	// The bridge writes the partial line before cancelling its context.
	// The daemon may not have processed the read yet — give it a moment.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(d.Received()) >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	received := d.Received()
	require.Len(t, received, 2, "partial line before EOF should still be forwarded to daemon")
	assert.Equal(t, `{"jsonrpc":"2.0","method":"ping","id":2}`, string(received[1]))
}

// Hardening: Bridge exits promptly when daemon is unreachable.
// Verifies the "every failure path exits within 5 seconds" requirement.
func TestHardening_ExitTimeOnDaemonDisconnect(t *testing.T) {
	d := testutil.NewMockDaemon()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, d.URL(), nil)
	require.NoError(t, err)

	stdinR, stdinW, stdoutBuf, stdoutW := testutil.StdioPair()
	defer stdinW.Close()

	done := make(chan error, 1)
	go func() {
		done <- bridge.Run(ctx, stdinR, stdoutW, conn, testLogger(t))
	}()

	// Verify bridge is live.
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"ping","id":1}`)
	require.True(t, waitForOutput(stdoutBuf, 1, 3*time.Second))

	// Kill daemon and measure exit time.
	start := time.Now()
	d.Close()

	select {
	case <-done:
		elapsed := time.Since(start)
		assert.Less(t, elapsed, 5*time.Second, "bridge should exit within 5 seconds of daemon disconnect")
	case <-time.After(5 * time.Second):
		t.Fatal("bridge did not exit within 5 seconds of daemon disconnect")
	}
}

// Partition 21 (strengthened): Daemon→stdout message ordering.
// The Z spec says daemonQueue' = daemonQueue ⁀ ⟨msg?⟩ — append preserves order.
// Existing TestBridge_MultipleMessages tests stdin→daemon ordering via echo.
// This test directly verifies daemon→stdout ordering via push messages.
func TestPartition21_DaemonMessageOrdering(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()
	d.Handler = func([]byte) []byte { return nil } // no echo

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, d.URL(), nil)
	require.NoError(t, err)

	stdinR, stdinW, stdoutBuf, stdoutW := testutil.StdioPair()

	done := make(chan error, 1)
	go func() {
		done <- bridge.Run(ctx, stdinR, stdoutW, conn, testLogger(t))
	}()

	// Send a message to ensure connection is established.
	fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"init","id":1}`)
	time.Sleep(50 * time.Millisecond)

	// Push 5 ordered messages from daemon.
	for i := range 5 {
		d.Push <- []byte(fmt.Sprintf(`{"seq":%d}`, i))
	}
	require.True(t, waitForOutput(stdoutBuf, 5, 3*time.Second), "timed out waiting for push messages")

	stdinW.Close()
	<-done

	lines := strings.Split(strings.TrimSpace(stdoutBuf.String()), "\n")
	require.Len(t, lines, 5)
	for i, line := range lines {
		assert.Equal(t, fmt.Sprintf(`{"seq":%d}`, i), line,
			"daemon messages must arrive in order")
	}
}
