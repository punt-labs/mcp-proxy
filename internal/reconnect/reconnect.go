// Package reconnect implements a reconnecting stdio-to-daemon bridge.
//
// Unlike bridge.Run which ties stdin reading to a single connection lifetime,
// reconnect decouples them: a single stdin goroutine feeds a channel for the
// process lifetime, while per-connection goroutines consume from it. When a
// connection drops, a new connection picks up from the channel without losing
// messages.
package reconnect

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/coder/websocket"
)

// Conn is the subset of *websocket.Conn used by the reconnect loop.
type Conn interface {
	Read(ctx context.Context) (websocket.MessageType, []byte, error)
	Write(ctx context.Context, typ websocket.MessageType, p []byte) error
	Ping(ctx context.Context) error
	CloseNow() error
}

// DialFunc connects to the daemon. Called on each reconnect attempt.
type DialFunc func(ctx context.Context) (Conn, error)

// Config holds tunable parameters for the reconnecting bridge.
type Config struct {
	// PingInterval is how often to send WebSocket pings to the daemon.
	// Zero disables pinging.
	PingInterval time.Duration

	// PongTimeout is how long to wait for a pong response before
	// declaring the daemon unresponsive and forcing a reconnect.
	PongTimeout time.Duration
}

// backoff schedule: 250ms, 500ms, 1s, 2s, 4s, 5s (cap).
var backoffSteps = []time.Duration{
	250 * time.Millisecond,
	500 * time.Millisecond,
	1 * time.Second,
	2 * time.Second,
	4 * time.Second,
	5 * time.Second,
}

func nextBackoff(attempt int) time.Duration {
	if attempt >= len(backoffSteps) {
		return backoffSteps[len(backoffSteps)-1]
	}
	return backoffSteps[attempt]
}

// Run bridges stdin/stdout to a daemon via the provided dial function, reconnecting
// on daemon disconnect with exponential backoff. It returns nil on stdin EOF, or
// an error if the context is cancelled.
func Run(ctx context.Context, stdin io.Reader, stdout io.Writer, dial DialFunc, logger *slog.Logger) error {
	return RunWithConfig(ctx, stdin, stdout, dial, Config{}, logger)
}

// RunWithConfig is like Run but accepts explicit configuration.
func RunWithConfig(ctx context.Context, stdin io.Reader, stdout io.Writer, dial DialFunc, cfg Config, logger *slog.Logger) error {
	lines := make(chan []byte, 64)

	// Stdin goroutine: lives for the process lifetime.
	// Reads lines, copies bytes, sends to channel. Closes channel on EOF.
	stdinDone := make(chan error, 1)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(stdin)
		scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024) // 1MB max line
		for scanner.Scan() {
			raw := scanner.Bytes()
			if len(raw) == 0 {
				continue
			}
			// Copy — scanner reuses the buffer.
			line := make([]byte, len(raw))
			copy(line, raw)

			select {
			case lines <- line:
			case <-ctx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil {
			stdinDone <- fmt.Errorf("reading stdin: %w", err)
		}
	}()

	var pending []byte
	var hs handshake
	attempt := 0
	connNum := 0

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		conn, err := dial(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			wait := nextBackoff(attempt)
			logger.Debug("dial failed, retrying", "error", err, "backoff", wait)
			fmt.Fprintf(os.Stderr, "mcp-proxy: daemon unreachable, retrying in %s...\n", wait)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return ctx.Err()
			}
			attempt++
			continue
		}

		// Connected — reset backoff and announce.
		attempt = 0
		connNum++
		fmt.Fprintln(os.Stderr, "mcp-proxy: connected")
		logger.Debug("connected", "connNum", connNum)

		isReconnect := connNum > 1
		pending, err = runConnection(ctx, conn, lines, stdinDone, stdout, pending, cfg, logger, &hs, isReconnect)
		conn.CloseNow()

		if err == nil {
			// stdin EOF — clean shutdown.
			logger.Debug("stdin EOF, shutting down")
			return nil
		}

		// Check if context was cancelled during the connection.
		if ctx.Err() != nil {
			return ctx.Err()
		}

		logger.Debug("daemon disconnected, reconnecting", "error", err)
		fmt.Fprintln(os.Stderr, "mcp-proxy: daemon disconnected, reconnecting...")
	}
}

// runConnection runs a single connection lifecycle. It returns:
//   - nil if stdin reached EOF (clean shutdown)
//   - an error if the daemon disconnected or the connection failed
//   - pending: any message consumed from lines but not successfully written
func runConnection(
	ctx context.Context,
	conn Conn,
	lines <-chan []byte,
	stdinDone <-chan error,
	stdout io.Writer,
	pending []byte,
	cfg Config,
	logger *slog.Logger,
	hs *handshake,
	isReconnect bool,
) ([]byte, error) {
	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()

	readerDone := make(chan error, 1)
	swallowCh := make(chan json.RawMessage, 1)

	// Ping goroutine: periodic liveness check.
	// Cancels connCtx if the daemon stops responding.
	if cfg.PingInterval > 0 && cfg.PongTimeout > 0 {
		go func() {
			ticker := time.NewTicker(cfg.PingInterval)
			defer ticker.Stop()
			for {
				select {
				case <-connCtx.Done():
					return
				case <-ticker.C:
					pingCtx, pingCancel := context.WithTimeout(connCtx, cfg.PongTimeout)
					err := conn.Ping(pingCtx)
					pingCancel()
					if err != nil {
						if connCtx.Err() != nil {
							return
						}
						logger.Debug("ping failed, forcing reconnect", "error", err)
						connCancel()
						return
					}
					logger.Debug("ping ok")
				}
			}
		}()
	}

	// Reader goroutine: daemon -> stdout.
	// Only this goroutine writes to stdout — no mutex needed.
	go func() {
		defer connCancel() // signal writer to stop on disconnect
		for {
			_, msg, err := conn.Read(connCtx)
			if err != nil {
				if connCtx.Err() != nil {
					readerDone <- nil
					return
				}
				readerDone <- fmt.Errorf("reading from daemon: %w", err)
				return
			}
			logger.Debug("daemon->stdout", "size", len(msg))

			// Check if we should swallow this response (replayed handshake).
			select {
			case swallowID := <-swallowCh:
				var env struct {
					ID json.RawMessage `json:"id"`
				}
				if json.Unmarshal(msg, &env) == nil && bytes.Equal(env.ID, swallowID) {
					logger.Debug("swallowed replayed initialize response", "id", string(swallowID))
					continue
				}
				// Not the one we're looking for — put the ID back.
				// Non-blocking: if connCancel() fired between our pop and this
				// put-back, a blocking send would deadlock (writer waits on
				// readerDone, reader waits on swallowCh).
				select {
				case swallowCh <- swallowID:
				default:
				}
			default:
			}

			_, writeErr := fmt.Fprintf(stdout, "%s\n", msg)
			if writeErr != nil {
				readerDone <- fmt.Errorf("writing to stdout: %w", writeErr)
				return
			}
		}
	}()

	// Replay handshake on reconnect, before retrying pending messages.
	if isReconnect && hs.cached() {
		swallowCh <- hs.initID
		if err := conn.Write(connCtx, websocket.MessageText, hs.initRequest); err != nil {
			// Drain swallowCh so reader doesn't block.
			select {
			case <-swallowCh:
			default:
			}
			connCancel()
			<-readerDone
			return pending, fmt.Errorf("writing replayed initialize: %w", err)
		}
		if hs.initialized != nil {
			if err := conn.Write(connCtx, websocket.MessageText, hs.initialized); err != nil {
				select {
				case <-swallowCh:
				default:
				}
				connCancel()
				<-readerDone
				return pending, fmt.Errorf("writing replayed initialized: %w", err)
			}
		}
		logger.Debug("handshake replayed")
	}

	// Writer: channel -> daemon. Runs in this goroutine.
	// First, retry any pending message from a previous failed write.
	if pending != nil {
		logger.Debug("retrying pending message", "size", len(pending))
		if err := conn.Write(connCtx, websocket.MessageText, pending); err != nil {
			connCancel()
			<-readerDone
			return pending, fmt.Errorf("writing pending to daemon: %w", err)
		}
		pending = nil
	}

	for {
		select {
		case line, ok := <-lines:
			if !ok {
				// Channel closed — stdin EOF.
				// Cancel conn context to unblock the reader goroutine,
				// then wait for it to exit.
				connCancel()
				<-readerDone
				// Drain stdin error if present. On clean EOF, nothing
				// was sent to stdinDone so the default branch fires.
				// On scanner error, the send happens-before close(lines),
				// so the buffered value is guaranteed visible here.
				select {
				case err := <-stdinDone:
					if err != nil {
						return nil, err
					}
				default:
				}
				return nil, nil
			}
			logger.Debug("stdin->daemon", "size", len(line))
			if err := conn.Write(connCtx, websocket.MessageText, line); err != nil {
				// Write failed — this line is pending for retry.
				connCancel()
				<-readerDone
				return line, fmt.Errorf("writing to daemon: %w", err)
			}
			// Sniff handshake frames so we can replay them on reconnect.
			if !hs.ready() {
				hs.sniff(line)
			}

		case <-connCtx.Done():
			// Reader detected disconnect or parent context cancelled.
			err := <-readerDone
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("connection lost: %w", connCtx.Err())
		}
	}
}
