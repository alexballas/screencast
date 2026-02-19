package capture

import (
	"errors"
	"io"
)

const (
	// PixelFormatBGRA is the unified output pixel format across all platforms.
	PixelFormatBGRA = "BGRA"
)

var (
	ErrNotImplemented = errors.New("screen capture backend is not implemented on this platform")
	ErrCancelled      = errors.New("screen capture request was cancelled")
	ErrNoStreams      = errors.New("screen capture returned no streams")
	ErrInvalidOptions = errors.New("invalid screen capture options")
)

// Options configures a capture session.
type Options struct {
	// StreamIndex selects the stream from the OS chooser result. Default is 0.
	StreamIndex int
}

// Stream is a unified raw frame source. Read yields raw BGRA bytes.
type Stream struct {
	io.ReadCloser

	Width       uint32
	Height      uint32
	FrameRate   uint32
	PixelFormat string
}

// Open initializes an OS-specific screen capture backend and returns a unified
// BGRA frame reader.
func Open(options *Options) (*Stream, error) {
	return open(options)
}
