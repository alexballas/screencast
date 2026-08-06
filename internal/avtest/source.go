// Package avtest provides the pieces the end-to-end tests share: a synthetic
// capture backend that emits one simultaneous audiovisual event, and the
// ffprobe measurements that locate that event in an encoded stream.
//
// It lives outside the _test.go files so that every muxer package can measure
// the same event the same way, instead of each one carrying its own copy.
package avtest

import (
	"io"
	"math"
	"sync"
	"time"

	"go2tv.app/screencast/capture"
	"go2tv.app/screencast/internal/pipeline"
)

// FlashAt and FlashFor place the event, and the offset is load-bearing rather
// than arbitrary.
//
// The event is located twice in the encoded output: the first video frame
// bright enough, on a 33.33ms grid at 30fps, and the first AAC frame loud
// enough, on a 21.33ms one. An onset landing on either boundary lets
// sub-millisecond jitter decide which frame comes first, and the reading jumps
// a whole frame. 2500ms is frame 75.0 exactly, and over 50 runs it split the
// ts measurement between -17ms and +16ms - 33ms apart, one video frame, noise
// in the detector rather than anything the pipeline did.
//
// 2656ms is 10.67ms from the nearest edge of both grids, the most any value can
// be: half an AAC frame is 10.67ms, so nothing does better on the finer grid.
// Over 20 runs each, both muxers then centre on -21ms: ts reports it 19 times
// out of 20 (sd 2.0, down from 15.8 on the boundary), hls 18 times out of 20,
// its two outliers being one AAC frame away rather than one video frame.
//
// That -21ms is this harness, not a desync. lit lights a whole video frame that
// the flash merely overlaps, so the first lit frame can start 22.7ms before the
// onset, while audio moves in 10ms chunks and starts at most 6ms early. The
// picture is therefore expected to lead the sound by roughly 17ms, and both
// muxers agreeing on that figure is what says the pipeline is even-handed.
const (
	FlashAt  = 2656 * time.Millisecond
	FlashFor = 200 * time.Millisecond
)

// FlashBeepSource is a synthetic capture backend that emits one unmistakable
// audiovisual event: a white flash and a full-scale tone, starting at the same
// instant and lasting the same duration.
//
// Both tracks are generated from one start time and one offset counter, so the
// flash and the beep are simultaneous by construction rather than by timing -
// there is no scheduling jitter between them to explain away. Whatever
// separation survives to the encoded output was introduced by the pipeline
// under test, which is the whole measurement.
type FlashBeepSource struct {
	start     time.Time
	fps       uint32
	width     uint32
	height    uint32
	flashAt   time.Duration
	flashFor  time.Duration
	closeOnce sync.Once
	closed    chan struct{}

	// videoDone closes once the last flash frame has been handed over, so a test
	// can wait for the event to be in the pipeline instead of guessing.
	videoDone     chan struct{}
	videoDoneOnce sync.Once
}

func NewFlashBeepSource(width, height, fps uint32, flashAt, flashFor time.Duration) *FlashBeepSource {
	return &FlashBeepSource{
		start:     time.Now(),
		fps:       fps,
		width:     width,
		height:    height,
		flashAt:   flashAt,
		flashFor:  flashFor,
		closed:    make(chan struct{}),
		videoDone: make(chan struct{}),
	}
}

// Stream presents the source the way a capture backend does.
func (s *FlashBeepSource) Stream() *capture.Stream {
	return &capture.Stream{
		ReadCloser:  &flashBeepVideo{src: s},
		Audio:       &flashBeepAudio{src: s},
		Width:       s.width,
		Height:      s.height,
		FrameRate:   s.fps,
		PixelFormat: capture.PixelFormatBGRA,
	}
}

// VideoDone closes once the final flash frame has been handed to the pipeline.
func (s *FlashBeepSource) VideoDone() <-chan struct{} {
	return s.videoDone
}

// lit reports whether the event is active over the span [at, at+d).
func (s *FlashBeepSource) lit(at, d time.Duration) bool {
	return at+d > s.flashAt && at < s.flashAt+s.flashFor
}

// waitUntil sleeps until due, measured from the shared start, and reports
// whether the source is still open.
func (s *FlashBeepSource) waitUntil(due time.Duration) bool {
	delay := time.Until(s.start.Add(due))
	if delay <= 0 {
		select {
		case <-s.closed:
			return false
		default:
			return true
		}
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-s.closed:
		return false
	case <-t.C:
		return true
	}
}

func (s *FlashBeepSource) Close() {
	s.closeOnce.Do(func() { close(s.closed) })
}

// flashBeepVideo delivers one BGRA frame per frame interval, the shape a capture
// backend presents to the pacer.
type flashBeepVideo struct {
	src   *FlashBeepSource
	buf   []byte
	frame int64
}

func (v *flashBeepVideo) Read(p []byte) (int, error) {
	if len(v.buf) == 0 {
		interval := time.Second / time.Duration(v.src.fps)
		due := time.Duration(v.frame) * interval
		if !v.src.waitUntil(due) {
			return 0, io.EOF
		}

		size := int(v.src.width) * int(v.src.height) * pipeline.BytesPerPixelBGRA
		frame := make([]byte, size)
		if v.src.lit(due, interval) {
			for i := range frame {
				frame[i] = 0xFF // white, opaque
			}
		} else {
			// Black, opaque: alpha high so the frame is not ambiguous to scale.
			for i := 3; i < len(frame); i += pipeline.BytesPerPixelBGRA {
				frame[i] = 0xFF
			}
		}
		if due >= v.src.flashAt+v.src.flashFor {
			v.src.videoDoneOnce.Do(func() { close(v.src.videoDone) })
		}
		v.buf = frame
		v.frame++
	}

	n := copy(p, v.buf)
	v.buf = v.buf[n:]
	return n, nil
}

func (v *flashBeepVideo) Close() error {
	v.src.Close()
	return nil
}

// flashBeepAudio delivers 48kHz stereo s16le at real-time rate, tone during the
// flash and digital silence outside it.
type flashBeepAudio struct {
	src    *FlashBeepSource
	buf    []byte
	offset int64 // bytes produced so far
}

func (a *flashBeepAudio) Read(p []byte) (int, error) {
	if len(a.buf) == 0 {
		const chunk = 1920 // 10ms, whole PCM frames

		// Pace by byte count: the offset itself is the clock, so the tone lands
		// at the same instant as the flash without a second timer to drift.
		due := time.Duration(a.offset) * time.Second / pipeline.AudioBytesPerSecond
		if !a.src.waitUntil(due) {
			return 0, io.EOF
		}

		span := time.Duration(chunk) * time.Second / pipeline.AudioBytesPerSecond
		b := make([]byte, chunk)
		if a.src.lit(due, span) {
			// 1kHz sine at high amplitude: unmistakable against silence under
			// astats, and survives AAC intact.
			const freq = 1000.0
			sample := a.offset / pipeline.AudioFrameBytes
			for i := 0; i < chunk; i += pipeline.AudioFrameBytes {
				v := int16(20000 * math.Sin(2*math.Pi*freq*float64(sample)/48000))
				b[i] = byte(v)
				b[i+1] = byte(v >> 8)
				b[i+2] = byte(v)
				b[i+3] = byte(v >> 8)
				sample++
			}
		}
		a.buf = b
		a.offset += chunk
	}

	n := copy(p, a.buf)
	a.buf = a.buf[n:]
	return n, nil
}

func (a *flashBeepAudio) Close() error {
	a.src.Close()
	return nil
}
