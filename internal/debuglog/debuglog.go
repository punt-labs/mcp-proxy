// Package debuglog provides structured debug logging for mcp-proxy.
//
// Debug logging is off by default and enabled via the MCP_PROXY_DEBUG
// environment variable:
//
//   - Not set or empty: no debug logging (Nop logger).
//   - "1" or "true": log to $TMPDIR/mcp-proxy-<pid>.log.
//   - Any other value: treated as a file path.
//
// All log entries use slog.LevelDebug — when disabled, calls are zero-cost.
package debuglog

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// FromEnv creates a logger based on MCP_PROXY_DEBUG. Returns the logger
// and a closer that must be called to flush and close the log file.
// If debug logging is disabled, the closer is a no-op.
func FromEnv() (*slog.Logger, io.Closer) {
	val := os.Getenv("MCP_PROXY_DEBUG")
	if val == "" {
		return Nop(), io.NopCloser(nil)
	}

	var path string
	if val == "1" || val == "true" {
		path = filepath.Join(os.TempDir(), fmt.Sprintf("mcp-proxy-%d.log", os.Getpid()))
	} else {
		path = val
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		// Can't open log file — fall back to nop rather than failing.
		return Nop(), io.NopCloser(nil)
	}

	logger := New(f)
	logger.Debug("debug logging enabled", "path", path, "pid", os.Getpid())
	return logger, f
}

// New creates a debug-level text logger writing to w.
func New(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
}

// Nop returns a logger that discards all output.
func Nop() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestLogger wraps a *slog.Logger and captures output for test assertions.
// Log output is sent to t.Log so it appears only when tests fail or with -v.
type TestLogger struct {
	*slog.Logger
	buf *safeLogBuffer
}

// NewTestLogger creates a logger that writes to the test's log output.
func NewTestLogger(t interface{ Log(args ...any) }) *TestLogger {
	buf := &safeLogBuffer{}
	w := io.MultiWriter(buf, &testLogWriter{t: t})
	logger := slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	return &TestLogger{Logger: logger, buf: buf}
}

// Output returns all log output captured so far.
func (tl *TestLogger) Output() string {
	return tl.buf.String()
}

type testLogWriter struct {
	t interface{ Log(args ...any) }
}

func (w *testLogWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}

type safeLogBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *safeLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *safeLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
