package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/punt-labs/mcp-proxy/internal/debuglog"
	"github.com/punt-labs/mcp-proxy/internal/hook"
	"github.com/punt-labs/mcp-proxy/internal/reconnect"
	"github.com/punt-labs/mcp-proxy/internal/session"
	"github.com/punt-labs/mcp-proxy/internal/transport"
)

// runHealthCheck dials, closes, and returns. Preserves the pre-migration
// behavior verbatim (repo-root main.go:193-209).
func runHealthCheck(ctx context.Context, rawURL string, extraHeaders map[string]string, caCert string, stderr io.Writer) error {
	logger := debuglog.Nop()

	if ctx == nil {
		ctx = context.Background()
	}
	// Safety-net timeout slightly beyond Dial's internal DialTimeout,
	// so runHealthCheck never hangs even if Dial's internals change.
	dialCtx, cancel := context.WithTimeout(ctx, transport.DialTimeout+time.Second)
	defer cancel()

	conn, err := transport.Dial(dialCtx, rawURL, 0, extraHeaders, caCert, logger)
	if err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}
	_ = conn.CloseNow()
	_, _ = fmt.Fprintln(stderr, "mcp-proxy: ok")
	return nil
}

// runProxy runs the long-lived bridge. Signal handling is inside this
// function, not at the cobra layer (DES-008).
func runProxy(rawURL string, extraHeaders map[string]string, caCert string, stdin io.Reader, stdout, stderr io.Writer) error {
	logger, logCloser := debuglog.FromEnv()
	defer func() { _ = logCloser.Close() }()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	go forceExitOnSecondSignal(ctx, stderr)

	sessionKey := session.FindSessionKey()
	logger.Debug("session key resolved", "key", sessionKey)

	dial := func(dialCtx context.Context) (reconnect.Conn, error) {
		conn, err := transport.Dial(dialCtx, rawURL, sessionKey, extraHeaders, caCert, logger)
		if err != nil {
			return nil, err
		}
		conn.SetReadLimit(1024 * 1024)
		return conn, nil
	}

	cfg := reconnect.Config{
		PingInterval: envDuration("MCP_PROXY_PING_INTERVAL", 5*time.Second, stderr),
		PongTimeout:  envDuration("MCP_PROXY_PONG_TIMEOUT", 2*time.Second, stderr),
	}
	logger.Debug("config", "ping_interval", cfg.PingInterval, "pong_timeout", cfg.PongTimeout)

	if err := reconnect.RunWithConfig(ctx, stdin, stdout, dial, cfg, logger); err != nil {
		return err
	}
	return nil
}

// runHook does the one-shot hook relay. Preserves repo-root main.go:248-297 verbatim.
func runHook(rawURL, event string, async bool, extraHeaders map[string]string, caCert string, stdin io.Reader, stdout, stderr io.Writer) error {
	logger, logCloser := debuglog.FromEnv()
	defer func() { _ = logCloser.Close() }()

	sessionKey := session.FindSessionKey()
	logger.Debug("hook mode", "event", event, "async", async, "session_key", sessionKey)

	u, err := url.Parse(rawURL)
	if err != nil {
		// Malformed URL is a usage error, not a runtime error.
		return &usageError{msg: fmt.Sprintf("invalid URL: %v", err)}
	}
	u.Path = "/hook"
	u.RawPath = ""
	u.Fragment = ""
	hookURL := u.String()

	dialCtx, dialCancel := context.WithTimeout(context.Background(), transport.DialTimeout+time.Second)
	defer dialCancel()

	conn, err := transport.DialHook(dialCtx, hookURL, sessionKey, extraHeaders, caCert, logger)
	if err != nil {
		return err
	}
	defer func() { _ = conn.CloseNow() }()

	conn.SetReadLimit(1024 * 1024)

	ctx, cancel := context.WithTimeout(context.Background(), hook.ResponseTimeout)
	defer cancel()

	if err := hook.Run(ctx, stdin, stdout, stderr, conn, event, async, logger); err != nil {
		if errors.Is(err, hook.ErrDaemonError) {
			return &runtimeError{err: err, silent: true}
		}
		return err
	}
	return nil
}

// forceExitOnSecondSignal waits for context cancellation (first signal),
// then installs a handler that exits immediately on the next signal.
func forceExitOnSecondSignal(ctx context.Context, stderr io.Writer) {
	<-ctx.Done()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	_, _ = fmt.Fprintln(stderr, "mcp-proxy: forced exit")
	os.Exit(1)
}

// envDuration reads a duration from an environment variable. Accepts Go
// duration strings (e.g. "5s", "500ms"). Warns to stderr on parse errors
// or negative values. Zero is allowed.
func envDuration(key string, fallback time.Duration, stderr io.Writer) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "mcp-proxy: invalid %s=%q, using default %s\n", key, v, fallback)
		return fallback
	}
	if d < 0 {
		_, _ = fmt.Fprintf(stderr, "mcp-proxy: negative %s=%s, using default %s\n", key, v, fallback)
		return fallback
	}
	return d
}
