package avsync

import (
	"sync/atomic"
	"time"
)

// Skew is the shared state that keeps the two ffmpeg input timelines
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
type Skew struct {
	dropped atomic.Int64 // nanoseconds of timeline abandoned
	frames  atomic.Int64
	anchor  atomic.Int64 // UnixNano of video pts 0; 0 until the pacer starts
}

// NewSkew returns a zero-value skew.
func NewSkew() *Skew {
	return &Skew{}
}

// MarkStart records the instant the first video frame reached ffmpeg. Only the
// first call counts: the pacer re-anchors its own clock when it abandons a span,
// but pts 0 stays where it was and the relay compensates via abandoned instead.
func (s *Skew) MarkStart(t time.Time) {
	if s == nil {
		return
	}
	s.anchor.CompareAndSwap(0, t.UnixNano())
}

// StartedAt reports the real time video pts 0 corresponds to, or fallback when
// there is no pacer (tests) or it has not emitted its first frame yet.
func (s *Skew) StartedAt(fallback time.Time) time.Time {
	if s == nil {
		return fallback
	}
	if ns := s.anchor.Load(); ns != 0 {
		return time.Unix(0, ns)
	}
	return fallback
}

// Drop records frames and the timeline they occupied as abandoned.
func (s *Skew) Drop(frames int64, d time.Duration) {
	if s == nil || frames <= 0 {
		return
	}
	s.frames.Add(frames)
	s.dropped.Add(int64(d))
}

// Abandoned reports the total timeline the pacer skipped.
func (s *Skew) Abandoned() time.Duration {
	if s == nil {
		return 0
	}
	return time.Duration(s.dropped.Load())
}

// DroppedFrames reports how many frames the pacer gave up on.
func (s *Skew) DroppedFrames() int64 {
	if s == nil {
		return 0
	}
	return s.frames.Load()
}
