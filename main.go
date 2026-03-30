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

	"github.com/punt-labs/mcp-proxy/internal/config"
	"github.com/punt-labs/mcp-proxy/internal/debuglog"
	"github.com/punt-labs/mcp-proxy/internal/hook"
	"github.com/punt-labs/mcp-proxy/internal/reconnect"
	"github.com/punt-labs/mcp-proxy/internal/session"
	"github.com/punt-labs/mcp-proxy/internal/transport"
)

var version = "dev"

const usage = `Usage: mcp-proxy [--config <profile>] [<daemon-url>]
       mcp-proxy --version
       mcp-proxy --health <daemon-url>
       mcp-proxy <daemon-url> --hook <event>
       mcp-proxy <daemon-url> --hook --async <event>

Examples:
  mcp-proxy ws://localhost:8080/mcp              # MCP bridge (long-running)
  mcp-proxy --config quarry                      # Load URL and headers from ~/.punt-labs/mcp-proxy/quarry.toml
  mcp-proxy --config quarry ws://override/mcp    # Config headers, positional URL wins
  mcp-proxy --health ws://localhost:8080/mcp     # Health check
  mcp-proxy ws://localhost:8080 --hook PreToolUse        # Sync hook relay
  mcp-proxy ws://localhost:8080 --hook --async SessionEnd # Async hook relay
`

func main() {
	os.Exit(run())
}

// parsedArgs holds the result of argument parsing.
type parsedArgs struct {
	profile     string // --config <profile>, empty if not given
	daemonURL   string // positional URL, empty if not given
	healthCheck bool
	hookEvent   string
	hookAsync   bool
	showVersion bool
}

func parseArgs(args []string) (parsedArgs, bool) {
	var p parsedArgs

	// --version (must be the only arg)
	if len(args) == 1 && args[0] == "--version" {
		p.showVersion = true
		return p, true
	}

	// Consume named flags first, leaving positional args.
	rest := args[:0:0]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--config":
			if i+1 >= len(args) {
				return p, false
			}
			p.profile = args[i+1]
			i++
		default:
			rest = append(rest, args[i])
		}
	}
	args = rest

	// --health <url>
	if len(args) >= 1 && args[0] == "--health" {
		if len(args) != 2 {
			return p, false
		}
		p.healthCheck = true
		p.daemonURL = args[1]
		return p, true
	}

	// <url> --hook [--async] <event>
	if len(args) >= 3 && args[1] == "--hook" {
		p.daemonURL = args[0]
		hookArgs := args[2:]
		if len(hookArgs) >= 1 && hookArgs[0] == "--async" {
			p.hookAsync = true
			hookArgs = hookArgs[1:]
		}
		if len(hookArgs) != 1 {
			return p, false
		}
		p.hookEvent = hookArgs[0]
		return p, true
	}

	// Optional positional URL.
	switch len(args) {
	case 0:
		// No URL — config or default.
		return p, true
	case 1:
		p.daemonURL = args[0]
		return p, true
	default:
		return p, false
	}
}

func run() int {
	p, ok := parseArgs(os.Args[1:])
	if !ok {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}

	if p.showVersion {
		fmt.Printf("mcp-proxy %s\n", version)
		return 0
	}

	if p.healthCheck {
		return runHealthCheck(p.daemonURL)
	}

	// Load config profile (if requested).
	var extraHeaders map[string]string
	configURL := ""
	if p.profile != "" {
		prof, err := config.Load(p.profile)
		if err != nil {
			// Insecure permissions — print tilde path as specified.
			fmt.Fprintf(os.Stderr, "mcp-proxy: %v\n", err)
			return 1
		}
		configURL = prof.URL
		extraHeaders = prof.Headers
	}

	// Resolve effective daemon URL:
	//   positional URL > config URL > default.
	daemonURL := p.daemonURL
	if daemonURL == "" {
		daemonURL = configURL
	}
	if daemonURL == "" {
		daemonURL = config.DefaultURL
	}

	if p.hookEvent != "" {
		return runHook(daemonURL, p.hookEvent, p.hookAsync, extraHeaders)
	}
	return runProxy(daemonURL, extraHeaders)
}

func runHealthCheck(rawURL string) int {
	logger := debuglog.Nop()

	// Safety-net timeout slightly beyond Dial's internal DialTimeout,
	// so runHealthCheck never hangs even if Dial's internals change.
	ctx, cancel := context.WithTimeout(context.Background(), transport.DialTimeout+time.Second)
	defer cancel()

	conn, err := transport.Dial(ctx, rawURL, 0, nil, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcp-proxy: health check failed: %v\n", err)
		return 1
	}
	conn.CloseNow()
	fmt.Fprintln(os.Stderr, "mcp-proxy: ok")
	return 0
}

func runProxy(rawURL string, extraHeaders map[string]string) int {
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
		conn, err := transport.Dial(dialCtx, rawURL, sessionKey, extraHeaders, logger)
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

func runHook(rawURL string, event string, async bool, extraHeaders map[string]string) int {
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

	conn, err := transport.DialHook(dialCtx, hookURL, sessionKey, extraHeaders, logger)
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
// Logs a warning to stderr on parse errors or negative values.
// Zero is allowed (used to disable features like keepalive).
func envDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcp-proxy: invalid %s=%q, using default %s\n", key, v, fallback)
		return fallback
	}
	if d < 0 {
		fmt.Fprintf(os.Stderr, "mcp-proxy: negative %s=%s, using default %s\n", key, v, fallback)
		return fallback
	}
	return d
}
