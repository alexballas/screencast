package pipeline

import (
	"bytes"
	"strings"
	"sync"
)

type LockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *LockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

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
