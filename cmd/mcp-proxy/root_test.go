package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parseOnly executes rootCmd against argv but replaces the RunE with a
// capture-only shim so no daemon dial ever fires. Returns the parsed
// flags, positional args, and the cobra execution error.
func parseOnly(t *testing.T, argv []string) (*flags, []string, error) {
	t.Helper()
	var stdin bytes.Buffer
	var stdout, stderr bytes.Buffer

	root, f := newRootCmd(&stdin, &stdout, &stderr)

	var captured []string
	root.RunE = func(_ *cobra.Command, args []string) error {
		captured = append([]string(nil), args...)
		return nil
	}
	// The version subcommand still calls its own RunE (prints version and
	// returns nil). Leave it alone.

	root.SetArgs(rewriteHookAsync(argv))
	root.SetOut(&stdout)
	root.SetErr(&stderr)

	err := root.Execute()
	return f, captured, err
}

// TestRewriteHookAsync exercises the pre-parse rewrite that keeps the
// pre-migration `--hook --async <event>` shape working under pflag's
// greedy StringVar consumption.
func TestRewriteHookAsync(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"empty", []string{}, []string{}},
		{"no hook", []string{"ws://x"}, []string{"ws://x"}},
		{"hook then event", []string{"--hook", "PreToolUse"}, []string{"--hook", "PreToolUse"}},
		{
			"hook async event",
			[]string{"--hook", "--async", "SessionEnd"},
			[]string{"--async", "--hook", "SessionEnd"},
		},
		{
			"config hook async event",
			[]string{"--config", "quarry", "--hook", "--async", "SessionEnd"},
			[]string{"--config", "quarry", "--async", "--hook", "SessionEnd"},
		},
		{
			"url then hook async event",
			[]string{"ws://x", "--hook", "--async", "SessionEnd"},
			[]string{"ws://x", "--async", "--hook", "SessionEnd"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteHookAsync(tc.in)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestRootCmd_ParseOrderIndependence covers docs/cli-cobra.md §2's table:
// each row is a distinct argv shape that must parse to the same flag state.
func TestRootCmd_ParseOrderIndependence(t *testing.T) {
	type want struct {
		profile, hook string
		health, async bool
		args          []string
	}
	tests := []struct {
		name string
		argv []string
		want want
	}{
		{
			"url then hook event",
			[]string{"ws://x", "--hook", "PreToolUse"},
			want{hook: "PreToolUse", args: []string{"ws://x"}},
		},
		{
			"hook event then url",
			[]string{"--hook", "PreToolUse", "ws://x"},
			want{hook: "PreToolUse", args: []string{"ws://x"}},
		},
		{
			"health then url",
			[]string{"--health", "ws://x"},
			want{health: true, args: []string{"ws://x"}},
		},
		{
			"config then health",
			[]string{"--config", "quarry", "--health"},
			want{profile: "quarry", health: true, args: nil},
		},
		{
			"config hook async event",
			[]string{"--config", "quarry", "--hook", "--async", "SessionEnd"},
			want{profile: "quarry", hook: "SessionEnd", async: true, args: nil},
		},
		{
			"url hook async event",
			[]string{"ws://x", "--hook", "--async", "SessionEnd"},
			want{hook: "SessionEnd", async: true, args: []string{"ws://x"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, args, err := parseOnly(t, tc.argv)
			require.NoError(t, err)
			assert.Equal(t, tc.want.profile, f.profile, "profile")
			assert.Equal(t, tc.want.hook, f.hookEvent, "hook")
			assert.Equal(t, tc.want.health, f.health, "health")
			assert.Equal(t, tc.want.async, f.hookAsync, "async")
			assert.Equal(t, tc.want.args, args, "positional args")
		})
	}
}

// TestDispatch_UsageErrors covers the §9 usage-error matrix. dispatch is
// called directly so the tests do not need a real daemon; each returned
// error must satisfy errors.As(&*usageError).
func TestDispatch_UsageErrors(t *testing.T) {
	tests := []struct {
		name   string
		flags  flags
		args   []string
		fsKeys map[string]bool // simulates fs.Changed for the empty --config case
	}{
		{"async without hook", flags{hookAsync: true}, []string{"ws://x"}, nil},
		{"health and hook", flags{health: true, hookEvent: "e"}, []string{"ws://x"}, nil},
		{"health no url no config", flags{health: true}, nil, nil},
		{"hook no url no config", flags{hookEvent: "e"}, nil, nil},
		{"no url no config", flags{}, nil, nil},
		{"empty config", flags{}, []string{"ws://x"}, map[string]bool{"config": true}},
		{"version with url", flags{showVersion: true}, []string{"ws://x"}, nil},
		{"version with config", flags{showVersion: true, profile: "quarry"}, nil, nil},
		{"version with health", flags{showVersion: true, health: true}, nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.fsKeys != nil {
				ctx = withFakeFlagSet(ctx, tc.fsKeys)
			}
			var stdout, stderr bytes.Buffer
			err := dispatch(ctx, &tc.flags, tc.args, strings.NewReader(""), &stdout, &stderr)
			require.Error(t, err)
			var ue *usageError
			assert.True(t, errors.As(err, &ue), "expected usageError, got %T: %v", err, err)
		})
	}
}

// TestRootCmd_Version verifies both the --version flag and the version
// subcommand emit `mcp-proxy <semver>\n` to stdout and return nil.
func TestRootCmd_Version(t *testing.T) {
	tests := []struct {
		name string
		argv []string
	}{
		{"flag", []string{"--version"}},
		{"subcommand", []string{"version"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			root, _ := newRootCmd(strings.NewReader(""), &stdout, &stderr)
			root.SetArgs(tc.argv)
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			err := root.Execute()
			require.NoError(t, err)
			out := stdout.String()
			assert.True(t, strings.HasPrefix(out, "mcp-proxy "), "stdout=%q", out)
			assert.True(t, strings.HasSuffix(out, "\n"), "stdout=%q", out)
		})
	}
}

// TestRootCmd_Help checks that --help and -h return nil and print usage.
func TestRootCmd_Help(t *testing.T) {
	for _, argv := range [][]string{{"--help"}, {"-h"}} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			root, _ := newRootCmd(strings.NewReader(""), &stdout, &stderr)
			root.SetArgs(argv)
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			err := root.Execute()
			require.NoError(t, err)
			assert.Contains(t, stdout.String(), "Usage:")
		})
	}
}

// TestRootCmd_UnknownFlag guards the §9 usage-error surface.
func TestRootCmd_UnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root, _ := newRootCmd(strings.NewReader(""), &stdout, &stderr)
	root.SetArgs([]string{"--nonsense"})
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	err := root.Execute()
	require.Error(t, err)
	assert.True(t, isUsageError(err), "unknown flag must classify as usage error: %v", err)
}

// TestIsUsageError_PflagPrefixes pins each pflag error class to the
// usage-error path. Without this, a pflag internals rename would
// silently route flag-parse failures to exit 1.
func TestIsUsageError_PflagPrefixes(t *testing.T) {
	msgs := []string{
		"unknown flag: --foo",
		"unknown shorthand flag: 'x' in -x",
		"flag needs an argument: --hook",
		"invalid argument \"abc\" for \"--async\"",
		"bad flag syntax: -=",
	}
	for _, m := range msgs {
		assert.True(t, isUsageError(errors.New(m)), "should be usage error: %q", m)
	}
	assert.False(t, isUsageError(errors.New("dial tcp: connection refused")))
}

// withFakeFlagSet returns a context carrying a real pflag.FlagSet with
// the named flags pre-marked as Changed(). This is the path the
// dispatch's --config="" guard reads through cobraFlagChanged.
func withFakeFlagSet(ctx context.Context, keys map[string]bool) context.Context {
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	for name := range keys {
		var s string
		fs.StringVar(&s, name, "", "")
		if keys[name] {
			require.NoError(nil, fs.Set(name, "")) //nolint:errcheck // best-effort test setup
		}
	}
	return context.WithValue(ctx, flagSetKey{}, fs)
}
