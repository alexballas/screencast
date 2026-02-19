//go:build darwin

package capture

import "fmt"

// macOS implementation target: ScreenCaptureKit (SCShareableContent + SCStream).
func open(options *Options) (*Stream, error) {
	_ = options
	return nil, fmt.Errorf("%w: use ScreenCaptureKit (SCStream) to emit %s frames", ErrNotImplemented, PixelFormatBGRA)
}
