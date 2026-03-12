package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/punt-labs/mcp-proxy/internal/debuglog"
	"github.com/punt-labs/mcp-proxy/internal/reconnect"
	"github.com/punt-labs/mcp-proxy/internal/session"
	"github.com/punt-labs/mcp-proxy/internal/transport"
)

const usage = `Usage: mcp-proxy [--health] <daemon-url>

Example: mcp-proxy ws://localhost:8080/mcp
         mcp-proxy --health ws://localhost:8080/mcp
`

func main() {
	os.Exit(run())
}

func run() int {
	args := os.Args[1:]

	if len(args) >= 1 && args[0] == "--health" {
		if len(args) != 2 {
			fmt.Fprint(os.Stderr, usage)
			return 2
		}
		return runHealthCheck(args[1])
	}
	if len(args) != 1 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	return runProxy(args[0])
}

func runHealthCheck(rawURL string) int {
	logger := debuglog.Nop()

	// Dial applies its own DialTimeout internally — no outer timeout needed.
	conn, err := transport.Dial(context.Background(), rawURL, 0, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcp-proxy: health check failed: %v\n", err)
		return 1
	}
	conn.CloseNow()
	fmt.Fprintln(os.Stderr, "mcp-proxy: ok")
	return 0
}

func runProxy(rawURL string) int {
	logger, logCloser := debuglog.FromEnv()
	defer logCloser.Close()

	// First signal cancels context (graceful shutdown).
	// Second signal force-exits immediately.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	go forceExitOnSecondSignal(ctx)

	sessionKey := session.FindSessionKey()
	logger.Debug("session key resolved", "key", sessionKey)

	dial := func(dialCtx context.Context) (reconnect.Conn, error) {
		conn, err := transport.Dial(dialCtx, rawURL, sessionKey, logger)
		if err != nil {
			return nil, err
		}
		// MCP messages can be large (tool responses with embedded data).
		conn.SetReadLimit(1024 * 1024) // 1MB
		return conn, nil
	}

	err := reconnect.Run(ctx, os.Stdin, os.Stdout, dial, logger)
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
