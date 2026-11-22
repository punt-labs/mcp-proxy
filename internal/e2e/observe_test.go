//go:build e2e

package e2e_test

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/punt-labs/mcp-proxy/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func installForSmoke(t *testing.T, home string) string {
	t.Helper()
	binDir := filepath.Join(home, "bin")
	install := exec.CommandContext(t.Context(), "go", "install", "./cmd/mcp-proxy")
	install.Dir = findModuleRoot(t)
	install.Env = append(os.Environ(), "CGO_ENABLED=0", "GOBIN="+binDir)
	out, err := install.CombinedOutput()
	require.NoError(t, err, "%s", out)
	return filepath.Join(binDir, "mcp-proxy")
}

func TestObservability_E2E(t *testing.T) {
	home := t.TempDir()
	bin := installForSmoke(t, home)
	stateDir := filepath.Join(home, ".punt-labs", "mcp-proxy")
	logDir := filepath.Join(stateDir, "logs")
	logPath := filepath.Join(logDir, "mcp-proxy.log")
	const maxSize = 5 * 1024 * 1024
	const seed = "rotation fixture\n"
	require.NoError(t, os.MkdirAll(logDir, 0o755))
	require.NoError(t, os.WriteFile(logPath, []byte(strings.Repeat(seed, maxSize/len(seed))), 0o600))
	require.NoError(t, os.Chmod(logPath, 0o644))

	d := testutil.NewMockDaemon()
	defer d.Close()
	d.Handler = func(msg []byte) []byte {
		if strings.Contains(string(msg), "hook/") {
			return []byte(`{"jsonrpc":"2.0","id":1,"result":{"decision":"allow"}}`)
		}
		return msg
	}
	appendSecrets := func(url string) string { return url + "?owner=synthetic-user-marker" }
	env := append(os.Environ(), "HOME="+home, "PUNT_TELEMETRY=", "MCP_PROXY_TELEMETRY=", "MCP_PROXY_DEBUG=true", "MCP_PROXY_TOKEN=synthetic-token-marker")

	proxy := exec.CommandContext(t.Context(), bin, appendSecrets(d.URL()))
	proxy.Env = append(env, "MCP_PROXY_PING_INTERVAL=synthetic-duration-marker")
	stdin, err := proxy.StdinPipe()
	require.NoError(t, err)
	stdout := &testutil.SafeBuffer{}
	proxy.Stdout = stdout
	require.NoError(t, proxy.Start())
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = proxy.Process.Kill()
		_ = proxy.Wait()
	})
	const init = `{"jsonrpc":"2.0","method":"initialize","id":"synthetic-id-marker","params":{"user":"synthetic-payload-marker"}}`
	_, err = fmt.Fprintln(stdin, init)
	require.NoError(t, err)
	require.True(t, waitForLines(stdout, 1, 5*time.Second), "initialize response not forwarded")
	d.CloseConn()
	require.Eventually(t, func() bool { return d.ConnCount() == 2 && len(d.Received()) == 2 }, 5*time.Second, 10*time.Millisecond)
	const ping = `{"jsonrpc":"2.0","method":"ping","id":2}`
	_, err = fmt.Fprintln(stdin, ping)
	require.NoError(t, err)
	require.True(t, waitForLines(stdout, 2, 5*time.Second), "reconnected ping response not forwarded")
	require.NoError(t, stdin.Close())
	require.NoError(t, proxy.Wait())
	assert.Equal(t, init+"\n"+ping+"\n", stdout.String())

	hook := exec.CommandContext(t.Context(), bin, appendSecrets(d.BaseURL()), "--hook", "synthetic-hook-marker")
	hook.Env = env
	hook.Stdin = strings.NewReader(`{"user":"synthetic-payload-marker"}`)
	out, err := hook.Output()
	require.NoError(t, err)
	assert.JSONEq(t, `{"decision":"allow"}`, string(out))

	ok := exec.CommandContext(t.Context(), bin, "--health", appendSecrets(d.URL()))
	ok.Env = append(env, "MCP_PROXY_DEBUG=")
	out, err = ok.Output()
	require.NoError(t, err)
	assert.Empty(t, out)

	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "synthetic-rejection-marker", http.StatusUnauthorized)
	}))
	defer denied.Close()
	health := exec.CommandContext(t.Context(), bin, "--health", "ws"+strings.TrimPrefix(denied.URL, "http")+"/synthetic-path-marker")
	health.Env = env
	out, err = health.Output()
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit)
	assert.Equal(t, 1, exit.ExitCode())
	assert.Empty(t, out)

	for _, dir := range []string{stateDir, logDir} {
		info, statErr := os.Stat(dir)
		require.NoError(t, statErr)
		assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	}
	entries, err := os.ReadDir(logDir)
	require.NoError(t, err)
	require.Len(t, entries, 3)
	for _, entry := range entries {
		info, statErr := entry.Info()
		require.NoError(t, statErr)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
		assert.LessOrEqual(t, info.Size(), int64(maxSize))
		body, readErr := os.ReadFile(filepath.Join(logDir, entry.Name()))
		require.NoError(t, readErr)
		require.False(t, strings.Contains(string(body), "synthetic-"), "%s contains user content", entry.Name())
		t.Logf("%s mode=%04o bytes=%d", entry.Name(), info.Mode().Perm(), info.Size())
	}
	assert.FileExists(t, logPath+".1")
	diagnostics, err := os.ReadFile(logPath)
	require.NoError(t, err)
	for _, line := range []string{
		"[WARNING] mcp-proxy.proxy: Rejected duration setting",
		"[INFO] mcp-proxy.reconnect: Connected to daemon",
		"[WARNING] mcp-proxy.reconnect: Daemon connection interrupted",
		"[INFO] mcp-proxy.reconnect: Handshake replay finished outcome=success",
		"[INFO] mcp-proxy.hook: Operation finished outcome=success",
		"[WARNING] mcp-proxy.transport: Daemon rejected WebSocket authorization status=401",
		"[ERROR] mcp-proxy.health: Operation failed unexpectedly",
		"[INFO] mcp-proxy.health: Operation finished outcome=failure",
	} {
		assert.Contains(t, string(diagnostics), line)
	}
	t.Logf("Diagnostics:\n%s", diagnostics)
	checkUsage(t, stateDir)
}

func checkUsage(t *testing.T, stateDir string) {
	t.Helper()
	dbPath := filepath.Join(stateDir, "telemetry.db")
	info, err := os.Stat(dbPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	db, err := sql.Open("sqlite", dbPath+"?mode=ro")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(t.Context(), "SELECT signal, name, schema_version, end_ns - start_ns, attributes FROM signals")
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var observations []string
	for rows.Next() {
		var signal, name, attrs string
		var version int
		var duration int64
		require.NoError(t, rows.Scan(&signal, &name, &version, &duration, &attrs))
		assert.Equal(t, "span", signal)
		assert.Equal(t, 1, version)
		assert.Positive(t, duration)
		observations = append(observations, name+" "+attrs)
		t.Logf("SQLite %s %s v%d duration_ns=%d %s", signal, name, version, duration, attrs)
	}
	require.NoError(t, rows.Err())
	assert.ElementsMatch(t, []string{
		`mcp-proxy.proxy.run {"outcome":"success"}`,
		`mcp-proxy.hook.run {"outcome":"success"}`,
		`mcp-proxy.health.run {"outcome":"success"}`,
		`mcp-proxy.health.run {"outcome":"failure"}`,
	}, observations)
}

func TestTelemetryOff_E2E(t *testing.T) {
	bin := binaryPath(t)
	d := testutil.NewMockDaemon()
	defer d.Close()
	for _, setting := range []string{"PUNT_TELEMETRY", "MCP_PROXY_TELEMETRY", "config", "invalid_config", "invalid_retention"} {
		t.Run(setting, func(t *testing.T) {
			home := t.TempDir()
			stateDir := filepath.Join(home, ".punt-labs", "mcp-proxy")
			args := []string{"--health", d.URL()}
			env := append(os.Environ(), "HOME="+home, "PUNT_TELEMETRY=", "MCP_PROXY_TELEMETRY=")
			switch setting {
			case "config", "invalid_config", "invalid_retention":
				toml := "disabled = true\n"
				switch setting {
				case "invalid_config":
					toml = "disabled = \"unexpected\"\n"
				case "invalid_retention":
					toml = "retention_days = 3651\n"
				}
				require.NoError(t, os.MkdirAll(stateDir, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(stateDir, "telemetry.conf"), []byte(toml), 0o600))
			default:
				env = append(env, setting+"=off")
			}
			cmd := exec.CommandContext(t.Context(), bin, args...)
			cmd.Env = env
			out, err := cmd.Output()
			require.NoError(t, err)
			assert.Empty(t, out)
			assert.NoFileExists(t, filepath.Join(stateDir, "telemetry.db"))
			assert.FileExists(t, filepath.Join(stateDir, "logs", "mcp-proxy.log"))
		})
	}
}

func TestDiagnosticsUnavailable_E2E(t *testing.T) {
	bin := binaryPath(t)
	d := testutil.NewMockDaemon()
	defer d.Close()
	home := t.TempDir()
	logDir := filepath.Join(home, ".punt-labs", "mcp-proxy", "logs")
	require.NoError(t, os.MkdirAll(filepath.Dir(logDir), 0o700))
	require.NoError(t, os.WriteFile(logDir, nil, 0o600))

	cmd := exec.CommandContext(t.Context(), bin, "--health", d.URL())
	cmd.Env = append(os.Environ(), "HOME="+home, "PUNT_TELEMETRY=off")
	stderr := &testutil.SafeBuffer{}
	cmd.Stderr = stderr
	out, err := cmd.Output()
	require.NoError(t, err)
	assert.Empty(t, out)
	assert.Contains(t, stderr.String(), "warning: could not initialize private diagnostics")
	assert.Contains(t, stderr.String(), "mcp-proxy: ok")
}
