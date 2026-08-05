package avsync

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"go2tv.app/screencast/capture"
	"go2tv.app/screencast/internal/debugutil"
)

// Pacer throttles raw frames from a damage-driven capture source to a steady
// target frame rate, repeating the latest frame when the source delivers fewer
// than the wall clock demands and dropping stale frames when ffmpeg falls
// behind. ffmpeg derives video pts from the frame count, so pacing on every
// platform keeps the video timeline tracking real time and in step with the
// audio relay.
type Pacer struct {
	src       io.ReadCloser
	pr        *io.PipeReader
	pw        *io.PipeWriter
	skew      *Skew
	closeOnce sync.Once
	closeErr  error
}

// NewPacer wraps a capture stream so it yields one frame per frame interval. It
// returns the stream unchanged when pacing is unnecessary (no target fps or an
// unmeasurable frame size).
func NewPacer(stream *capture.Stream, fps uint32, skew *Skew) (io.ReadCloser, error) {
	if stream == nil || stream.ReadCloser == nil {
		return nil, errors.New("nil stream")
	}
	if fps == 0 {
		return stream, nil
	}

	frameSize, err := RawFrameSize(stream.Width, stream.Height, stream.PixelFormat)
	if err != nil {
		return nil, err
	}
	if frameSize == 0 {
		return stream, nil
	}

	pr, pw := io.Pipe()
	p := &Pacer{
		src:  stream,
		pr:   pr,
		pw:   pw,
		skew: skew,
	}

	go p.run(frameSize, fps)
	if debugutil.Enabled() {
		debugutil.Printf(
			"screencast/avsync frame_pacer enabled width=%d height=%d fps=%d frame_bytes=%d",
			stream.Width,
			stream.Height,
			fps,
			frameSize,
		)
	}

	return p, nil
}

// RawFrameSize reports the byte size of one BGRA frame at the given dimensions.
func RawFrameSize(width, height uint32, pixelFormat string) (int, error) {
	if width == 0 || height == 0 {
		return 0, errors.New("invalid raw frame size")
	}

	switch pixelFormat {
	case "", capture.PixelFormatBGRA:
		size := uint64(width) * uint64(height) * BytesPerPixelBGRA
		if size == 0 || size > uint64(^uint(0)>>1) {
			return 0, fmt.Errorf("raw frame too large: %dx%d", width, height)
		}
		return int(size), nil
	default:
		return 0, fmt.Errorf("unsupported raw pixel format %q", pixelFormat)
	}
}

func (p *Pacer) Read(buf []byte) (int, error) {
	return p.pr.Read(buf)
}

func (p *Pacer) Close() error {
	p.closeOnce.Do(func() {
		p.closeErr = errors.Join(p.src.Close(), p.pw.Close(), p.pr.Close())
	})
	return p.closeErr
}

func (p *Pacer) run(frameSize int, fps uint32) {
	frameCh := make(chan []byte, 1)
	srcErrCh := make(chan error, 1)

	// Only three frames are ever live: one being filled, one queued, one held as
	// latest. Recycle them - a 4K session otherwise churns ~33MB per frame, and
	// the pacer now runs on every platform.
	freeCh := make(chan []byte, 3)
	recycle := func(frame []byte) {
		if frame == nil {
			return
		}
		select {
		case freeCh <- frame:
		default:
		}
	}

	go func() {
		for {
			var frame []byte
			select {
			case frame = <-freeCh:
			default:
				frame = make([]byte, frameSize)
			}

			if _, err := io.ReadFull(p.src, frame); err != nil {
				srcErrCh <- err
				close(frameCh)
				return
			}

			select {
			case frameCh <- frame:
			default:
				select {
				case stale := <-frameCh:
					recycle(stale)
				default:
				}
				frameCh <- frame
			}
		}
	}()

	frameInterval := time.Second / time.Duration(fps)
	if frameInterval <= 0 {
		frameInterval = time.Second / time.Duration(60)
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

	// Writes block until ffmpeg drains the frame, and a Ticker drops ticks while
	// the receiver is busy. Counting ticks would therefore lose a frame per
	// stalled interval, and ffmpeg derives video pts from the frame count - so
	// every lost frame is permanent video lag against the wall-clock-paced
	// audio. Drive off the clock and make the missed frames up instead.
	start := time.Now()
	// The audio relay anchors its clock here too, so both inputs measure from
	// the same instant rather than from whenever ffmpeg got round to each one.
	p.skew.MarkStart(start)
	emitted := int64(1)
	maxDebt := int64(fps) * int64(MaxFrameDebtSeconds)

	for {
		select {
		case srcErr = <-srcErrCh:
			_ = p.pw.CloseWithError(srcErr)
			return
		case frame, ok := <-frameCh:
			if ok {
				// Safe to reclaim: Write is synchronous, so nothing references
				// the outgoing frame once we are back at the select.
				recycle(latest)
				latest = frame
			}
		case <-ticker.C:
			want := int64(time.Since(start)/frameInterval) + 1
			if debt := want - emitted; debt > maxDebt {
				// Sustained overload: ffmpeg cannot encode fps frames per second
				// at all, so catching up would only grow latency without end.
				// Re-anchor, hand the abandoned span to the audio relay so both
				// timelines stay equally short, and surface it - the real fix is
				// a lower fps/size.
				skipped := debt - 1
				abandoned := time.Duration(skipped) * frameInterval
				p.skew.Drop(skipped, abandoned)
				if debugutil.Enabled() {
					debugutil.Printf(
						"screencast/avsync frame_pacer overloaded dropped_frames=%d behind=%s fps=%d total_dropped=%d",
						skipped,
						abandoned.Round(time.Millisecond),
						fps,
						p.skew.DroppedFrames(),
					)
				}
				start = start.Add(abandoned)
				want = emitted + 1
			}

			for burst := 0; emitted < want && burst < MaxFrameBurst; burst++ {
				// Re-poll before every write, not once per tick: a write blocks
				// until ffmpeg drains the frame, and frameCh only ever holds the
				// newest one. Looking once emits the same stale frame up to
				// MaxFrameBurst times while the frames captured during the burst
				// are overwritten and lost - the picture freezes for the whole
				// catch-up and then jumps.
				select {
				case frame, ok := <-frameCh:
					if ok {
						recycle(latest)
						latest = frame
					}
				default:
				}

				if _, err := p.pw.Write(latest); err != nil {
					_ = p.pw.CloseWithError(err)
					return
				}
				emitted++
			}
		}
	}
}
