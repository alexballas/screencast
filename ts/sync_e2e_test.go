package ts

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go2tv.app/screencast/capture"
	"go2tv.app/screencast/internal/avtest"
)

// The muxer's whole purpose is that what was captured together plays back
// together on a renderer. Feed the pipeline one event that is simultaneous by
// construction - a white flash and a tone burst generated from the same clock -
// then decode the TS bytes a TV would actually receive and measure how far
// apart they ended up.
//
// This is the only test here that measures the product rather than a component
// of it: real ffmpeg, real encoder selection, real TS output drained through
// Stream() exactly as a renderer drains it.
func TestSessionKeepsCapturedEventInSync(t *testing.T) {
	tools := avtest.RequireTooling(t)

	const (
		width  = 320
		height = 240
		fps    = 30
		// Placed to sit clear of both detection grids - see avtest.FlashAt.
		flashAt  = avtest.FlashAt
		flashFor = avtest.FlashFor
		// Same budget as the hls measurement, and for the same reason: both
		// muxers sit behind the same pacer and audio relay, so a structural
		// desync shows up identically. Measured across repeated runs the skew
		// stays well inside one astats frame (~21ms, the audio detection
		// quantum), so this leaves several times the headroom for slower
		// hardware while still failing if the pre-roll discard or the timeline
		// tracking regresses - stubbing out the discard alone moves it to
		// 124ms.
		tolerance = 80 * time.Millisecond
	)

	src := avtest.NewFlashBeepSource(width, height, fps, flashAt, flashFor)
	sess := startWithSource(t, tools, src)

	// A renderer has to keep reading or ffmpeg blocks on the stdout pipe, so
	// drain continuously from the moment the session starts.
	clip := filepath.Join(t.TempDir(), "clip.ts")
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

	assertTSShape(t, clip, n)
	assertAudioIsDeclaredAAC(t, tools, clip)

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

// assertTSShape checks the container invariant the startup probe depends on,
// against bytes a real ffmpeg produced rather than a hand-built packet: the
// stream is a whole number of 188-byte packets, each opening with the sync
// byte. The unit tests build their own packets, so only this can catch ffmpeg
// emitting a container other than the plain TS the muxer args ask for - 192-byte
// m2ts cells being the one that would slip through a sync-byte-only check.
//
// It also pins the prefixReader handover for free. The first bytes a caller
// reads come from the probe's startup buffer and the rest from the ffmpeg pipe;
// if that seam ever dropped or repeated a byte, the alignment checked here
// would break for every packet after it.
func assertTSShape(t *testing.T, clip string, n int64) {
	t.Helper()

	data, err := os.ReadFile(clip)
	if err != nil {
		t.Fatalf("reading clip: %v", err)
	}
	if int64(len(data)) != n {
		t.Fatalf("clip is %d bytes, copier reported %d", len(data), n)
	}
	if len(data)%tsPacketSize != 0 {
		t.Errorf("stream is %d bytes, not a whole number of %d-byte TS packets", len(data), tsPacketSize)
	}

	packets := 0
	for off := 0; off+tsPacketSize <= len(data); off += tsPacketSize {
		if data[off] != tsSyncByte {
			t.Fatalf("packet %d at offset %d: got 0x%02x at the sync position, want 0x%02x",
				packets, off, data[off], tsSyncByte)
		}
		packets++
	}
	if packets == 0 {
		t.Fatal("stream contained no complete TS packets")
	}
	t.Logf("verified %d TS packets from a real ffmpeg session", packets)
}

// assertAudioIsDeclaredAAC pins the PMT stream_type of the audio track to 0x0F,
// ISO/IEC 13818-7 ADTS AAC.
//
// The beep measured above proves only that the samples are in the stream and on
// time, which is not the same as a renderer being able to find them: ffmpeg's
// demuxer probes elementary streams and plays the audio whatever the PMT says.
// A renderer that trusts the PMT does not. Running the muxer in m2ts mode - the
// obvious thing to reach for, since the 192-byte packets it produces are what
// the non-_ISO DLNA profiles ask for - declares this same AAC as stream_type
// 0x06, PES private data, and GStreamer's tsdemux then creates no pad for it.
// The picture plays, the sound is silently dropped, and every check above still
// passes. So assert the declaration, not just the samples.
func assertAudioIsDeclaredAAC(t *testing.T, tools avtest.Tooling, clip string) {
	t.Helper()

	const streamTypeADTSAAC = 0x0F

	out, err := exec.Command(tools.FFprobe,
		"-v", "error",
		"-select_streams", "a",
		"-show_entries", "stream=codec_tag",
		"-of", "csv=p=0",
		clip,
	).Output()
	if err != nil {
		t.Fatalf("ffprobe on the encoded output: %v", err)
	}

	// ffmpeg's mpegts demuxer reports the PMT stream_type as the codec tag.
	field := strings.TrimSpace(string(out))
	if field == "" {
		t.Fatal("the encoded output declares no audio stream at all")
	}
	tag, err := strconv.ParseUint(strings.TrimPrefix(strings.Fields(field)[0], "0x"), 16, 32)
	if err != nil {
		t.Fatalf("parsing codec tag %q: %v", field, err)
	}
	if tag != streamTypeADTSAAC {
		t.Fatalf("PMT declares the audio as stream_type 0x%02x, want 0x%02x (ADTS AAC); "+
			"a renderer that trusts the PMT will drop the track", tag, streamTypeADTSAAC)
	}
}

// startWithSource swaps the capture backend for src and starts a real session
// against it: real ffmpeg, real encoder selection, real TS output.
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
