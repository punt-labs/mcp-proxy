package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/punt-labs/mcp-proxy/internal/debuglog"
	"github.com/punt-labs/mcp-proxy/internal/hook"
	"github.com/punt-labs/mcp-proxy/internal/reconnect"
	"github.com/punt-labs/mcp-proxy/internal/session"
	"github.com/punt-labs/mcp-proxy/internal/transport"
)

const usage = `Usage: mcp-proxy <daemon-url>
       mcp-proxy --health <daemon-url>
       mcp-proxy <daemon-url> --hook <event>
       mcp-proxy <daemon-url> --hook --async <event>

Examples:
  mcp-proxy ws://localhost:8080/mcp              # MCP bridge (long-running)
  mcp-proxy --health ws://localhost:8080/mcp     # Health check
  mcp-proxy ws://localhost:8080 --hook PreToolUse        # Sync hook relay
  mcp-proxy ws://localhost:8080 --hook --async SessionEnd # Async hook relay
`

func main() {
	os.Exit(run())
}

func run() int {
	args := os.Args[1:]

	// --health <url>
	if len(args) >= 1 && args[0] == "--health" {
		if len(args) != 2 {
			fmt.Fprint(os.Stderr, usage)
			return 2
		}
		return runHealthCheck(args[1])
	}

	// <url> --hook [--async] <event>
	if len(args) >= 3 && args[1] == "--hook" {
		return parseHook(args[0], args[2:])
	}

	// <url> (proxy mode)
	if len(args) != 1 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	return runProxy(args[0])
}

func parseHook(rawURL string, args []string) int {
	async := false
	if len(args) >= 1 && args[0] == "--async" {
		async = true
		args = args[1:]
	}
	if len(args) != 1 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	return runHook(rawURL, args[0], async)
}

func runHealthCheck(rawURL string) int {
	logger := debuglog.Nop()

	// Safety-net timeout slightly beyond Dial's internal DialTimeout,
	// so runHealthCheck never hangs even if Dial's internals change.
	ctx, cancel := context.WithTimeout(context.Background(), transport.DialTimeout+time.Second)
	defer cancel()

	conn, err := transport.Dial(ctx, rawURL, 0, logger)
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

	cfg := reconnect.Config{
		PingInterval: envDuration("MCP_PROXY_PING_INTERVAL", 5*time.Second),
		PongTimeout:  envDuration("MCP_PROXY_PONG_TIMEOUT", 2*time.Second),
	}
	logger.Debug("config", "ping_interval", cfg.PingInterval, "pong_timeout", cfg.PongTimeout)

	err := reconnect.RunWithConfig(ctx, os.Stdin, os.Stdout, dial, cfg, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcp-proxy: %v\n", err)
		return 1
	}
	return 0
}

func runHook(rawURL string, event string, async bool) int {
	logger, logCloser := debuglog.FromEnv()
	defer logCloser.Close()

	sessionKey := session.FindSessionKey()
	logger.Debug("hook mode", "event", event, "async", async, "session_key", sessionKey)

	// Validate and append /hook to the base URL.
	u, err := url.Parse(rawURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcp-proxy: invalid URL: %v\n", err)
		return 2
	}
	trimmedPath := strings.TrimRight(u.Path, "/")
	if trimmedPath != "" && trimmedPath != "/hook" {
		fmt.Fprintf(os.Stderr, "mcp-proxy: hook mode expects a base URL (e.g., ws://host:port), got path %q\n", u.Path)
		return 2
	}
	var hookURL string
	if trimmedPath == "/hook" {
		u.Path = "/hook"
		hookURL = u.String()
	} else {
		hookURL, err = appendPath(rawURL, "hook")
		if err != nil {
			fmt.Fprintf(os.Stderr, "mcp-proxy: invalid URL: %v\n", err)
			return 2
		}
	}

	// Dial with standard timeout.
	dialCtx, dialCancel := context.WithTimeout(context.Background(), transport.DialTimeout+time.Second)
	defer dialCancel()

	conn, err := transport.DialHook(dialCtx, hookURL, sessionKey, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcp-proxy: %v\n", err)
		return 1
	}
	defer conn.CloseNow()

	conn.SetReadLimit(1024 * 1024) // 1MB

	// Overall hook timeout: covers stdin read + send + response wait.
	// The hook framework enforces the real budget by killing the process —
	// this is a safety net against silent hangs.
	ctx, cancel := context.WithTimeout(context.Background(), hook.ResponseTimeout)
	defer cancel()

	err = hook.Run(ctx, os.Stdin, os.Stdout, os.Stderr, conn, event, async, logger)
	if err != nil {
		// ErrDaemonError means hook.Run already printed the error to stderr.
		if !errors.Is(err, hook.ErrDaemonError) {
			fmt.Fprintf(os.Stderr, "mcp-proxy: %v\n", err)
		}
		return 1
	}
	return 0
}

// appendPath appends a path segment to a WebSocket URL.
func appendPath(rawURL, segment string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + segment
	return u.String(), nil
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

// envDuration reads a duration from an environment variable, falling back to
// the provided default. Accepts Go duration strings (e.g., "5s", "500ms").
func envDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}
