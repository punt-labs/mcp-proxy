package testutil

import (
	"bytes"
	"io"
	"sync"
)

// StdioPair creates a simulated stdin/stdout pair for testing the bridge.
// Write lines to stdinWriter to simulate stdin input.
// Read from stdoutReader to capture stdout output.
func StdioPair() (stdinReader io.Reader, stdinWriter io.WriteCloser, stdoutReader *SafeBuffer, stdoutWriter io.Writer) {
	pr, pw := io.Pipe()
	buf := &SafeBuffer{}
	return pr, pw, buf, buf
}

// SafeBuffer is a concurrency-safe bytes.Buffer for capturing stdout output.
type SafeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *SafeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// Bytes returns a copy of the buffer contents.
func (b *SafeBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]byte, b.buf.Len())
	copy(out, b.buf.Bytes())
	return out
}

// String returns the buffer contents as a string.
func (b *SafeBuffer) String() string {
	return string(b.Bytes())
}

// Len returns the number of bytes in the buffer.
func (b *SafeBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}
