package hls

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"go2tv.app/screencast/capture"
	"go2tv.app/screencast/internal/pipeline"
)

const (
	bytesPerPixelBGRA = 4
	// Frames the pacer may emit in one tick while catching up after a stall.
	maxFrameBurst = 8
	// Backlog past which catching up is hopeless rather than merely late.
	maxFrameDebtSeconds = 2
)

// timelineSkew is the shared state that keeps the two ffmpeg input timelines
// describing the same stretch of real time.
//
// ffmpeg derives both pts from counts - frames for rawvideo, bytes for s16le -
// so a shortfall on one input is invisible to it. Two things have to cross over
// from video to audio for the counts to mean the same thing:
//
// anchor is the instant video pts 0 corresponds to. ffmpeg opens pipe:0 and
// drains a frame before it ever connects to the audio socket, so the relay
// starting its own clock on connect would place audio byte 0 later in real time
// than video frame 0 while ffmpeg stamps both pts 0 - audio would lead the
// picture by that gap for the whole session.
//
// dropped is timeline the pacer gave up on. Dropping video without dropping the
// same span of audio desyncs the stream by the dropped duration, permanently,
// and every later drop stacks on top. The relay shortens its own clock by
// whatever lands here, leaving only the trim dead band as residual instead of
// an unbounded gap.
type timelineSkew struct {
	dropped atomic.Int64 // nanoseconds of timeline abandoned
	frames  atomic.Int64
	anchor  atomic.Int64 // UnixNano of video pts 0; 0 until the pacer starts
}

// markStart records the instant the first video frame reached ffmpeg. Only the
// first call counts: the pacer re-anchors its own clock when it abandons a span,
// but pts 0 stays where it was and the relay compensates via abandoned instead.
func (s *timelineSkew) markStart(t time.Time) {
	if s == nil {
		return
	}
	s.anchor.CompareAndSwap(0, t.UnixNano())
}

// startedAt reports the real time video pts 0 corresponds to, or fallback when
// there is no pacer (tests) or it has not emitted its first frame yet.
func (s *timelineSkew) startedAt(fallback time.Time) time.Time {
	if s == nil {
		return fallback
	}
	if ns := s.anchor.Load(); ns != 0 {
		return time.Unix(0, ns)
	}
	return fallback
}

func (s *timelineSkew) drop(frames int64, d time.Duration) {
	if s == nil || frames <= 0 {
		return
	}
	s.frames.Add(frames)
	s.dropped.Add(int64(d))
}

// abandoned reports the total timeline the pacer skipped.
func (s *timelineSkew) abandoned() time.Duration {
	if s == nil {
		return 0
	}
	return time.Duration(s.dropped.Load())
}

func (s *timelineSkew) droppedFrames() int64 {
	if s == nil {
		return 0
	}
	return s.frames.Load()
}

type framePacer struct {
	src       io.ReadCloser
	pr        *io.PipeReader
	pw        *io.PipeWriter
	skew      *timelineSkew
	closeOnce sync.Once
	closeErr  error
}

// newFramePacer takes ownership of stream: on success the returned pacer's
// Close is what closes it, and the caller must not close it as well. On failure
// ownership stays with the caller.
//
// A zero frame rate is an error rather than a request to pass the stream
// straight back. Handing the same object back as the paced input would make the
// caller's two handles one, and which of them still owns the stream would then
// depend on arguments it cannot inspect afterwards.
func newFramePacer(stream *capture.Stream, fps uint32, skew *timelineSkew) (*framePacer, error) {
	if stream == nil || stream.ReadCloser == nil {
		return nil, errors.New("nil stream")
	}
	if fps == 0 {
		return nil, errors.New("frame pacer needs a non-zero frame rate")
	}

	frameSize, err := rawFrameSize(stream.Width, stream.Height, stream.PixelFormat)
	if err != nil {
		return nil, err
	}

	pr, pw := io.Pipe()
	p := &framePacer{
		src:  stream,
		pr:   pr,
		pw:   pw,
		skew: skew,
	}

	go p.run(frameSize, fps)
	if pipeline.DebugEnabled() {
		pipeline.DebugPrintf(
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

	// Writes block until ffmpeg drains the frame, and a Ticker drops ticks while
	// the receiver is busy. Counting ticks would therefore lose a frame per
	// stalled interval, and ffmpeg derives video pts from the frame count - so
	// every lost frame is permanent video lag against the wall-clock-paced
	// audio. Drive off the clock and make the missed frames up instead.
	start := time.Now()
	// The audio relay anchors its clock here too, so both inputs measure from
	// the same instant rather than from whenever ffmpeg got round to each one.
	p.skew.markStart(start)
	emitted := int64(1)
	maxDebt := int64(fps) * int64(maxFrameDebtSeconds)

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
				p.skew.drop(skipped, abandoned)
				if pipeline.DebugEnabled() {
					pipeline.DebugPrintf(
						"screencast/hls frame_pacer overloaded dropped_frames=%d behind=%s fps=%d total_dropped=%d",
						skipped,
						abandoned.Round(time.Millisecond),
						fps,
						p.skew.droppedFrames(),
					)
				}
				start = start.Add(abandoned)
				want = emitted + 1
			}

			for burst := 0; emitted < want && burst < maxFrameBurst; burst++ {
				// Re-poll before every write, not once per tick: a write blocks
				// until ffmpeg drains the frame, and frameCh only ever holds the
				// newest one. Looking once emits the same stale frame up to
				// maxFrameBurst times while the frames captured during the burst
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
