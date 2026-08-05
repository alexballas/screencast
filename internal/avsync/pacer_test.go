package avsync

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
	"time"

	"go2tv.app/screencast/capture"
)

// newTickingPacer wires a pacer to a BGRA source that emits one frame per tick
// at fps, the shape every capture backend presents. Returns the pacer and its
// frame size; both ends are closed on test cleanup, pacer first.
func newTickingPacer(t *testing.T, fps uint32, skew *Skew) (io.ReadCloser, int) {
	t.Helper()

	const (
		width  = 4
		height = 1
	)
	frameSize := width * height * BytesPerPixelBGRA

	srcR, srcW := io.Pipe()
	t.Cleanup(func() { _ = srcW.Close() })

	go func() {
		frame := make([]byte, frameSize)
		tk := time.NewTicker(time.Second / time.Duration(fps))
		defer tk.Stop()
		for range tk.C {
			if _, err := srcW.Write(frame); err != nil {
				return
			}
		}
	}()

	paced, err := NewPacer(&capture.Stream{
		ReadCloser:  srcR,
		Width:       width,
		Height:      height,
		FrameRate:   fps,
		PixelFormat: capture.PixelFormatBGRA,
	}, fps, skew)
	if err != nil {
		t.Fatalf("NewPacer() error = %v", err)
	}
	t.Cleanup(func() { _ = paced.Close() })

	return paced, frameSize
}

func TestFramePacerRepeatsLatestFrame(t *testing.T) {
	const (
		width  = 2
		height = 1
		fps    = 50
	)

	frameSize := width * height * BytesPerPixelBGRA
	srcR, srcW := io.Pipe()
	stream := &capture.Stream{
		ReadCloser:  srcR,
		Width:       width,
		Height:      height,
		FrameRate:   fps,
		PixelFormat: capture.PixelFormatBGRA,
	}

	paced, err := NewPacer(stream, fps, nil)
	if err != nil {
		t.Fatalf("NewPacer() error = %v", err)
	}
	defer paced.Close()

	first := bytes.Repeat([]byte{0x11}, frameSize)
	second := bytes.Repeat([]byte{0x22}, frameSize)
	done := make(chan struct{})

	go func() {
		_, _ = srcW.Write(first)
		time.Sleep(35 * time.Millisecond)
		_, _ = srcW.Write(second)
		<-done
		_ = srcW.Close()
	}()

	gotFirst := readExactWithTimeout(t, paced, frameSize, 300*time.Millisecond)
	gotRepeat := readExactWithTimeout(t, paced, frameSize, 300*time.Millisecond)
	gotSecond := readExactWithTimeout(t, paced, frameSize, 300*time.Millisecond)

	if !bytes.Equal(gotFirst, first) {
		t.Fatalf("first frame = %v, want %v", gotFirst, first)
	}
	if !bytes.Equal(gotRepeat, first) {
		t.Fatalf("repeat frame = %v, want %v", gotRepeat, first)
	}
	if !bytes.Equal(gotSecond, second) {
		t.Fatalf("second frame = %v, want %v", gotSecond, second)
	}
	close(done)
}

// ffmpeg derives video pts from the frame count, so frames lost while a write
// is blocked are permanent video lag against the wall-clock-paced audio. The
// pacer has to make them up rather than silently emit a short timeline.
func TestFramePacerRecoversFromConsumerStall(t *testing.T) {
	const (
		fps   = 50
		run   = time.Second
		stall = 200 * time.Millisecond
	)

	paced, frameSize := newTickingPacer(t, fps, nil)

	buf := make([]byte, frameSize)
	start := time.Now()
	emitted := 0
	stalled := false

	for time.Since(start) < run {
		if !stalled && time.Since(start) > run/3 {
			time.Sleep(stall)
			stalled = true
		}
		if _, err := io.ReadFull(paced, buf); err != nil {
			t.Fatalf("io.ReadFull() error = %v", err)
		}
		emitted++
	}

	// The pacer pays its debt back in bursts, so sample until it has recovered
	// rather than at one instant: a scheduler hiccup anywhere in the read loop
	// above lands in lag as debt the pacer has not been given a tick to pay off
	// yet. Unpaced code never recovers, so the deadline still catches it.
	lag := time.Since(start) - time.Duration(emitted)*time.Second/fps
	deadline := time.Now().Add(stall)
	for lag > stall/2 && time.Now().Before(deadline) {
		if _, err := io.ReadFull(paced, buf); err != nil {
			t.Fatalf("io.ReadFull() error = %v", err)
		}
		emitted++
		lag = time.Since(start) - time.Duration(emitted)*time.Second/fps
	}

	// Before catch-up this lost the whole stall (~160ms of a 1s run).
	if lag > stall/2 {
		t.Fatalf("video timeline %v after %v of wall clock: lag %v, want <= %v (emitted %d frames)",
			(time.Duration(emitted) * time.Second / fps).Round(time.Millisecond),
			time.Since(start).Round(time.Millisecond),
			lag.Round(time.Millisecond), (stall / 2).Round(time.Millisecond), emitted)
	}
}

// Catching up must not mean re-sending frames the source has already
// superseded. A write blocks until ffmpeg drains it and frameCh only ever holds
// the newest frame, so a burst that polls once emits one stale frame up to
// MaxFrameBurst times while every frame captured meanwhile is overwritten: the
// picture freezes for the length of the catch-up and then jumps.
func TestFramePacerBurstUsesFreshFrames(t *testing.T) {
	const (
		fps       = 50
		frameSize = BytesPerPixelBGRA // 1x1 BGRA: the whole frame is its id
		interval  = time.Second / fps
		stall     = 200 * time.Millisecond
		reads     = MaxFrameBurst
	)

	srcR, srcW := io.Pipe()
	t.Cleanup(func() { _ = srcW.Close() })

	// One distinctly numbered frame per interval, the shape of a capture backend
	// under continuous change.
	go func() {
		frame := make([]byte, frameSize)
		tk := time.NewTicker(interval)
		defer tk.Stop()
		for id := uint32(1); ; id++ {
			<-tk.C
			binary.LittleEndian.PutUint32(frame, id)
			if _, err := srcW.Write(frame); err != nil {
				return
			}
		}
	}()

	paced, err := NewPacer(&capture.Stream{
		ReadCloser:  srcR,
		Width:       1,
		Height:      1,
		FrameRate:   fps,
		PixelFormat: capture.PixelFormatBGRA,
	}, fps, nil)
	if err != nil {
		t.Fatalf("NewPacer() error = %v", err)
	}
	t.Cleanup(func() { _ = paced.Close() })

	buf := make([]byte, frameSize)
	if _, err := io.ReadFull(paced, buf); err != nil {
		t.Fatalf("io.ReadFull() error = %v", err)
	}

	// Build up debt, then read the catch-up back one frame per interval so the
	// pacer's writes stay blocked long enough for newer frames to land mid-burst.
	time.Sleep(stall)

	seen := make(map[uint32]bool, reads)
	ids := make([]uint32, 0, reads)
	for range reads {
		time.Sleep(interval)
		if _, err := io.ReadFull(paced, buf); err != nil {
			t.Fatalf("io.ReadFull() error = %v", err)
		}
		id := binary.LittleEndian.Uint32(buf)
		seen[id] = true
		ids = append(ids, id)
	}

	// The source produced a new frame per interval throughout, so a pacer that
	// re-polls delivers nearly all fresh ids. Polling once per tick pins the
	// whole burst to one id.
	if len(seen) <= reads/2 {
		t.Fatalf("catch-up delivered %d distinct frames out of %d reads (%v): the burst is re-sending superseded frames",
			len(seen), reads, ids)
	}
}

// A stall past MaxFrameDebtSeconds is given up on rather than paid back, and
// ffmpeg cannot see the gap - it derives video pts from the frame count. The
// abandoned span has to be recorded so the audio relay can shorten its own
// timeline by the same amount, otherwise the stream desyncs by that much for
// the rest of the session.
func TestFramePacerRecordsAbandonedTimeline(t *testing.T) {
	const (
		fps   = 50
		stall = MaxFrameDebtSeconds*time.Second + 500*time.Millisecond
	)

	skew := NewSkew()
	paced, frameSize := newTickingPacer(t, fps, skew)

	// The first read anchors the pacer clock; the stall then blocks its write
	// past the point where catching up is abandoned.
	buf := make([]byte, frameSize)
	if _, err := io.ReadFull(paced, buf); err != nil {
		t.Fatalf("io.ReadFull() error = %v", err)
	}
	time.Sleep(stall)

	deadline := time.Now().Add(time.Second)
	for skew.DroppedFrames() == 0 && time.Now().Before(deadline) {
		if _, err := io.ReadFull(paced, buf); err != nil {
			t.Fatalf("io.ReadFull() error = %v", err)
		}
	}

	dropped := skew.DroppedFrames()
	if dropped == 0 {
		t.Fatal("pacer abandoned the stall without recording it: audio cannot compensate")
	}
	if abandoned := skew.Abandoned(); abandoned < stall/2 || abandoned > stall+time.Second {
		t.Fatalf("abandoned %v over %d frames, want ~%v", abandoned.Round(time.Millisecond), dropped, stall)
	}
}

func TestRawFrameSizeBGRA(t *testing.T) {
	got, err := RawFrameSize(1280, 720, capture.PixelFormatBGRA)
	if err != nil {
		t.Fatalf("RawFrameSize() error = %v", err)
	}
	if want := 1280 * 720 * BytesPerPixelBGRA; got != want {
		t.Fatalf("RawFrameSize() = %d, want %d", got, want)
	}
}

func readExactWithTimeout(t *testing.T, r io.Reader, n int, timeout time.Duration) []byte {
	t.Helper()

	type result struct {
		buf []byte
		err error
	}

	done := make(chan result, 1)
	go func() {
		buf := make([]byte, n)
		_, err := io.ReadFull(r, buf)
		done <- result{buf: buf, err: err}
	}()

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("io.ReadFull() error = %v", res.err)
		}
		return res.buf
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for %d bytes", n)
		return nil
	}
}
