package hls

import (
	"io"

	"go2tv.app/screencast/internal/debugutil"
)

func envDebugEnabled() bool {
	return debugutil.Enabled()
}

func envDebugOutput() io.Writer {
	return debugutil.Output()
}

func envDebugPrintf(format string, args ...any) {
	debugutil.Printf(format, args...)
}

func mergeDebugWriter(w io.Writer) io.Writer {
	return debugutil.MergeWriter(w)
}
