package main

import (
	"bytes"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestReportError_Silent pins the *silentError contract: the leader
// suppresses its own "mcp-proxy: <err>" prefix line because the wrapped
// operation (currently hook.Run on hook.ErrDaemonError) has already
// written its own diagnostic. A future refactor that inverts the switch
// order or drops the errors.As branch would land silently — reviewers
// would just see a double diagnostic on daemon errors instead of one.
func TestReportError_Silent(t *testing.T) {
	var stderr bytes.Buffer
	code := reportError(&silentError{err: errors.New("underlying diag")}, &stderr)

	assert.Equal(t, 1, code, "silentError must exit 1")
	got := stderr.String()
	assert.NotContains(t, got, "mcp-proxy: underlying diag",
		"leader-added prefix must be suppressed")
	assert.NotContains(t, got, "underlying diag",
		"silent path means the leader does not print the message at all")
}

// TestReportError_Nil covers the trivial success path.
func TestReportError_Nil(t *testing.T) {
	var stderr bytes.Buffer
	code := reportError(nil, &stderr)
	assert.Equal(t, 0, code)
	assert.Empty(t, stderr.String())
}

// TestReportError_Usage confirms the usage branch prints the prefix and
// the short usage block, and exits 2.
func TestReportError_Usage(t *testing.T) {
	var stderr bytes.Buffer
	code := reportError(&usageError{msg: "bad flag"}, &stderr)
	assert.Equal(t, 2, code)
	got := stderr.String()
	assert.Contains(t, got, "mcp-proxy: bad flag")
	assert.Contains(t, got, "Usage:")
}

// TestReportError_Runtime covers a plain runtime error: exit 1 with the
// leader-added "mcp-proxy: <err>" prefix.
func TestReportError_Runtime(t *testing.T) {
	var stderr bytes.Buffer
	code := reportError(errors.New("boom"), &stderr)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr.String(), "mcp-proxy: boom")
}
