package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/punt-labs/mcp-proxy/internal/bridge"
	"github.com/punt-labs/mcp-proxy/internal/debuglog"
	"github.com/punt-labs/mcp-proxy/internal/session"
	"github.com/punt-labs/mcp-proxy/internal/transport"
)

const usage = "Usage: mcp-proxy <daemon-url>\n\nExample: mcp-proxy ws://localhost:8080/mcp\n"

func main() {
	os.Exit(run())
}

func run() int {
	if len(os.Args) != 2 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	rawURL := os.Args[1]

	logger, logCloser := debuglog.FromEnv()
	defer logCloser.(io.Closer).Close()

	// First signal cancels context (graceful shutdown).
	// Second signal force-exits immediately.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	go forceExitOnSecondSignal(ctx)

	sessionKey := session.FindSessionKey()
	logger.Debug("session key resolved", "key", sessionKey)

	conn, err := transport.Dial(ctx, rawURL, sessionKey, logger)
	if err != nil {
		var connErr *transport.ConnectionRefusedError
		var urlErr *transport.InvalidURLError
		var timeErr *transport.TimeoutError
		switch {
		case errors.As(err, &connErr):
			fmt.Fprintf(os.Stderr, "mcp-proxy: connection refused: %s\n", connErr.Addr)
		case errors.As(err, &urlErr):
			fmt.Fprintf(os.Stderr, "mcp-proxy: invalid URL: %s\n", urlErr.URL)
		case errors.As(err, &timeErr):
			fmt.Fprintf(os.Stderr, "mcp-proxy: connection timed out: %s\n", timeErr.Addr)
		default:
			fmt.Fprintf(os.Stderr, "mcp-proxy: %v\n", err)
		}
		return 1
	}

	err = bridge.Run(ctx, os.Stdin, os.Stdout, conn, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcp-proxy: %v\n", err)
		return 1
	}
	return 0
}

// forceExitOnSecondSignal waits for the context to be cancelled (first signal),
// then installs a handler that exits immediately on the next signal.
func forceExitOnSecondSignal(ctx context.Context) {
	<-ctx.Done()
	// Context cancelled — first signal received. Now wait for second.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fmt.Fprintf(os.Stderr, "mcp-proxy: forced exit\n")
	os.Exit(1)
}
