package pipeline

import (
	"bytes"
	"strings"
	"sync"
)

// maxStderrCaptureBytes bounds lockedBuffer so a verbose ffmpeg log cannot grow
// without limit over a long session; Tail keeps reading the last n bytes.
const maxStderrCaptureBytes = 2 << 20

// keepStderrCaptureBytes is what a trim leaves behind. Dropping to half the cap
// rather than back to it amortises the copy over the next megabyte of writes,
// instead of memmoving the whole buffer on every write once the cap is reached.
const keepStderrCaptureBytes = maxStderrCaptureBytes / 2

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write appends p, dropping the oldest bytes once the buffer exceeds
// maxStderrCaptureBytes. SCREENCAST_DEBUG adds -loglevel debug, which fills a
// megabyte in seconds, and a screencast can run for hours.
func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	n, err := b.buf.Write(p)
	if b.buf.Len() > maxStderrCaptureBytes {
		// Write copies with memmove and the buffer already has the capacity, so
		// reusing it in place is safe even though tail aliases its storage.
		tail := b.buf.Bytes()[b.buf.Len()-keepStderrCaptureBytes:]
		b.buf.Reset()
		_, _ = b.buf.Write(tail)
	}
	return n, err
}

func (b *lockedBuffer) Tail(n int) string {
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
