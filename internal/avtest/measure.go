package avtest

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Tooling is the external toolchain the end-to-end tests measure with. It is
// resolved per run rather than assumed: a machine without ffmpeg, without
// ffprobe, or with a build lacking the analysis filters skips these tests
// instead of failing them, since none of that is under this module's control.
type Tooling struct {
	FFmpeg  string
	FFprobe string
}

// RequireTooling resolves the full measurement chain: both binaries, an encoder
// that can produce the stream, and the four filters the probes below are built
// on. Anything missing fails the test. A skip here would mean the only test
// that measures the product rather than a component of it silently stopped
// running.
func RequireTooling(t *testing.T) Tooling {
	t.Helper()

	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Fatal("ffmpeg not installed: the end-to-end sync measurement needs ffmpeg and ffprobe on PATH")
	}
	ffprobePath, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Fatal("ffprobe not installed: the end-to-end sync measurement needs ffmpeg and ffprobe on PATH")
	}

	out, err := exec.Command(ffmpegPath, "-hide_banner", "-filters").Output()
	if err != nil {
		t.Fatalf("ffmpeg -filters failed: %v", err)
	}
	for _, name := range []string{"signalstats", "astats", "movie", "amovie"} {
		if !strings.Contains(string(out), " "+name+" ") {
			t.Fatalf("ffmpeg build lacks the %q filter, which the measurement is built on", name)
		}
	}

	encoders, err := exec.Command(ffmpegPath, "-hide_banner", "-encoders").Output()
	if err != nil {
		t.Fatalf("ffmpeg -encoders failed: %v", err)
	}
	// SelectVideoEncoder falls back to libx264, so without it there is no plan
	// that can encode at all on this machine.
	if !strings.Contains(string(encoders), " libx264") {
		t.Fatal("ffmpeg build lacks libx264, so no encoder plan can produce the stream")
	}
	if !strings.Contains(string(encoders), " aac") {
		t.Fatal("ffmpeg build lacks the aac encoder, so the beep cannot be measured")
	}

	return Tooling{FFmpeg: ffmpegPath, FFprobe: ffprobePath}
}

// FindVideoFlash returns the presentation time of the first frame bright enough
// to be the flash. YAVG is the frame's average luma: black frames sit near 16
// (limited range), the white flash near 235.
func (tools Tooling) FindVideoFlash(clip string) (time.Duration, error) {
	const lumaThreshold = 128
	return tools.firstAbove(
		fmt.Sprintf("movie=%s,signalstats", escapeFilterPath(clip)),
		"frame=pts_time:frame_tags=lavfi.signalstats.YAVG",
		func(v float64) bool { return v > lumaThreshold },
		"no frame reached the flash luma threshold",
	)
}

// FindAudioBeep returns the presentation time of the first audio frame loud
// enough to be the tone. astats reports per-frame RMS in dBFS, so digital
// silence is -inf and the tone lands near -10.
func (tools Tooling) FindAudioBeep(clip string) (time.Duration, error) {
	const rmsThresholdDB = -40
	return tools.firstAbove(
		fmt.Sprintf("amovie=%s,astats=metadata=1:reset=1", escapeFilterPath(clip)),
		"frame=pts_time:frame_tags=lavfi.astats.Overall.RMS_level",
		func(v float64) bool { return v > rmsThresholdDB },
		"no audio frame reached the beep loudness threshold",
	)
}

// firstAbove runs one lavfi analysis graph and returns the timestamp of the
// first frame whose measured value satisfies want.
func (tools Tooling) firstAbove(graph, entries string, want func(float64) bool, notFound string) (time.Duration, error) {
	cmd := exec.Command(tools.FFprobe,
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

func AbsDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
