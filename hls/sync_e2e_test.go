package hls

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"go2tv.app/screencast/capture"
)

// ffmpegTooling is the external toolchain the end-to-end tests measure with. It
// is resolved per run rather than assumed: a machine without ffmpeg, without
// ffprobe, or with a build lacking the analysis filters skips these tests
// instead of failing them, since none of that is under this package's control.
type ffmpegTooling struct {
	ffmpeg  string
	ffprobe string
}

// requireFFmpegTooling skips the test unless the full measurement chain is
// present: both binaries, an encoder that can produce the stream, and the four
// filters the probes below are built on.
func requireFFmpegTooling(t *testing.T) ffmpegTooling {
	t.Helper()

	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed: skipping end-to-end sync measurement")
	}
	ffprobePath, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed: skipping end-to-end sync measurement")
	}

	out, err := exec.Command(ffmpegPath, "-hide_banner", "-filters").Output()
	if err != nil {
		t.Skipf("ffmpeg -filters failed (%v): skipping end-to-end sync measurement", err)
	}
	for _, name := range []string{"signalstats", "astats", "movie", "amovie"} {
		if !strings.Contains(string(out), " "+name+" ") {
			t.Skipf("ffmpeg build lacks the %q filter: skipping end-to-end sync measurement", name)
		}
	}

	encoders, err := exec.Command(ffmpegPath, "-hide_banner", "-encoders").Output()
	if err != nil {
		t.Skipf("ffmpeg -encoders failed (%v): skipping end-to-end sync measurement", err)
	}
	// selectVideoEncoder falls back to libx264, so without it there is no plan
	// that can encode at all on this machine.
	if !strings.Contains(string(encoders), " libx264") {
		t.Skip("ffmpeg build lacks libx264: skipping end-to-end sync measurement")
	}
	if !strings.Contains(string(encoders), " aac") {
		t.Skip("ffmpeg build lacks the aac encoder: skipping end-to-end sync measurement")
	}

	return ffmpegTooling{ffmpeg: ffmpegPath, ffprobe: ffprobePath}
}

// The pipeline's whole purpose is that what was captured together plays back
// together. Feed it one event that is simultaneous by construction - a white
// flash and a tone burst generated from the same clock - then decode the HLS
// output ffmpeg actually wrote and measure how far apart they ended up. This is
// the only test here that measures the product rather than a component of it.
func TestSessionKeepsCapturedEventInSync(t *testing.T) {
	tools := requireFFmpegTooling(t)

	const (
		width    = 320
		height   = 240
		fps      = 30
		flashAt  = 2500 * time.Millisecond
		flashFor = 200 * time.Millisecond
		// Measured, not budgeted: across repeated runs the skew is deterministic
		// to one astats frame (~21ms, the audio detection quantum) and never
		// exceeded 17ms. This leaves roughly 4x headroom for slower hardware
		// while still failing on a structural desync - dropping the pre-roll
		// discard alone moves it past 100ms. The relay's dead band permits more
		// displacement than this in principle, but only transiently: the event
		// lands 2.5s in, once the clock is tracking.
		tolerance = 80 * time.Millisecond
	)

	src := newFlashBeepSource(width, height, fps, flashAt, flashFor)
	sess := startWithSource(t, tools, src)

	// Wait for the event to be through the source, then let ffmpeg finish the
	// segments carrying it (hls_time is 1s).
	select {
	case <-src.videoDone:
	case <-time.After(flashAt + flashFor + 10*time.Second):
		t.Fatal("synthetic capture never produced the flash")
	}
	time.Sleep(3 * time.Second)

	clip := collectSegments(t, sess.Dir())
	if err := sess.Close(); err != nil {
		t.Fatalf("Session.Close() error = %v", err)
	}

	flash, err := findVideoFlash(tools, clip)
	if err != nil {
		t.Fatalf("locating the flash in the encoded output: %v", err)
	}
	beep, err := findAudioBeep(tools, clip)
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
			absDuration(skew).Round(time.Millisecond), lead,
			flash.Round(time.Millisecond), beep.Round(time.Millisecond), tolerance)
	}
}

// startWithSource swaps the capture backend for src and starts a real session
// against it: real ffmpeg, real encoder selection, real HLS output.
func startWithSource(t *testing.T, tools ffmpegTooling, src *flashBeepSource) *Session {
	t.Helper()

	restore := openCapture
	openCapture = func(*capture.Options) (*capture.Stream, error) { return src.stream(), nil }
	t.Cleanup(func() { openCapture = restore })

	sess, err := Start(&Options{
		FFmpegPath:     tools.ffmpeg,
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

// findVideoFlash returns the presentation time of the first frame bright enough
// to be the flash. YAVG is the frame's average luma: black frames sit near 16
// (limited range), the white flash near 235.
func findVideoFlash(tools ffmpegTooling, clip string) (time.Duration, error) {
	const lumaThreshold = 128
	return firstAbove(
		tools,
		fmt.Sprintf("movie=%s,signalstats", escapeFilterPath(clip)),
		"frame=pts_time:frame_tags=lavfi.signalstats.YAVG",
		func(v float64) bool { return v > lumaThreshold },
		"no frame reached the flash luma threshold",
	)
}

// findAudioBeep returns the presentation time of the first audio frame loud
// enough to be the tone. astats reports per-frame RMS in dBFS, so digital
// silence is -inf and the tone lands near -10.
func findAudioBeep(tools ffmpegTooling, clip string) (time.Duration, error) {
	const rmsThresholdDB = -40
	return firstAbove(
		tools,
		fmt.Sprintf("amovie=%s,astats=metadata=1:reset=1", escapeFilterPath(clip)),
		"frame=pts_time:frame_tags=lavfi.astats.Overall.RMS_level",
		func(v float64) bool { return v > rmsThresholdDB },
		"no audio frame reached the beep loudness threshold",
	)
}

// firstAbove runs one lavfi analysis graph and returns the timestamp of the
// first frame whose measured value satisfies want.
func firstAbove(tools ffmpegTooling, graph, entries string, want func(float64) bool, notFound string) (time.Duration, error) {
	cmd := exec.Command(tools.ffprobe,
		"-v", "error",
		"-f", "lavfi",
		"-i", graph,
		"-show_entries", entries,
		"-of", "csv=p=0",
	)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe %q: %w (%s)", graph, err, strings.TrimSpace(stderr.String()))
	}

	rows := csv.NewReader(strings.NewReader(string(out)))
	rows.FieldsPerRecord = -1
	for {
		row, err := rows.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("parsing ffprobe output: %w", err)
		}
		if len(row) < 2 {
			continue
		}
		at, err := strconv.ParseFloat(strings.TrimSpace(row[0]), 64)
		if err != nil {
			continue
		}
		// Silence reports -inf and a dropped tag reports nothing; neither is a
		// measurement, so skip rather than treat as a value.
		value, err := strconv.ParseFloat(strings.TrimSpace(row[1]), 64)
		if err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
			continue
		}
		if want(value) {
			return time.Duration(at * float64(time.Second)), nil
		}
	}
	return 0, errors.New(notFound)
}

// escapeFilterPath quotes a path for use inside a lavfi filter graph, where ':'
// and '\' separate arguments.
func escapeFilterPath(path string) string {
	r := strings.NewReplacer(`\`, `\\`, `:`, `\:`, `'`, `\'`)
	return r.Replace(path)
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
