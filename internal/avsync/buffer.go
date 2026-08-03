package avsync

import (
	"bytes"
	"strings"
	"sync"
)

// LockedBuffer is a concurrency-safe stderr capture with a tail window.
type LockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// NewLockedBuffer returns an empty locked buffer.
func NewLockedBuffer() *LockedBuffer {
	return &LockedBuffer{}
}

// Write appends p to the buffer.
func (b *LockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
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
