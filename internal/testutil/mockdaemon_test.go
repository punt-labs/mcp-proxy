package testutil_test

import (
	"context"
	"testing"
	"time"

	"github.com/punt-labs/mcp-proxy/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"nhooyr.io/websocket"
)

func TestMockDaemon_Echo(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, d.URL()+"?session_key=12345", nil)
	require.NoError(t, err)
	defer conn.Close(websocket.StatusNormalClosure, "")

	msg := []byte(`{"jsonrpc":"2.0","method":"ping","id":1}`)
	err = conn.Write(ctx, websocket.MessageText, msg)
	require.NoError(t, err)

	_, resp, err := conn.Read(ctx)
	require.NoError(t, err)
	assert.Equal(t, msg, resp)

	assert.Equal(t, "12345", d.SessionKey())
	assert.True(t, d.Connected())
}

func TestMockDaemon_CustomHandler(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	d.Handler = func(msg []byte) []byte {
		return []byte(`{"jsonrpc":"2.0","result":"pong","id":1}`)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, d.URL(), nil)
	require.NoError(t, err)
	defer conn.Close(websocket.StatusNormalClosure, "")

	err = conn.Write(ctx, websocket.MessageText, []byte(`{"jsonrpc":"2.0","method":"ping","id":1}`))
	require.NoError(t, err)

	_, resp, err := conn.Read(ctx)
	require.NoError(t, err)
	assert.JSONEq(t, `{"jsonrpc":"2.0","result":"pong","id":1}`, string(resp))
}

func TestMockDaemon_Push(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, d.URL(), nil)
	require.NoError(t, err)
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Send a message first to ensure the connection is established and the push goroutine is running.
	err = conn.Write(ctx, websocket.MessageText, []byte(`{"jsonrpc":"2.0","method":"ping","id":1}`))
	require.NoError(t, err)
	_, _, err = conn.Read(ctx) // consume echo
	require.NoError(t, err)

	push := []byte(`{"jsonrpc":"2.0","method":"tools/list_changed"}`)
	d.Push <- push

	_, resp, err := conn.Read(ctx)
	require.NoError(t, err)
	assert.Equal(t, push, resp)
}

func TestMockDaemon_Disconnect(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, d.URL(), nil)
	require.NoError(t, err)

	conn.Close(websocket.StatusNormalClosure, "bye")

	// Give the server time to notice the disconnect.
	time.Sleep(100 * time.Millisecond)
	assert.True(t, d.Disconnected())
}

func TestMockDaemon_ReceivedMessages(t *testing.T) {
	d := testutil.NewMockDaemon()
	defer d.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, d.URL(), nil)
	require.NoError(t, err)
	defer conn.Close(websocket.StatusNormalClosure, "")

	messages := []string{
		`{"jsonrpc":"2.0","method":"a","id":1}`,
		`{"jsonrpc":"2.0","method":"b","id":2}`,
		`{"jsonrpc":"2.0","method":"c","id":3}`,
	}

	for _, m := range messages {
		err = conn.Write(ctx, websocket.MessageText, []byte(m))
		require.NoError(t, err)
		_, _, err = conn.Read(ctx) // consume echo
		require.NoError(t, err)
	}

	received := d.Received()
	require.Len(t, received, 3)
	for i, m := range messages {
		assert.Equal(t, m, string(received[i]))
	}
}
