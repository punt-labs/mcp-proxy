package transport_test

import (
	"context"
	"testing"
	"time"

	"github.com/punt-labs/mcp-proxy/internal/debuglog"
	"github.com/punt-labs/mcp-proxy/internal/testutil"
	"github.com/punt-labs/mcp-proxy/internal/transport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/coder/websocket"
)

func TestDial_Success(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logger := debuglog.NewTestLogger(t).Logger
	conn, err := transport.Dial(ctx, d.URL(), 12345, nil, "", logger)
	require.NoError(t, err)
	defer conn.Close(websocket.StatusNormalClosure, "")

	assert.Equal(t, "12345", d.SessionKey())
}

func TestDial_BearerToken(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	t.Setenv("MCP_PROXY_TOKEN", "test-secret-key")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logger := debuglog.NewTestLogger(t).Logger
	conn, err := transport.Dial(ctx, d.URL(), 12345, nil, "", logger)
	require.NoError(t, err)
	defer conn.Close(websocket.StatusNormalClosure, "")

	assert.Equal(t, "Bearer test-secret-key", d.AuthHeader())
}

func TestDial_NoBearerTokenByDefault(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logger := debuglog.NewTestLogger(t).Logger
	conn, err := transport.Dial(ctx, d.URL(), 12345, nil, "", logger)
	require.NoError(t, err)
	defer conn.Close(websocket.StatusNormalClosure, "")

	assert.Empty(t, d.AuthHeader())
}

func TestDial_InvalidURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{"bad scheme", "http://localhost:8080/mcp"},
		{"no scheme", "localhost:8080/mcp"},
		{"empty host", "ws:///mcp"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			logger := debuglog.NewTestLogger(t).Logger
			_, err := transport.Dial(ctx, tt.url, 1, nil, "", logger)
			var urlErr *transport.InvalidURLError
			assert.ErrorAs(t, err, &urlErr)
		})
	}
}

func TestDial_ConnectionRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logger := debuglog.NewTestLogger(t).Logger
	// Port 1 is almost certainly not listening.
	_, err := transport.Dial(ctx, "ws://127.0.0.1:1/mcp", 1, nil, "", logger)
	require.Error(t, err)

	// Should be either ConnectionRefused or a generic dial error.
	// The exact error depends on OS behavior for port 1.
	var connErr *transport.ConnectionRefusedError
	var timeErr *transport.TimeoutError
	isExpected := assert.ErrorAs(t, err, &connErr) || assert.ErrorAs(t, err, &timeErr) || err != nil
	assert.True(t, isExpected, "expected a connection error, got: %v", err)
}

func TestDial_ExtraHeaders(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logger := debuglog.NewTestLogger(t).Logger
	extra := map[string]string{
		"X-Custom-Header": "my-value",
	}
	conn, err := transport.Dial(ctx, d.URL(), 1, extra, "", logger)
	require.NoError(t, err)
	defer conn.Close(websocket.StatusNormalClosure, "")

	assert.Equal(t, "my-value", d.Header("X-Custom-Header"))
}

func TestDial_ExtraHeaders_OverrideToken(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	t.Setenv("MCP_PROXY_TOKEN", "env-token")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logger := debuglog.NewTestLogger(t).Logger
	extra := map[string]string{
		"Authorization": "Bearer config-token",
	}
	conn, err := transport.Dial(ctx, d.URL(), 1, extra, "", logger)
	require.NoError(t, err)
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Config header takes precedence over MCP_PROXY_TOKEN.
	assert.Equal(t, "Bearer config-token", d.AuthHeader())
}

func TestDial_Timeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	logger := debuglog.NewTestLogger(t).Logger
	// Non-routable address to trigger timeout.
	_, err := transport.Dial(ctx, "ws://192.0.2.1:9999/mcp", 1, nil, "", logger)
	require.Error(t, err)
}
