package testutil

import (
	"log/slog"
	"testing"
)

// TestLogger wraps a *slog.Logger that writes to the test log.
type TestLogger struct {
	*slog.Logger
}

// NewTestLogger creates a logger that writes to the test's log output.
func NewTestLogger(t testing.TB) *TestLogger {
	w := &testLogWriter{t: t}
	logger := slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	return &TestLogger{Logger: logger}
}

type testLogWriter struct {
	t testing.TB
}

func (w *testLogWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}
