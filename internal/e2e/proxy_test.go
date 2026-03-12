//go:build e2e

package e2e_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/punt-labs/mcp-proxy/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func binaryPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	binPath := dir + "/mcp-proxy"
	cmd := exec.Command("go", "build", "-o", binPath, ".")
	cmd.Dir = findModuleRoot(t)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "go build failed: %s", out)
	return binPath
}

func findModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(dir + "/go.mod"); err == nil {
			return dir
		}
		parent := dir[:strings.LastIndex(dir, "/")]
		if parent == dir {
			t.Fatal("could not find module root")
		}
		dir = parent
	}
}

// waitForLines polls a SafeBuffer until it contains at least n newlines.
func waitForLines(buf *testutil.SafeBuffer, n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Count(buf.String(), "\n") >= n {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func TestProxy_E2E_RequestResponse(t *testing.T) {
	bin := binaryPath(t)
	d := testutil.NewMockDaemon()
	defer d.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, d.URL())
	stdinPipe, err := cmd.StdinPipe()
	require.NoError(t, err)

	stdout := &testutil.SafeBuffer{}
	stderr := &testutil.SafeBuffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	require.NoError(t, cmd.Start())

	fmt.Fprintln(stdinPipe, `{"jsonrpc":"2.0","method":"ping","id":1}`)
	require.True(t, waitForLines(stdout, 1, 5*time.Second), "timed out waiting for response")

	stdinPipe.Close()
	err = cmd.Wait()
	assert.NoError(t, err, "stderr: %s", stderr.String())
	// stdout may have "connected" on stderr plus the echoed JSON on stdout.
	// Split stdout lines and find the JSON one.
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	require.Len(t, lines, 1)
	assert.JSONEq(t, `{"jsonrpc":"2.0","method":"ping","id":1}`, lines[0])
}

func TestProxy_E2E_SessionKey(t *testing.T) {
	bin := binaryPath(t)
	d := testutil.NewMockDaemon()
	defer d.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, d.URL())
	stdinPipe, err := cmd.StdinPipe()
	require.NoError(t, err)

	stdout := &testutil.SafeBuffer{}
	stderr := &testutil.SafeBuffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	require.NoError(t, cmd.Start())

	fmt.Fprintln(stdinPipe, `{"jsonrpc":"2.0","method":"ping","id":1}`)
	require.True(t, waitForLines(stdout, 1, 5*time.Second), "timed out waiting for response")

	stdinPipe.Close()
	err = cmd.Wait()
	assert.NoError(t, err, "stderr: %s", stderr.String())

	sk := d.SessionKey()
	assert.NotEmpty(t, sk, "session_key should be set on WebSocket upgrade")
}

func TestProxy_E2E_NoArgs(t *testing.T) {
	bin := binaryPath(t)

	cmd := exec.Command(bin)
	stderr := &testutil.SafeBuffer{}
	cmd.Stderr = stderr

	err := cmd.Run()
	assert.Error(t, err)
	assert.Contains(t, stderr.String(), "Usage:")
}

func TestProxy_E2E_ConnectionRefused_Reconnects(t *testing.T) {
	bin := binaryPath(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "ws://127.0.0.1:1/mcp")
	cmd.Stdin = strings.NewReader("")

	stderr := &testutil.SafeBuffer{}
	cmd.Stderr = stderr

	_ = cmd.Run()
	// With reconnect, the proxy retries until the context times out.
	// Verify it printed retry messages to stderr.
	assert.Contains(t, stderr.String(), "retrying")
}

func TestProxy_E2E_HealthCheckSuccess(t *testing.T) {
	bin := binaryPath(t)
	d := testutil.NewMockDaemon()
	defer d.Close()

	cmd := exec.Command(bin, "--health", d.URL())
	stderr := &testutil.SafeBuffer{}
	cmd.Stderr = stderr

	err := cmd.Run()
	assert.NoError(t, err, "stderr: %s", stderr.String())
	assert.Contains(t, stderr.String(), "ok")
}

func TestProxy_E2E_HealthCheckFailure(t *testing.T) {
	bin := binaryPath(t)

	cmd := exec.Command(bin, "--health", "ws://127.0.0.1:1/mcp")
	stderr := &testutil.SafeBuffer{}
	cmd.Stderr = stderr

	err := cmd.Run()
	assert.Error(t, err)
	if exitErr, ok := err.(*exec.ExitError); ok {
		assert.Equal(t, 1, exitErr.ExitCode())
	}
	assert.Contains(t, stderr.String(), "health check failed")
}

func TestProxy_E2E_HealthCheckNoURL(t *testing.T) {
	bin := binaryPath(t)

	cmd := exec.Command(bin, "--health")
	stderr := &testutil.SafeBuffer{}
	cmd.Stderr = stderr

	err := cmd.Run()
	assert.Error(t, err)
	if exitErr, ok := err.(*exec.ExitError); ok {
		assert.Equal(t, 2, exitErr.ExitCode())
	}
	assert.Contains(t, stderr.String(), "Usage")
}

func TestProxy_E2E_MultipleMessages(t *testing.T) {
	bin := binaryPath(t)
	d := testutil.NewMockDaemon()
	defer d.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, d.URL())
	stdinPipe, err := cmd.StdinPipe()
	require.NoError(t, err)

	stdout := &testutil.SafeBuffer{}
	stderr := &testutil.SafeBuffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	require.NoError(t, cmd.Start())

	for i := range 3 {
		fmt.Fprintf(stdinPipe, `{"jsonrpc":"2.0","method":"ping","id":%d}`+"\n", i+1)
	}
	require.True(t, waitForLines(stdout, 3, 5*time.Second), "timed out waiting for 3 responses")

	stdinPipe.Close()
	err = cmd.Wait()
	assert.NoError(t, err, "stderr: %s", stderr.String())

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	assert.Len(t, lines, 3)
}
