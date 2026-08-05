package avsync

import (
	"bytes"
	"strings"
	"sync"
)

// maxStderrCaptureBytes bounds LockedBuffer so a verbose ffmpeg log cannot grow
// without limit over a long session; Tail keeps reading the last n bytes.
const maxStderrCaptureBytes = 2 << 20

// LockedBuffer is a concurrency-safe stderr capture with a tail window.
type LockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// NewLockedBuffer returns an empty locked buffer.
func NewLockedBuffer() *LockedBuffer {
	return &LockedBuffer{}
}

// Write appends p to the buffer, dropping the oldest bytes once the buffer
// exceeds maxStderrCaptureBytes.
func (b *LockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	n, err := b.buf.Write(p)
	if b.buf.Len() > maxStderrCaptureBytes {
		tail := b.buf.Bytes()[b.buf.Len()-maxStderrCaptureBytes:]
		var trimmed bytes.Buffer
		_, _ = trimmed.Write(tail)
		b.buf = trimmed
	}
	return n, err
}

// Tail returns the last n bytes, or the whole buffer when shorter.
func (b *LockedBuffer) Tail(n int) string {
	b.mu.Lock()
	defer b.mu.Unlock()

	s := strings.TrimSpace(b.buf.String())
	if s == "" {
		return "no ffmpeg stderr output"
	}
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
