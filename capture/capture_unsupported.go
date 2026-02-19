//go:build !linux && !darwin && !windows

package capture

import "fmt"

func open(options *Options) (*Stream, error) {
	_ = options
	return nil, fmt.Errorf("%w: no backend for this operating system", ErrNotImplemented)
}
