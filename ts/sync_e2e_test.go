package ts

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go2tv.app/screencast/capture"
	"go2tv.app/screencast/internal/avtest"
)

// The muxer's whole purpose is that what was captured together plays back
// together on a renderer. Feed the pipeline one event that is simultaneous by
// construction - a white flash and a tone burst generated from the same clock -
// then decode the m2ts bytes a TV would actually receive and measure how far
// apart they ended up.
//
// This is the only test here that measures the product rather than a component
// of it: real ffmpeg, real encoder selection, real m2ts output drained through
// Stream() exactly as a renderer drains it.
func TestSessionKeepsCapturedEventInSync(t *testing.T) {
	tools := avtest.RequireTooling(t)

	const (
		width    = 320
		height   = 240
		fps      = 30
		flashAt  = 2500 * time.Millisecond
		flashFor = 200 * time.Millisecond
		// Same budget as the hls measurement, and for the same reason: both
		// muxers sit behind the same pacer and audio relay, so a structural
		// desync shows up identically. Measured here across repeated runs the
		// skew stayed within one astats frame (~21ms, the audio detection
		// quantum) and never exceeded 16ms, so this leaves roughly 5x headroom
		// for slower hardware while still failing if the pre-roll discard or
		// the timeline tracking regresses.
		tolerance = 80 * time.Millisecond
	)

	src := avtest.NewFlashBeepSource(width, height, fps, flashAt, flashFor)
	sess := startWithSource(t, tools, src)

	// A renderer has to keep reading or ffmpeg blocks on the stdout pipe, so
	// drain continuously from the moment the session starts.
	clip := filepath.Join(t.TempDir(), "clip.m2ts")
	out, err := os.Create(clip)
	if err != nil {
		t.Fatalf("creating clip: %v", err)
	}
	copied := make(chan int64, 1)
	go func() {
		n, _ := io.Copy(out, sess.Stream())
		copied <- n
	}()

	// Wait for the event to be through the source, then let ffmpeg push it out
	// of the encoder and into the stream.
	select {
	case <-src.VideoDone():
	case <-time.After(flashAt + flashFor + 10*time.Second):
		t.Fatal("synthetic capture never produced the flash")
	}
	time.Sleep(3 * time.Second)

	// Closing the session kills ffmpeg, which ends the copy.
	if err := sess.Close(); err != nil {
		t.Fatalf("Session.Close() error = %v", err)
	}
	n := <-copied
	if err := out.Close(); err != nil {
		t.Fatalf("closing clip: %v", err)
	}
	if n == 0 {
		t.Fatal("session produced no stream bytes")
	}

	assertM2TSShape(t, clip, n)

	flash, err := tools.FindVideoFlash(clip)
	if err != nil {
		t.Fatalf("locating the flash in the encoded output: %v", err)
	}
	beep, err := tools.FindAudioBeep(clip)
	if err != nil {
		t.Fatalf("locating the beep in the encoded output: %v", err)
	}

	// Positive skew means the picture arrives after the sound.
	skew := flash - beep
	t.Logf("stream %d bytes, flash at %v, beep at %v, skew %v (tolerance %v)",
		n, flash.Round(time.Millisecond), beep.Round(time.Millisecond),
		skew.Round(time.Millisecond), tolerance)

	if skew > tolerance || skew < -tolerance {
		lead := "audio leads the picture"
		if skew < 0 {
			lead = "audio lags the picture"
		}
		t.Fatalf("captured simultaneously but encoded %v apart (%s): flash at %v, beep at %v, want within %v",
			avtest.AbsDuration(skew).Round(time.Millisecond), lead,
			flash.Round(time.Millisecond), beep.Round(time.Millisecond), tolerance)
	}
}

// assertM2TSShape checks the container invariant the startup probe depends on,
// against bytes a real ffmpeg produced rather than a hand-built packet: every
// 192-byte cell carries its TS sync byte 4 bytes in. The unit tests build their
// own packets, so only this can catch ffmpeg declining -mpegts_m2ts_mode.
//
// It also pins the prefixReader handover for free. The first bytes a caller
// reads come from the probe's startup buffer and the rest from the ffmpeg pipe;
// if that seam ever dropped or repeated a byte, the alignment checked here
// would break for every packet after it.
func assertM2TSShape(t *testing.T, clip string, n int64) {
	t.Helper()

	data, err := os.ReadFile(clip)
	if err != nil {
		t.Fatalf("reading clip: %v", err)
	}
	if int64(len(data)) != n {
		t.Fatalf("clip is %d bytes, copier reported %d", len(data), n)
	}
	if len(data)%m2tsPacketSize != 0 {
		t.Errorf("stream is %d bytes, not a whole number of %d-byte m2ts packets", len(data), m2tsPacketSize)
	}

	packets := 0
	for off := 0; off+m2tsPacketSize <= len(data); off += m2tsPacketSize {
		if data[off+m2tsHeaderSize] != tsSyncByte {
			t.Fatalf("packet %d at offset %d: got 0x%02x at the sync position, want 0x%02x",
				packets, off, data[off+m2tsHeaderSize], tsSyncByte)
		}
		packets++
	}
	if packets == 0 {
		t.Fatal("stream contained no complete m2ts packets")
	}
	t.Logf("verified %d m2ts packets from a real ffmpeg session", packets)
}

// startWithSource swaps the capture backend for src and starts a real session
// against it: real ffmpeg, real encoder selection, real m2ts output.
func startWithSource(t *testing.T, tools avtest.Tooling, src *avtest.FlashBeepSource) *Session {
	t.Helper()

	restore := openCapture
	openCapture = func(*capture.Options) (*capture.Stream, error) { return src.Stream(), nil }
	t.Cleanup(func() { openCapture = restore })

	sess, err := Start(&Options{
		FFmpegPath:     tools.FFmpeg,
		IncludeAudio:   true,
		StartupTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	return sess
}
