//go:build e2e

// Black-box tests for the CLI surface added in the cobra migration
// (docs/cli-cobra.md §9). These invoke the compiled binary and assert
// on stdout, stderr, and exit codes — not on internal flag state.
package e2e_test

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/punt-labs/mcp-proxy/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCLI_E2E_Help pins the fix for the pre-migration bug where --help
// was treated as a URL positional and the reconnect loop entered.
func TestCLI_E2E_Help(t *testing.T) {
	bin := binaryPath(t)
	for _, argv := range [][]string{{"--help"}, {"-h"}} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			cmd := exec.Command(bin, argv...)
			var stdout, stderr testutil.SafeBuffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr

			err := cmd.Run()
			assert.NoError(t, err, "stderr: %s", stderr.String())
			assert.Contains(t, stdout.String(), "Usage:")
		})
	}
}

// TestCLI_E2E_VersionFlag exercises `mcp-proxy --version`.
func TestCLI_E2E_VersionFlag(t *testing.T) {
	bin := binaryPath(t)
	cmd := exec.Command(bin, "--version")
	var stdout, stderr testutil.SafeBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	require.NoError(t, err, "stderr: %s", stderr.String())

	matched, matchErr := regexp.MatchString(`^mcp-proxy \S+\n$`, stdout.String())
	require.NoError(t, matchErr)
	assert.True(t, matched, "unexpected --version output: %q", stdout.String())
}

// TestCLI_E2E_VersionSubcommand exercises `mcp-proxy version`.
func TestCLI_E2E_VersionSubcommand(t *testing.T) {
	bin := binaryPath(t)
	cmd := exec.Command(bin, "version")
	var stdout, stderr testutil.SafeBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	require.NoError(t, err, "stderr: %s", stderr.String())

	matched, matchErr := regexp.MatchString(`^mcp-proxy \S+\n$`, stdout.String())
	require.NoError(t, matchErr)
	assert.True(t, matched, "unexpected `version` output: %q", stdout.String())
}

// TestCLI_E2E_UnknownFlag is the regression guard for the exit-code
// split. If pflag's internal error format ever changes, this test
// fails and the fix is a one-line update to pflagErrorPrefixes.
func TestCLI_E2E_UnknownFlag(t *testing.T) {
	bin := binaryPath(t)
	cmd := exec.Command(bin, "--nonsense")
	var stdout, stderr testutil.SafeBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	require.Error(t, err)
	exitErr, ok := err.(*exec.ExitError)
	require.True(t, ok, "expected ExitError, got %T", err)
	assert.Equal(t, 2, exitErr.ExitCode())
	assert.Contains(t, stderr.String(), "unknown flag")
}

// TestCLI_E2E_MissingConfigProfile documents the current behavior of
// `mcp-proxy --config <does-not-exist> --health`: config.Load silently
// falls back on ENOENT (config.go:66-70), URL resolves to DefaultURL,
// and the health dial fails against the unreachable default. Exit 1.
// If the operator wants missing profiles to be fatal, that is a
// separate ADR — this test pins today's shape.
func TestCLI_E2E_MissingConfigProfile(t *testing.T) {
	bin := binaryPath(t)
	cmd := exec.Command(bin, "--config", "nope-does-not-exist", "--health")
	var stdout, stderr testutil.SafeBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	require.Error(t, err)
	exitErr, ok := err.(*exec.ExitError)
	require.True(t, ok, "expected ExitError, got %T", err)
	assert.Equal(t, 1, exitErr.ExitCode(), "stderr: %s", stderr.String())
	assert.Contains(t, stderr.String(), "health check failed")
}
