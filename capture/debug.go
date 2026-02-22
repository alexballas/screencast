package capture

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	captureDebugEnabledOnce sync.Once
	captureDebugEnabledFlag bool

	captureDebugOutputOnce sync.Once
	captureDebugOutput     io.Writer = os.Stderr

	captureDebugLoggerOnce sync.Once
	captureDebugLogger     *log.Logger
)

func captureDebugEnabled() bool {
	captureDebugEnabledOnce.Do(func() {
		captureDebugEnabledFlag = strings.TrimSpace(os.Getenv("SCREENCAST_DEBUG")) == "1" ||
			strings.TrimSpace(os.Getenv("SCREENCAST_CAPTURE_DEBUG")) == "1"
	})
	return captureDebugEnabledFlag
}

func captureDebugWriter() io.Writer {
	captureDebugOutputOnce.Do(func() {
		p := strings.TrimSpace(os.Getenv("SCREENCAST_CAPTURE_DEBUG_FILE"))
		if p == "" {
			p = strings.TrimSpace(os.Getenv("SCREENCAST_DEBUG_FILE"))
		}
		if p == "" {
			return
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "screencast capture debug log open failed: %v\n", err)
			return
		}
		captureDebugOutput = f
	})
	return captureDebugOutput
}

func captureDebugf(format string, args ...any) {
	if !captureDebugEnabled() {
		return
	}
	captureDebugLoggerOnce.Do(func() {
		captureDebugLogger = log.New(captureDebugWriter(), "screencast/capture ", log.LstdFlags|log.Lmicroseconds)
	})
	captureDebugLogger.Printf(format, args...)
}

func captureShouldLogSlowWrite(last *atomic.Int64, period time.Duration) bool {
	if last == nil || period <= 0 {
		return true
	}

	now := time.Now().UnixNano()
	for {
		prev := last.Load()
		if prev != 0 && time.Duration(now-prev) < period {
			return false
		}
		if last.CompareAndSwap(prev, now) {
			return true
		}
	}
}
