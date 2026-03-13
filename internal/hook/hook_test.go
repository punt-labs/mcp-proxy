package hook_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/punt-labs/mcp-proxy/internal/debuglog"
	"github.com/punt-labs/mcp-proxy/internal/hook"
	"github.com/punt-labs/mcp-proxy/internal/testutil"
	"github.com/punt-labs/mcp-proxy/internal/transport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSyncRequestResponse(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	d.Handler = func(msg []byte) []byte {
		return []byte(`{"jsonrpc":"2.0","id":1,"result":{"additionalContext":"hello from daemon"}}`)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logger := debuglog.Nop()
	conn, err := transport.DialHook(ctx, d.HookURL(), 42, logger)
	require.NoError(t, err)
	conn.SetReadLimit(1024 * 1024)
	defer conn.CloseNow()

	stdin := strings.NewReader(`{"tool":"bash","input":"ls"}`)
	var stdout, stderr bytes.Buffer

	err = hook.Run(ctx, stdin, &stdout, &stderr, conn, "PreToolUse", false, logger)
	require.NoError(t, err)

	assert.Empty(t, stderr.String())
	assert.JSONEq(t, `{"additionalContext":"hello from daemon"}`, strings.TrimSpace(stdout.String()))
}

func TestSyncErrorResponse(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	d.Handler = func(msg []byte) []byte {
		return []byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"internal error"}}`)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logger := debuglog.Nop()
	conn, err := transport.DialHook(ctx, d.HookURL(), 42, logger)
	require.NoError(t, err)
	conn.SetReadLimit(1024 * 1024)
	defer conn.CloseNow()

	stdin := strings.NewReader(`{"event":"test"}`)
	var stdout, stderr bytes.Buffer

	err = hook.Run(ctx, stdin, &stdout, &stderr, conn, "PreToolUse", false, logger)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "daemon returned error")

	assert.Empty(t, stdout.String())
	assert.Contains(t, stderr.String(), "internal error")
}

func TestAsyncFireAndForget(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logger := debuglog.Nop()
	conn, err := transport.DialHook(ctx, d.HookURL(), 42, logger)
	require.NoError(t, err)
	conn.SetReadLimit(1024 * 1024)
	// No defer CloseNow — sendNotification does a graceful close.

	stdin := strings.NewReader(`{"session":"abc123"}`)
	var stdout, stderr bytes.Buffer

	err = hook.Run(ctx, stdin, &stdout, &stderr, conn, "SessionEnd", true, logger)
	require.NoError(t, err)

	assert.Empty(t, stdout.String())
	assert.Empty(t, stderr.String())

	// Wait for daemon to process (no fixed sleep — poll for robustness).
	require.Eventually(t, func() bool {
		return len(d.Received()) == 1
	}, 2*time.Second, 10*time.Millisecond, "daemon should receive notification")

	received := d.Received()

	var envelope struct {
		Method string          `json:"method"`
		ID     json.RawMessage `json:"id"`
		Params json.RawMessage `json:"params"`
	}
	require.NoError(t, json.Unmarshal(received[0], &envelope))
	assert.Equal(t, "hook/SessionEnd", envelope.Method)
	assert.Nil(t, envelope.ID, "notification should not have id")
	assert.JSONEq(t, `{"session":"abc123"}`, string(envelope.Params))
}

func TestStdinPayloadPassthrough(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	d.Handler = func(msg []byte) []byte {
		// Echo back the received params as the result.
		var env struct {
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(msg, &env); err != nil {
			return []byte(`{"jsonrpc":"2.0","id":1,"error":{"message":"parse error"}}`)
		}
		resp, _ := json.Marshal(map[string]json.RawMessage{
			"jsonrpc": json.RawMessage(`"2.0"`),
			"id":      json.RawMessage(`1`),
			"result":  env.Params,
		})
		return resp
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logger := debuglog.Nop()
	conn, err := transport.DialHook(ctx, d.HookURL(), 42, logger)
	require.NoError(t, err)
	conn.SetReadLimit(1024 * 1024)
	defer conn.CloseNow()

	payload := `{"tool":"bash","input":{"command":"echo hello"}}`
	stdin := strings.NewReader(payload)
	var stdout, stderr bytes.Buffer

	err = hook.Run(ctx, stdin, &stdout, &stderr, conn, "PreToolUse", false, logger)
	require.NoError(t, err)

	assert.JSONEq(t, payload, strings.TrimSpace(stdout.String()))
}

func TestEmptyStdin(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	d.Handler = func(msg []byte) []byte {
		return []byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logger := debuglog.Nop()
	conn, err := transport.DialHook(ctx, d.HookURL(), 42, logger)
	require.NoError(t, err)
	conn.SetReadLimit(1024 * 1024)
	defer conn.CloseNow()

	stdin := strings.NewReader("")
	var stdout, stderr bytes.Buffer

	err = hook.Run(ctx, stdin, &stdout, &stderr, conn, "SessionStart", false, logger)
	require.NoError(t, err)

	// Verify the daemon received null params.
	received := d.Received()
	require.Len(t, received, 1)

	var envelope struct {
		Params json.RawMessage `json:"params"`
	}
	require.NoError(t, json.Unmarshal(received[0], &envelope))
	assert.Equal(t, "null", string(envelope.Params))
}

func TestEventNamePassthrough(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	d.Handler = func(msg []byte) []byte {
		return []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)
	}

	events := []string{"PreToolUse", "PostToolUse", "SessionStart", "Stop", "UserPromptSubmit"}

	for _, event := range events {
		t.Run(event, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			logger := debuglog.Nop()
			conn, err := transport.DialHook(ctx, d.HookURL(), 42, logger)
			require.NoError(t, err)
			conn.SetReadLimit(1024 * 1024)
			defer conn.CloseNow()

			stdin := strings.NewReader(`{}`)
			var stdout, stderr bytes.Buffer

			err = hook.Run(ctx, stdin, &stdout, &stderr, conn, event, false, logger)
			require.NoError(t, err)

			received := d.Received()
			require.NotEmpty(t, received)

			// Check the last received message has the right method.
			last := received[len(received)-1]
			var envelope struct {
				Method string `json:"method"`
			}
			require.NoError(t, json.Unmarshal(last, &envelope))
			assert.Equal(t, "hook/"+event, envelope.Method)
		})
	}
}

func TestLargePayload(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	d.Handler = func(msg []byte) []byte {
		return []byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	logger := debuglog.Nop()
	conn, err := transport.DialHook(ctx, d.HookURL(), 42, logger)
	require.NoError(t, err)
	conn.SetReadLimit(1024 * 1024)
	defer conn.CloseNow()

	// Generate a ~900KB JSON payload (under 1MB limit).
	bigValue := strings.Repeat("x", 900*1024)
	payload := `{"data":"` + bigValue + `"}`
	stdin := strings.NewReader(payload)
	var stdout, stderr bytes.Buffer

	err = hook.Run(ctx, stdin, &stdout, &stderr, conn, "PreToolUse", false, logger)
	require.NoError(t, err)

	// Verify the daemon received the full payload.
	received := d.Received()
	require.Len(t, received, 1)
	assert.Contains(t, string(received[0]), bigValue[:100])
}

func TestAsyncDeliveryGuarantee(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logger := debuglog.Nop()
	conn, err := transport.DialHook(ctx, d.HookURL(), 42, logger)
	require.NoError(t, err)
	conn.SetReadLimit(1024 * 1024)

	payload := `{"important":"data"}`
	stdin := strings.NewReader(payload)
	var stdout, stderr bytes.Buffer

	// Run returns immediately after graceful close.
	err = hook.Run(ctx, stdin, &stdout, &stderr, conn, "Notification", true, logger)
	require.NoError(t, err)

	// Despite the proxy closing immediately, the daemon should have the message.
	require.Eventually(t, func() bool {
		return len(d.Received()) == 1
	}, 2*time.Second, 10*time.Millisecond, "async notification must be delivered despite immediate close")

	received := d.Received()

	var envelope struct {
		Params json.RawMessage `json:"params"`
	}
	require.NoError(t, json.Unmarshal(received[0], &envelope))
	assert.JSONEq(t, payload, string(envelope.Params))
}

// TestNoEOFStdinDoesNotHang is a regression test for the biff DES-027 bug.
// Claude Code pipes hook payloads to stdin but doesn't always close the pipe
// promptly. With io.ReadAll this would hang forever; with deadline-based reads
// it completes in ~150ms.
func TestNoEOFStdinDoesNotHang(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	d.Handler = func(msg []byte) []byte {
		return []byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logger := debuglog.Nop()
	conn, err := transport.DialHook(ctx, d.HookURL(), 42, logger)
	require.NoError(t, err)
	conn.SetReadLimit(1024 * 1024)
	defer conn.CloseNow()

	// os.Pipe gives *os.File which supports SetReadDeadline.
	// Write data but DON'T close the write end — simulates Claude Code
	// not closing stdin promptly (the biff DES-027 bug condition).
	r, w, err := os.Pipe()
	require.NoError(t, err)
	defer r.Close()
	defer w.Close()

	_, err = w.Write([]byte(`{"event":"test"}`))
	require.NoError(t, err)
	// Deliberately not closing w — this is the bug condition.

	var stdout, stderr bytes.Buffer

	start := time.Now()
	err = hook.Run(ctx, r, &stdout, &stderr, conn, "PreToolUse", false, logger)
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Less(t, elapsed, 2*time.Second, "should not hang waiting for EOF")
	assert.JSONEq(t, `{"ok":true}`, strings.TrimSpace(stdout.String()))
}

// TestEmptyStdinNoEOF verifies that when stdin has no data AND no EOF
// (open pipe, nothing written), the proxy returns empty params quickly.
func TestEmptyStdinNoEOF(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	d.Handler = func(msg []byte) []byte {
		return []byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logger := debuglog.Nop()
	conn, err := transport.DialHook(ctx, d.HookURL(), 42, logger)
	require.NoError(t, err)
	conn.SetReadLimit(1024 * 1024)
	defer conn.CloseNow()

	// Open pipe, no data written, no EOF.
	r, w, err := os.Pipe()
	require.NoError(t, err)
	defer r.Close()
	defer w.Close()

	var stdout, stderr bytes.Buffer

	start := time.Now()
	err = hook.Run(ctx, r, &stdout, &stderr, conn, "SessionStart", false, logger)
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Less(t, elapsed, 2*time.Second, "should not hang on empty stdin without EOF")

	// Verify daemon received null params.
	received := d.Received()
	require.NotEmpty(t, received)
	last := received[len(received)-1]
	var envelope struct {
		Params json.RawMessage `json:"params"`
	}
	require.NoError(t, json.Unmarshal(last, &envelope))
	assert.Equal(t, "null", string(envelope.Params))
}
