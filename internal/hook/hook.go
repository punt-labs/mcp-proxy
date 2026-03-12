// Package hook implements one-shot JSON-RPC relay for Claude Code hook scripts.
//
// Hook relay reads a single payload from stdin, wraps it in a JSON-RPC envelope,
// sends it to the daemon over WebSocket, and either waits for a response (sync)
// or exits immediately (async). This is a third execution mode alongside the
// long-running MCP bridge and the health check.
package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"

	"nhooyr.io/websocket"
)

// ResponseTimeout is the safety-net timeout for waiting for a daemon response
// after the request is sent. The hook framework enforces the real budget by
// killing the process — this just prevents silent hangs.
const ResponseTimeout = 30 // seconds (used by caller to set context deadline)

// request is a JSON-RPC 2.0 request or notification envelope.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	ID      *int            `json:"id,omitempty"`
	Params  json.RawMessage `json:"params"`
}

// response is the subset of a JSON-RPC 2.0 response the proxy inspects.
// Only top-level fields are examined — result/error contents are opaque.
type response struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

// Run executes a one-shot hook relay. It reads all of stdin, wraps the payload
// in a JSON-RPC envelope with method "hook/<event>", and sends it to the daemon.
//
// For sync hooks (async=false): sends a request (with id), reads messages until
// one with matching id, prints result to stdout or error to stderr.
//
// For async hooks (async=true): sends a notification (no id), performs a graceful
// WebSocket close to guarantee delivery, and returns.
func Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, conn *websocket.Conn, event string, async bool, logger *slog.Logger) error {
	payload, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}

	method := "hook/" + event

	// Use trimmed payload as params. Empty stdin becomes JSON null.
	var params json.RawMessage
	if trimmed := bytes.TrimSpace(payload); len(trimmed) > 0 {
		params = json.RawMessage(trimmed)
	}

	if async {
		return sendNotification(ctx, conn, method, params, logger)
	}
	return sendRequest(ctx, conn, method, params, stdout, stderr, logger)
}

// sendNotification sends a JSON-RPC notification (no id, no response expected)
// and performs a graceful WebSocket close to guarantee the frame is delivered.
func sendNotification(ctx context.Context, conn *websocket.Conn, method string, params json.RawMessage, logger *slog.Logger) error {
	req := request{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}
	msg, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshaling notification: %w", err)
	}

	logger.Debug("sending async notification", "method", method, "size", len(msg))

	if err := conn.Write(ctx, websocket.MessageText, msg); err != nil {
		return fmt.Errorf("sending notification: %w", err)
	}

	// Graceful close: send Close frame and wait for echo (RFC 6455 §7.1.2).
	// This adds ~1ms on localhost but guarantees the notification is delivered.
	// Do NOT use CloseNow() — a TCP RST can race the notification frame.
	return conn.Close(websocket.StatusNormalClosure, "")
}

// sendRequest sends a JSON-RPC request (with id:1), reads messages until one
// with matching id is found, and routes the result to stdout or error to stderr.
func sendRequest(ctx context.Context, conn *websocket.Conn, method string, params json.RawMessage, stdout, stderr io.Writer, logger *slog.Logger) error {
	id := 1
	req := request{
		JSONRPC: "2.0",
		Method:  method,
		ID:      &id,
		Params:  params,
	}
	msg, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshaling request: %w", err)
	}

	logger.Debug("sending sync request", "method", method, "size", len(msg))

	if err := conn.Write(ctx, websocket.MessageText, msg); err != nil {
		return fmt.Errorf("sending request: %w", err)
	}

	// Read messages until we find one with matching id.
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("reading response: %w", err)
		}

		var resp response
		if err := json.Unmarshal(data, &resp); err != nil {
			logger.Debug("ignoring unparseable message", "error", err)
			continue
		}

		// Match on id. We sent 1, expect "1" back (JSON numeric literal).
		if string(resp.ID) != "1" {
			logger.Debug("ignoring message with non-matching id", "id", string(resp.ID))
			continue
		}

		// Error response: print error to stderr, return error for exit code 1.
		if len(resp.Error) > 0 && string(resp.Error) != "null" {
			fmt.Fprintf(stderr, "%s\n", resp.Error)
			return fmt.Errorf("daemon returned error")
		}

		// Success response: print result to stdout.
		if len(resp.Result) > 0 && string(resp.Result) != "null" {
			fmt.Fprintf(stdout, "%s\n", resp.Result)
		}

		logger.Debug("received response", "size", len(resp.Result))
		return nil
	}
}
