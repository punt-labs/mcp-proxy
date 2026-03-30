package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/punt-labs/mcp-proxy/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeConfig creates a temp TOML file at the expected profile path inside a
// temporary home directory and returns the home dir path.
func writeConfig(t *testing.T, profile, content string, mode os.FileMode) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".punt-labs", "mcp-proxy")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, profile+".toml")
	require.NoError(t, os.WriteFile(path, []byte(content), mode))
	return home
}

// setHome overrides os.UserHomeDir by setting HOME (Unix) for the duration of
// the test.
func setHome(t *testing.T, home string) {
	t.Helper()
	orig := os.Getenv("HOME")
	t.Setenv("HOME", home)
	t.Cleanup(func() { os.Setenv("HOME", orig) })
}

func TestLoad_FileNotFound_Fallback(t *testing.T) {
	// Point HOME at a fresh dir with no config file.
	setHome(t, t.TempDir())

	p, err := config.Load("quarry")
	require.NoError(t, err)
	assert.Empty(t, p.URL)
	assert.Nil(t, p.Headers)
}

func TestLoad_InsecurePermissions_Error(t *testing.T) {
	home := writeConfig(t, "quarry", `[quarry]
url = "ws://example.com/mcp"
`, 0o644)
	setHome(t, home)

	_, err := config.Load("quarry")
	require.Error(t, err)

	var permErr *config.InsecurePermissionsError
	require.ErrorAs(t, err, &permErr)
	assert.Contains(t, permErr.Error(), "insecure permissions")
	assert.Contains(t, permErr.Error(), "quarry.toml")
}

func TestLoad_MissingSection_Fallback(t *testing.T) {
	home := writeConfig(t, "quarry", `[other]
url = "ws://other.example.com/mcp"
`, 0o600)
	setHome(t, home)

	p, err := config.Load("quarry")
	require.NoError(t, err)
	assert.Empty(t, p.URL)
	assert.Nil(t, p.Headers)
}

func TestLoad_ValidFile_URLAndHeaders(t *testing.T) {
	home := writeConfig(t, "quarry", `[quarry]
url = "ws://okinos.user.home.lab:8420/mcp"

[quarry.headers]
Authorization = "Bearer test-token-123"
X-Custom = "hello"
`, 0o600)
	setHome(t, home)

	p, err := config.Load("quarry")
	require.NoError(t, err)
	assert.Equal(t, "ws://okinos.user.home.lab:8420/mcp", p.URL)
	require.NotNil(t, p.Headers)
	assert.Equal(t, "Bearer test-token-123", p.Headers["Authorization"])
	assert.Equal(t, "hello", p.Headers["X-Custom"])
}

func TestLoad_ValidFile_URLOnly(t *testing.T) {
	home := writeConfig(t, "myprofile", `[myprofile]
url = "ws://remote.host:9999/mcp"
`, 0o600)
	setHome(t, home)

	p, err := config.Load("myprofile")
	require.NoError(t, err)
	assert.Equal(t, "ws://remote.host:9999/mcp", p.URL)
	assert.Empty(t, p.Headers)
}

func TestInsecurePermissionsError_IsTyped(t *testing.T) {
	home := writeConfig(t, "q", `[q]
url = "ws://x/mcp"
`, 0o666)
	setHome(t, home)

	_, err := config.Load("q")
	require.Error(t, err)

	var permErr *config.InsecurePermissionsError
	assert.True(t, errors.As(err, &permErr))
}
