// Package bridge implements bidirectional forwarding between stdio and a WebSocket connection.
//
// The production code path uses [reconnect.Run] (which manages connection lifecycle
// and reconnection). This package remains as the single-connection primitive used by
// unit tests and partition tests to verify forwarding correctness in isolation.
package bridge

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"nhooyr.io/websocket"
)

// Run bridges stdin/stdout to a WebSocket connection. It forwards lines from
// stdin as WebSocket text messages and writes received WebSocket messages as
// newline-terminated lines to stdout.
//
// Run blocks until stdin reaches EOF (clean shutdown), the WebSocket connection
// closes, or the context is cancelled. It returns nil on clean stdin EOF shutdown.
func Run(ctx context.Context, stdin io.Reader, stdout io.Writer, conn *websocket.Conn, logger *slog.Logger) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer conn.CloseNow()

	// MCP messages can be large (tool responses with embedded data).
	// Default read limit in nhooyr.io/websocket is 32KB.
	conn.SetReadLimit(1024 * 1024) // 1MB, matches scanner buffer

	logger.Debug("bridge started")

	var (
		stdinErr  error
		daemonErr error
		stdinDone bool
		wg        sync.WaitGroup
		mu        sync.Mutex // protects stdinErr, daemonErr, stdinDone
	)

	// Stdout writes must be serialized.
	var stdoutMu sync.Mutex

	// stdin -> WebSocket: scan lines, send each as a text message.
	// On EOF, cancel context — stdin closed means the caller (Claude Code)
	// is done and the process should exit.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()

		scanner := bufio.NewScanner(stdin)
		scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024) // 1MB max line

		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			logger.Debug("stdin→daemon", "size", len(line))
			if err := conn.Write(ctx, websocket.MessageText, line); err != nil {
				if ctx.Err() != nil {
					return
				}
				mu.Lock()
				stdinErr = fmt.Errorf("writing to daemon: %w", err)
				mu.Unlock()
				logger.Debug("stdin→daemon write failed", "error", err)
				return
			}
		}
		if err := scanner.Err(); err != nil {
			if ctx.Err() != nil {
				return
			}
			mu.Lock()
			stdinErr = fmt.Errorf("reading stdin: %w", err)
			mu.Unlock()
			logger.Debug("stdin scanner error", "error", err)
			return
		}

		mu.Lock()
		stdinDone = true
		mu.Unlock()
		logger.Debug("stdin EOF")
	}()

	// WebSocket -> stdout: read messages, write as newline-terminated lines.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			cancel()
			// Unblock the scanner if it's stuck on stdin.Read().
			if closer, ok := stdin.(io.Closer); ok {
				closer.Close()
			}
		}()

		for {
			_, msg, err := conn.Read(ctx)
			if err != nil {
				if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
					logger.Debug("daemon closed normally")
					return
				}
				if ctx.Err() != nil {
					return
				}
				mu.Lock()
				daemonErr = fmt.Errorf("reading from daemon: %w", err)
				mu.Unlock()
				logger.Debug("daemon read failed", "error", err)
				return
			}

			logger.Debug("daemon→stdout", "size", len(msg))

			stdoutMu.Lock()
			_, writeErr := fmt.Fprintf(stdout, "%s\n", msg)
			stdoutMu.Unlock()

			if writeErr != nil {
				mu.Lock()
				daemonErr = fmt.Errorf("writing to stdout: %w", writeErr)
				mu.Unlock()
				logger.Debug("stdout write failed", "error", writeErr)
				return
			}
		}
	}()

	wg.Wait()

	mu.Lock()
	defer mu.Unlock()

	if stdinDone && daemonErr == nil {
		logger.Debug("bridge stopped", "cause", "stdin EOF (clean)")
		return nil
	}
	if stdinErr != nil {
		logger.Debug("bridge stopped", "cause", "stdin error", "error", stdinErr)
		return stdinErr
	}
	logger.Debug("bridge stopped", "cause", "daemon error", "error", daemonErr)
	return daemonErr
}
