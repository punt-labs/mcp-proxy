package main

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/punt-labs/mcp-proxy/internal/config"
)

// flags holds the parsed root-command flag state. Kept as a value type so
// tests can construct it directly.
type flags struct {
	profile     string
	health      bool
	hookEvent   string
	hookAsync   bool
	showVersion bool
}

// flagSetKey stashes the *pflag.FlagSet on the cobra context so dispatch
// can query fs.Changed for the empty-string --config edge case.
type flagSetKey struct{}

// newRootCmd builds the root cobra command. stdin/out/err are captured so
// tests can inject buffers; production callers pass os.Stdin/Stdout/Stderr.
func newRootCmd(stdin io.Reader, stdout, stderr io.Writer) *cobra.Command {
	var f flags

	cmd := &cobra.Command{
		Use:   "mcp-proxy [flags] [daemon-url]",
		Short: "Bridge MCP stdio to a shared daemon over WebSocket.",
		Long: `mcp-proxy bridges MCP stdio to a shared daemon over WebSocket.

The default mode is the long-running MCP bridge: JSON-RPC on stdin/stdout
is forwarded to the daemon over WebSocket. --health performs a one-shot
liveness dial. --hook forwards a single event payload from stdin to the
daemon's /hook endpoint.`,
		Args:          cobra.MaximumNArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		Example: `  mcp-proxy ws://localhost:8420/mcp
  mcp-proxy --config quarry
  mcp-proxy --health ws://localhost:8420/mcp
  mcp-proxy --config quarry --health
  mcp-proxy ws://localhost:8420 --hook PreToolUse < payload.json
  mcp-proxy --config quarry --hook --async SessionEnd < payload.json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return dispatch(cmd.Context(), &f, args, stdin, stdout, stderr)
		},
	}

	fs := cmd.Flags()
	fs.StringVar(&f.profile, "config", "",
		"Load URL and headers from ~/.punt-labs/mcp-proxy/<profile>.toml")
	fs.BoolVar(&f.health, "health", false,
		"Health check: dial, close, exit 0/1")
	fs.StringVar(&f.hookEvent, "hook", "",
		"Hook relay: send stdin as JSON-RPC hook/<event>")
	fs.BoolVar(&f.hookAsync, "async", false,
		"With --hook: send as notification, no response")
	fs.BoolVar(&f.showVersion, "version", false,
		"Print the version and exit")

	// Stash the flagset on the context so dispatch can query fs.Changed
	// for the empty-string --config edge case (design §7).
	cmd.PersistentPreRun = func(cmd *cobra.Command, _ []string) {
		ctx := context.WithValue(cmd.Context(), flagSetKey{}, fs)
		cmd.SetContext(ctx)
	}

	cmd.SetUsageTemplate(usageTemplate)
	cmd.SetHelpTemplate(helpTemplate)

	cmd.AddCommand(newVersionCmd(stdout))

	return cmd
}

// dispatch resolves the URL, validates flag interactions, and hands off
// to the right execution mode. It never talks to the network directly —
// every mode calls a helper that owns its own I/O and lifecycle.
func dispatch(ctx context.Context, f *flags, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if f.showVersion {
		if f.profile != "" || f.health || f.hookEvent != "" || f.hookAsync || len(args) > 0 {
			return &usageError{msg: "--version takes no other arguments"}
		}
		_, _ = fmt.Fprintf(stdout, "mcp-proxy %s\n", version)
		return nil
	}

	if f.health && f.hookEvent != "" {
		return &usageError{msg: "--health and --hook are mutually exclusive"}
	}
	if f.hookAsync && f.hookEvent == "" {
		return &usageError{msg: "--async requires --hook"}
	}

	// pflag lets --config="" through silently; the old parser rejected it.
	if f.profile == "" && cobraFlagChanged(ctx, "config") {
		return &usageError{msg: "--config requires a non-empty profile name"}
	}

	positionalURL := ""
	if len(args) == 1 {
		positionalURL = args[0]
	}

	// The old parser required a URL positional or --config for every mode.
	// Keep that contract so `mcp-proxy` with no args exits 2, not runProxy
	// against the default URL.
	if positionalURL == "" && f.profile == "" {
		return &usageError{msg: "daemon URL required (or use --config)"}
	}

	var extraHeaders map[string]string
	var caCert string
	configURL := ""
	if f.profile != "" {
		prof, err := config.Load(f.profile)
		if err != nil {
			return err
		}
		configURL = prof.URL
		extraHeaders = prof.Headers
		caCert = prof.CACert
	}

	daemonURL := positionalURL
	if daemonURL == "" {
		daemonURL = configURL
	}
	if daemonURL == "" {
		daemonURL = config.DefaultURL
	}

	switch {
	case f.health:
		return runHealthCheck(ctx, daemonURL, extraHeaders, caCert, stderr)
	case f.hookEvent != "":
		return runHook(daemonURL, f.hookEvent, f.hookAsync, extraHeaders, caCert, stdin, stdout, stderr)
	default:
		return runProxy(daemonURL, extraHeaders, caCert, stdin, stdout, stderr)
	}
}

// cobraFlagChanged is a shim so dispatch can query pflag state without
// carrying the *cobra.Command through helper signatures.
func cobraFlagChanged(ctx context.Context, name string) bool {
	if fs, ok := ctx.Value(flagSetKey{}).(*pflag.FlagSet); ok {
		return fs.Changed(name)
	}
	return false
}

// helpTemplate produces plain-text --help output — no colour, no markdown,
// with the environment block appended after the flag list.
const helpTemplate = `{{.Long}}

Usage:
  {{.UseLine}}
  mcp-proxy version

Flags:
{{.LocalFlags.FlagUsages}}
Environment:
  MCP_PROXY_TOKEN          Bearer token added to the WebSocket upgrade
  MCP_PROXY_DEBUG          1|true to log to $TMPDIR/mcp-proxy-<pid>.log,
                           any other value is treated as a file path
  MCP_PROXY_PING_INTERVAL  WebSocket ping cadence (default 5s)
  MCP_PROXY_PONG_TIMEOUT   Pong wait after each ping (default 2s)

Examples:
{{.Example}}

Exit codes:
  0  Success
  1  Runtime error (dial failure, daemon error, unclean shutdown)
  2  Usage error (unrecognised flag, missing required argument)
`

// usageTemplate is what cobra prints on a parse error. Kept short because
// main() also emits the 'mcp-proxy: <err>' line.
const usageTemplate = `Usage:
  {{.UseLine}}
  mcp-proxy version
`
