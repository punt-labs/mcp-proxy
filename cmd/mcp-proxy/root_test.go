package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
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
		name      string
		flags     flags
		args      []string
		configSet bool
	}{
		{"async without hook", flags{hookAsync: true}, []string{"ws://x"}, false},
		{"health and hook", flags{health: true, hookEvent: "e"}, []string{"ws://x"}, false},
		{"health no url no config", flags{health: true}, nil, false},
		{"hook no url no config", flags{hookEvent: "e"}, nil, false},
		{"no url no config", flags{}, nil, false},
		{"version with url", flags{showVersion: true}, []string{"ws://x"}, false},
		{"version with config", flags{showVersion: true, profile: "quarry"}, nil, false},
		{"version with health", flags{showVersion: true, health: true}, nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := dispatch(context.Background(), &tc.flags, tc.configSet, tc.args, strings.NewReader(""), &stdout, &stderr)
			require.Error(t, err)
			var ue *usageError
			assert.True(t, errors.As(err, &ue), "expected usageError, got %T: %v", err, err)
		})
	}
}

// TestRootCmd_EmptyConfig drives the empty --config="" guard through the
// real cobra tree. Cannot be exercised via dispatch() alone because the
// "was --config explicitly set" bit lives on cmd.Flags().
func TestRootCmd_EmptyConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root, _ := newRootCmd(strings.NewReader(""), &stdout, &stderr)
	root.SetArgs([]string{"--config", "", "ws://x"})
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	err := root.Execute()
	require.Error(t, err)
	assert.True(t, isUsageError(err), "empty --config must classify as usage error: %v", err)
	assert.Contains(t, err.Error(), "non-empty profile")
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

// TestRootCmd_CobraArgsErrors pins cobra's positional-args predicates to
// the usage-error path. Without wrapping cobra.MaximumNArgs/cobra.NoArgs
// in *usageError at the source, these three argv shapes wrongly exit 1
// instead of 2.
func TestRootCmd_CobraArgsErrors(t *testing.T) {
	tests := []struct {
		name    string
		argv    []string
		wantSub string
	}{
		{
			"root too many args",
			[]string{"foo", "bar", "baz"},
			"accepts at most",
		},
		{
			// Two extra positionals trip MaximumNArgs before RunE, so
			// dispatch's own "--version takes no other arguments" guard
			// never runs. The Args wrapper is what routes this to exit 2.
			"version flag with extra positionals",
			[]string{"--version", "foo", "bar"},
			"accepts at most",
		},
		{
			"version subcommand extra token",
			[]string{"version", "extra-token"},
			"unknown command",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			root, _ := newRootCmd(strings.NewReader(""), &stdout, &stderr)
			root.SetArgs(tc.argv)
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			err := root.Execute()
			require.Error(t, err)
			assert.True(t, isUsageError(err), "must classify as usage error: %v", err)
			assert.Contains(t, err.Error(), tc.wantSub)
		})
	}
}
