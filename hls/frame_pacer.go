package hls

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"go2tv.app/screencast/capture"
)

const bytesPerPixelBGRA = 4

type framePacer struct {
	src       io.ReadCloser
	pr        *io.PipeReader
	pw        *io.PipeWriter
	closeOnce sync.Once
	closeErr  error
}

func newFramePacer(stream *capture.Stream, fps uint32) (io.ReadCloser, error) {
	if stream == nil || stream.ReadCloser == nil {
		return nil, errors.New("nil stream")
	}
	if fps == 0 {
		return stream, nil
	}

	frameSize, err := rawFrameSize(stream.Width, stream.Height, stream.PixelFormat)
	if err != nil {
		return nil, err
	}
	if frameSize == 0 {
		return stream, nil
	}

	pr, pw := io.Pipe()
	p := &framePacer{
		src: stream,
		pr:  pr,
		pw:  pw,
	}

	go p.run(frameSize, fps)
	if envDebugEnabled() {
		envDebugPrintf(
			"screencast/hls frame_pacer enabled width=%d height=%d fps=%d frame_bytes=%d",
			stream.Width,
			stream.Height,
			fps,
			frameSize,
		)
	}

	return p, nil
}

func rawFrameSize(width, height uint32, pixelFormat string) (int, error) {
	if width == 0 || height == 0 {
		return 0, errors.New("invalid raw frame size")
	}

	switch pixelFormat {
	case "", capture.PixelFormatBGRA:
		size := uint64(width) * uint64(height) * bytesPerPixelBGRA
		if size == 0 || size > uint64(^uint(0)>>1) {
			return 0, fmt.Errorf("raw frame too large: %dx%d", width, height)
		}
		return int(size), nil
	default:
		return 0, fmt.Errorf("unsupported raw pixel format %q", pixelFormat)
	}
}

func (p *framePacer) Read(buf []byte) (int, error) {
	return p.pr.Read(buf)
}

func (p *framePacer) Close() error {
	p.closeOnce.Do(func() {
		p.closeErr = errors.Join(p.src.Close(), p.pw.Close(), p.pr.Close())
	})
	return p.closeErr
}

func (p *framePacer) run(frameSize int, fps uint32) {
	frameCh := make(chan []byte, 1)
	srcErrCh := make(chan error, 1)

	go func() {
		for {
			frame := make([]byte, frameSize)
			if _, err := io.ReadFull(p.src, frame); err != nil {
				srcErrCh <- err
				close(frameCh)
				return
			}

			select {
			case frameCh <- frame:
			default:
				select {
				case <-frameCh:
				default:
				}
				frameCh <- frame
			}
		}
	}()

	frameInterval := time.Second / time.Duration(fps)
	if frameInterval <= 0 {
		frameInterval = time.Second / time.Duration(defaultMaxFrameRate)
	}

	var (
		latest []byte
		srcErr error
	)

	waitForFirst := true
	for waitForFirst {
		select {
		case frame, ok := <-frameCh:
			if !ok {
				_ = p.pw.CloseWithError(srcErr)
				return
			}
			latest = frame
			waitForFirst = false
		case srcErr = <-srcErrCh:
			_ = p.pw.CloseWithError(srcErr)
			return
		}
	}

	if _, err := p.pw.Write(latest); err != nil {
		_ = p.pw.CloseWithError(err)
		return
	}

	ticker := time.NewTicker(frameInterval)
	defer ticker.Stop()

	for {
		select {
		case srcErr = <-srcErrCh:
			_ = p.pw.CloseWithError(srcErr)
			return
		case frame, ok := <-frameCh:
			if ok {
				latest = frame
			}
		case <-ticker.C:
			select {
			case frame, ok := <-frameCh:
				if ok {
					latest = frame
				}
			default:
			}
			if _, err := p.pw.Write(latest); err != nil {
				_ = p.pw.CloseWithError(err)
				return
			}
		}
	}
}
