//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/punt-labs/mcp-proxy/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// quarryServe starts "uv run quarry serve" from the quarry dev checkout
// and returns the WebSocket URL and a cleanup function.
// The quarry directory must be at ../quarry relative to the mcp-proxy module root.
func quarryServe(t *testing.T) (wsURL string, cleanup func()) {
	t.Helper()

	quarryDir := findQuarryDir(t)
	dbDir := t.TempDir()
	dbName := "integration-test"

	// Create database directory so quarry can write the port file.
	dbPath := filepath.Join(dbDir, dbName)
	require.NoError(t, os.MkdirAll(dbPath, 0o755))

	portFile := filepath.Join(dbPath, "serve.port")

	cmd := exec.Command("uv", "run", "quarry", "--db", dbName, "serve", "--port", "0")
	cmd.Dir = quarryDir
	cmd.Env = append(os.Environ(), "QUARRY_ROOT="+dbDir)

	stderr := &testutil.SafeBuffer{}
	cmd.Stderr = stderr
	cmd.Stdout = stderr // quarry logs to stderr, but capture both

	require.NoError(t, cmd.Start(), "failed to start quarry serve")

	cleanup = func() {
		if cmd.Process != nil {
			cmd.Process.Signal(os.Interrupt)
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				cmd.Process.Kill()
				<-done
			}
		}
	}

	// Wait for the port file to appear.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(portFile); err == nil && len(data) > 0 {
			port := strings.TrimSpace(string(data))
			return fmt.Sprintf("ws://127.0.0.1:%s/mcp", port), cleanup
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Dump stderr for debugging before failing.
	t.Logf("quarry stderr:\n%s", stderr.String())
	cleanup()
	t.Fatal("quarry serve did not write port file within 30s")
	return "", nil
}

func findQuarryDir(t *testing.T) string {
	t.Helper()

	// Walk up from the mcp-proxy module root to find ../quarry.
	dir, err := os.Getwd()
	require.NoError(t, err)

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			quarryDir := filepath.Join(dir, "..", "quarry")
			if info, err := os.Stat(quarryDir); err == nil && info.IsDir() {
				return quarryDir
			}
			t.Skip("quarry directory not found at ../quarry — skipping integration test")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("could not find module root — skipping integration test")
		}
		dir = parent
	}
}

func proxyBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	binPath := filepath.Join(dir, "mcp-proxy")

	// Find module root.
	wd, err := os.Getwd()
	require.NoError(t, err)
	modRoot := wd
	for {
		if _, err := os.Stat(filepath.Join(modRoot, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(modRoot)
		require.NotEqual(t, parent, modRoot, "could not find module root")
		modRoot = parent
	}

	cmd := exec.Command("go", "build", "-o", binPath, ".")
	cmd.Dir = modRoot
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "go build failed: %s", out)
	return binPath
}

func TestQuarry_Initialize(t *testing.T) {
	wsURL, cleanup := quarryServe(t)
	defer cleanup()

	bin := proxyBinary(t)

	cmd := exec.Command(bin, wsURL)
	stdinPipe, err := cmd.StdinPipe()
	require.NoError(t, err)

	stdout := &testutil.SafeBuffer{}
	stderr := &testutil.SafeBuffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	require.NoError(t, cmd.Start())
	defer func() {
		stdinPipe.Close()
		cmd.Wait()
	}()

	// Send MCP initialize request.
	initReq := `{"jsonrpc":"2.0","method":"initialize","id":1,"params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"mcp-proxy-integration","version":"0.1.0"}}}`
	fmt.Fprintln(stdinPipe, initReq)

	// Wait for initialize response.
	require.True(t, waitForLines(stdout, 1, 10*time.Second),
		"timed out waiting for initialize response; stderr: %s", stderr.String())

	line := strings.TrimSpace(strings.Split(stdout.String(), "\n")[0])
	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(line), &resp), "invalid JSON response: %s", line)

	assert.Equal(t, "2.0", resp["jsonrpc"])
	assert.Equal(t, float64(1), resp["id"])

	result, ok := resp["result"].(map[string]any)
	require.True(t, ok, "expected result object, got: %v", resp)

	serverInfo, ok := result["serverInfo"].(map[string]any)
	require.True(t, ok, "expected serverInfo object")
	assert.Equal(t, "punt-quarry", serverInfo["name"])
}

func TestQuarry_ToolsList(t *testing.T) {
	wsURL, cleanup := quarryServe(t)
	defer cleanup()

	bin := proxyBinary(t)

	cmd := exec.Command(bin, wsURL)
	stdinPipe, err := cmd.StdinPipe()
	require.NoError(t, err)

	stdout := &testutil.SafeBuffer{}
	stderr := &testutil.SafeBuffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	require.NoError(t, cmd.Start())
	defer func() {
		stdinPipe.Close()
		cmd.Wait()
	}()

	// Initialize first (required by MCP protocol).
	fmt.Fprintln(stdinPipe, `{"jsonrpc":"2.0","method":"initialize","id":1,"params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"mcp-proxy-integration","version":"0.1.0"}}}`)
	require.True(t, waitForLines(stdout, 1, 10*time.Second),
		"timed out waiting for initialize response; stderr: %s", stderr.String())

	// Send initialized notification (no response expected).
	fmt.Fprintln(stdinPipe, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	// Send tools/list request.
	fmt.Fprintln(stdinPipe, `{"jsonrpc":"2.0","method":"tools/list","id":2}`)
	require.True(t, waitForLines(stdout, 2, 10*time.Second),
		"timed out waiting for tools/list response; stderr: %s", stderr.String())

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	require.GreaterOrEqual(t, len(lines), 2, "expected at least 2 response lines")

	// Parse the tools/list response (last line).
	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[len(lines)-1]), &resp), "invalid JSON: %s", lines[len(lines)-1])

	assert.Equal(t, float64(2), resp["id"])

	result, ok := resp["result"].(map[string]any)
	require.True(t, ok, "expected result object")

	tools, ok := result["tools"].([]any)
	require.True(t, ok, "expected tools array")
	assert.GreaterOrEqual(t, len(tools), 5, "quarry should have at least 5 tools (find, ingest, list, show, status, ...)")

	// Verify some known tool names.
	toolNames := make([]string, 0, len(tools))
	for _, t := range tools {
		if tool, ok := t.(map[string]any); ok {
			if name, ok := tool["name"].(string); ok {
				toolNames = append(toolNames, name)
			}
		}
	}
	assert.Contains(t, toolNames, "find")
	assert.Contains(t, toolNames, "status")
}

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
