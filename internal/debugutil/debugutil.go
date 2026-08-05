// Package debugutil centralizes the SCREENCAST_DEBUG environment-driven debug
// logging shared by the hls and dlna pipelines.
package debugutil

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
)

// Enabled reports whether SCREENCAST_DEBUG=1 turns on diagnostic logging.
func Enabled() bool {
	return strings.TrimSpace(os.Getenv("SCREENCAST_DEBUG")) == "1"
}

var (
	outputOnce sync.Once
	output     io.Writer = os.Stderr

	loggerOnce sync.Once
	logger     *log.Logger
)

// Output returns the debug log destination, honoring SCREENCAST_DEBUG_FILE.
func Output() io.Writer {
	outputOnce.Do(func() {
		p := strings.TrimSpace(os.Getenv("SCREENCAST_DEBUG_FILE"))
		if p == "" {
			return
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "screencast debug log open failed: %v\n", err)
			return
		}
		output = f
	})
	return output
}

// Printf writes a debug log line to the debug output.
func Printf(format string, args ...any) {
	loggerOnce.Do(func() {
		logger = log.New(Output(), "", log.LstdFlags|log.Lmicroseconds)
	})
	logger.Printf(format, args...)
}

// MergeWriter combines w with the debug output so debug lines also land in the
// caller's logger when SCREENCAST_DEBUG is active.
func MergeWriter(w io.Writer) io.Writer {
	out := Output()
	if w == nil {
		return out
	}
	return io.MultiWriter(w, out)
}
