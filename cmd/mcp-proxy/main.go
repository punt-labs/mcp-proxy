// Command mcp-proxy is the entrypoint binary that bridges MCP stdio transport
// to a daemon over WebSocket. It runs in three modes: long-running proxy
// (default), one-shot health check (--health), and one-shot hook relay (--hook).
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// shortUsage is printed after a usage error so callers see the same tokens
// they would from `mcp-proxy --help`, minus the long-form flag descriptions.
const shortUsage = `Usage:
  mcp-proxy [flags] [daemon-url]
  mcp-proxy version
Run 'mcp-proxy --help' for the full flag list.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run wires cobra to the caller's argv and maps errors to exit codes.
//
//	0 clean exit
//	1 runtime error (dial failure, daemon error, unclean shutdown)
//	2 usage error (bad flag, missing required argument)
func run(argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cmd, _ := newRootCmd(stdin, stdout, stderr)
	cmd.SetArgs(rewriteHookAsync(argv))
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)

	return reportError(cmd.Execute(), stderr)
}

// reportError maps a cobra Execute() result to a process exit code and
// prints the appropriate diagnostic line to stderr. Extracted from run()
// so the *silentError branch — which deliberately suppresses the leader's
// "mcp-proxy: <err>" prefix because the underlying operation has already
// written its own diagnostic — can be exercised in isolation.
func reportError(err error, stderr io.Writer) int {
	switch {
	case err == nil:
		return 0
	case isUsageError(err):
		_, _ = fmt.Fprintf(stderr, "mcp-proxy: %v\n", err)
		_, _ = fmt.Fprint(stderr, shortUsage)
		return 2
	default:
		var se *silentError
		if errors.As(err, &se) {
			return 1
		}
		_, _ = fmt.Fprintf(stderr, "mcp-proxy: %v\n", err)
		return 1
	}
}

// rewriteHookAsync reorders '--hook --async' into '--async --hook' so
// pflag does not consume '--async' as the string value of --hook.
//
// The design (docs/cli-cobra.md §2) promises that the historical shape
// `mcp-proxy <url> --hook --async <event>` continues to parse to
// hook=<event>, async=true. Under a StringVar --hook, pflag greedily
// consumes the next token — including a flag — as the value; the swap
// keeps the promise without a custom Var type and without asking
// callers to change their argv.
func rewriteHookAsync(argv []string) []string {
	out := make([]string, 0, len(argv))
	i := 0
	for i < len(argv) {
		if argv[i] == "--hook" && i+1 < len(argv) && argv[i+1] == "--async" {
			out = append(out, "--async", "--hook")
			i += 2
			continue
		}
		out = append(out, argv[i])
		i++
	}
	return out
}

// usageError signals that the caller supplied an invalid argv shape.
// Cobra's own flag-parse failures are wrapped in this type so main()
// can route both classes to exit 2.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

// silentError suppresses main()'s "mcp-proxy: <err>" line when the
// underlying operation has already written its own diagnostic to stderr
// (e.g. hook.Run for ErrDaemonError). Exit code is still 1.
type silentError struct{ err error }

func (e *silentError) Error() string { return e.err.Error() }
func (e *silentError) Unwrap() error { return e.err }

// isUsageError reports whether err came from argv validation. Both pflag's
// parse failures and cobra's positional-args mismatches are wrapped in
// *usageError at their source (SetFlagErrorFunc, custom Args validators),
// so a single type-assertion suffices.
func isUsageError(err error) bool {
	var ue *usageError
	return errors.As(err, &ue)
}
