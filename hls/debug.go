package hls

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
)

func envDebugEnabled() bool {
	return strings.TrimSpace(os.Getenv("SCREENCAST_DEBUG")) == "1"
}

var (
	debugOutputOnce sync.Once
	debugOutput     io.Writer = os.Stderr

	debugLoggerOnce sync.Once
	debugLogger     *log.Logger
)

func envDebugOutput() io.Writer {
	debugOutputOnce.Do(func() {
		p := strings.TrimSpace(os.Getenv("SCREENCAST_DEBUG_FILE"))
		if p == "" {
			return
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "screencast debug log open failed: %v\n", err)
			return
		}
		debugOutput = f
	})
	return debugOutput
}

func envDebugPrintf(format string, args ...any) {
	debugLoggerOnce.Do(func() {
		debugLogger = log.New(envDebugOutput(), "", log.LstdFlags|log.Lmicroseconds)
	})
	debugLogger.Printf(format, args...)
}
