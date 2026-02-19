//go:build windows

package capture

import "fmt"

// Windows implementation target: Windows Graphics Capture (WinRT GraphicsCaptureItem).
func open(options *Options) (*Stream, error) {
	_ = options
	return nil, fmt.Errorf("%w: use Windows Graphics Capture to emit %s frames", ErrNotImplemented, PixelFormatBGRA)
}
