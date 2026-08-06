package hls

import (
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"go2tv.app/screencast/capture"
	"go2tv.app/screencast/internal/avtest"
)

// The pipeline's whole purpose is that what was captured together plays back
// together. Feed it one event that is simultaneous by construction - a white
// flash and a tone burst generated from the same clock - then decode the HLS
// output ffmpeg actually wrote and measure how far apart they ended up. This is
// the only test here that measures the product rather than a component of it.
func TestSessionKeepsCapturedEventInSync(t *testing.T) {
	tools := avtest.RequireTooling(t)

	const (
		width  = 320
		height = 240
		fps    = 30
		// Placed to sit clear of both detection grids - see avtest.FlashAt.
		flashAt  = avtest.FlashAt
		flashFor = avtest.FlashFor
		// Measured, not budgeted: across repeated runs the skew is deterministic
		// to one astats frame (~21ms, the audio detection quantum) and never
		// exceeded 17ms. This leaves roughly 4x headroom for slower hardware
		// while still failing on a structural desync - dropping the pre-roll
		// discard alone moves it past 100ms. The relay's dead band permits more
		// displacement than this in principle, but only transiently: the event
		// lands 2.5s in, once the clock is tracking.
		tolerance = 80 * time.Millisecond
	)

	src := avtest.NewFlashBeepSource(width, height, fps, flashAt, flashFor)
	sess := startWithSource(t, tools, src)

	// Wait for the event to be through the source, then let ffmpeg finish the
	// segments carrying it (hls_time is 1s).
	select {
	case <-src.VideoDone():
	case <-time.After(flashAt + flashFor + 10*time.Second):
		t.Fatal("synthetic capture never produced the flash")
	}
	time.Sleep(3 * time.Second)

	clip := collectSegments(t, sess.Dir())
	if err := sess.Close(); err != nil {
		t.Fatalf("Session.Close() error = %v", err)
	}

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
	t.Logf("flash at %v, beep at %v, skew %v (tolerance %v)",
		flash.Round(time.Millisecond), beep.Round(time.Millisecond),
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

// startWithSource swaps the capture backend for src and starts a real session
// against it: real ffmpeg, real encoder selection, real HLS output.
func startWithSource(t *testing.T, tools avtest.Tooling, src *avtest.FlashBeepSource) *Session {
	t.Helper()

	restore := openCapture
	openCapture = func(*capture.Options) (*capture.Stream, error) { return src.Stream(), nil }
	t.Cleanup(func() { openCapture = restore })

	sess, err := Start(&Options{
		FFmpegPath:     tools.FFmpeg,
		IncludeAudio:   true,
		HLSTimeSeconds: 1,
		StartupTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	return sess
}

// collectSegments concatenates the session's HLS segments into a single file the
// probes can read. MPEG-TS segments carry their own headers and a continuous
// timeline, so appending them preserves both tracks' timestamps.
//
// This must run before Session.Close, which deletes the session directory.
func collectSegments(t *testing.T, dir string) string {
	t.Helper()

	names, err := filepath.Glob(filepath.Join(dir, "segment_*.ts"))
	if err != nil {
		t.Fatalf("globbing segments: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("session produced no HLS segments")
	}
	sort.Strings(names)

	path := filepath.Join(t.TempDir(), "clip.ts")
	out, err := os.Create(path)
	if err != nil {
		t.Fatalf("creating clip: %v", err)
	}
	defer out.Close()

	for _, name := range names {
		in, err := os.Open(name)
		if err != nil {
			t.Fatalf("opening segment %s: %v", filepath.Base(name), err)
		}
		if _, err := io.Copy(out, in); err != nil {
			in.Close()
			t.Fatalf("appending segment %s: %v", filepath.Base(name), err)
		}
		in.Close()
	}
	return path
}
