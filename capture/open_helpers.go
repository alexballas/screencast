package capture

import (
	"fmt"
	"io"
	"time"
)

const defaultFirstFrameTimeout = 8 * time.Second

func validateOpenOptions(options *Options) (*Options, error) {
	if options == nil {
		options = &Options{}
	}
	if options.StreamIndex < 0 {
		return nil, fmt.Errorf("%w: StreamIndex must be >= 0", ErrInvalidOptions)
	}
	return options, nil
}

func waitForFirstFrame(platform string, ready <-chan struct{}, onTimeout func() error) error {
	start := time.Now()
	captureDebugf("platform=%s waiting_first_frame timeout=%s", platform, defaultFirstFrameTimeout)

	select {
	case <-ready:
		captureDebugf("platform=%s first_frame_ready elapsed=%s", platform, time.Since(start))
		return nil
	case <-time.After(defaultFirstFrameTimeout):
		captureDebugf("platform=%s first_frame_timeout elapsed=%s", platform, time.Since(start))
		if onTimeout != nil {
			_ = onTimeout()
		}
		return fmt.Errorf("%s capture timed out waiting for first frame", platform)
	}
}

func pipeReaderAsReadCloser(pr *io.PipeReader) io.ReadCloser {
	if pr == nil {
		return nil
	}
	return struct {
		io.Reader
		io.Closer
	}{pr, pr}
}
