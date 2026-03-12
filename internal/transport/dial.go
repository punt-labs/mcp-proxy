// Package transport handles connecting to the daemon process.
package transport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"time"

	"nhooyr.io/websocket"
)

const dialTimeout = 5 * time.Second

// ConnectionRefusedError indicates the daemon is not listening.
type ConnectionRefusedError struct {
	Addr string
}

func (e *ConnectionRefusedError) Error() string {
	return fmt.Sprintf("connection refused: %s", e.Addr)
}

// TimeoutError indicates the connection attempt timed out.
type TimeoutError struct {
	Addr string
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("connection timed out: %s", e.Addr)
}

// InvalidURLError indicates the daemon URL is malformed.
type InvalidURLError struct {
	URL string
	Err error
}

func (e *InvalidURLError) Error() string {
	return fmt.Sprintf("invalid URL %q: %v", e.URL, e.Err)
}

func (e *InvalidURLError) Unwrap() error {
	return e.Err
}

// Dial connects to the daemon at rawURL, passing sessionKey as a query parameter.
// Returns the WebSocket connection or a typed error.
func Dial(ctx context.Context, rawURL string, sessionKey int, logger *slog.Logger) (*websocket.Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, &InvalidURLError{URL: rawURL, Err: err}
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return nil, &InvalidURLError{URL: rawURL, Err: fmt.Errorf("scheme must be ws or wss, got %q", u.Scheme)}
	}
	if u.Host == "" {
		return nil, &InvalidURLError{URL: rawURL, Err: fmt.Errorf("missing host")}
	}

	// Append session key to query parameters.
	q := u.Query()
	q.Set("session_key", fmt.Sprintf("%d", sessionKey))
	u.RawQuery = q.Encode()

	logger.Debug("dialing daemon", "host", u.Host, "session_key", sessionKey)

	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	conn, _, err := websocket.Dial(dialCtx, u.String(), nil)
	if err != nil {
		addr := u.Host

		var netErr *net.OpError
		if errors.As(err, &netErr) {
			if netErr.Op == "dial" {
				var dnsErr *net.DNSError
				if errors.As(netErr.Err, &dnsErr) || isConnectionRefused(netErr.Err) {
					logger.Debug("dial failed", "reason", "connection refused", "addr", addr)
					return nil, &ConnectionRefusedError{Addr: addr}
				}
			}
		}

		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			logger.Debug("dial failed", "reason", "timeout", "addr", addr)
			return nil, &TimeoutError{Addr: addr}
		}

		// Connection refused shows up in different ways depending on platform.
		if isConnectionRefused(err) {
			logger.Debug("dial failed", "reason", "connection refused", "addr", addr)
			return nil, &ConnectionRefusedError{Addr: addr}
		}

		logger.Debug("dial failed", "reason", "unknown", "addr", addr, "error", err)
		return nil, fmt.Errorf("dialing %s: %w", addr, err)
	}

	logger.Debug("connected", "host", u.Host)
	return conn, nil
}

func isConnectionRefused(err error) bool {
	if err == nil {
		return false
	}
	// Check for syscall-level connection refused.
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return isConnectionRefused(opErr.Err)
	}
	return strings.Contains(err.Error(), "connection refused")
}

func isTimeout(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return false
}
