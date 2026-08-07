package pipeline

import (
	"fmt"
	"strconv"
	"strings"
)

// baseVideoFilter caps the encode at 720p and holds it to fps, before whatever
// pixel format the selected encoder needs is appended to it. The trunc pair
// keeps both dimensions even, which H.264 requires.
func baseVideoFilter(fpsArg string) string {
	return fmt.Sprintf(
		"fps=%s,scale='min(1280,iw)':'min(720,ih)':force_original_aspect_ratio=decrease,scale=trunc(iw/2)*2:trunc(ih/2)*2",
		fpsArg,
	)
}

// FFmpegArgsParams is everything the ffmpeg command line is derived from.
// Keeping the vector a pure function of a value lets a test pin the exact
// arguments without opening a capture backend or spawning anything.
type FFmpegArgsParams struct {
	Debug          bool
	EncoderPlan    VideoEncoderPlan
	VideoQueueSize int
	AudioQueueSize int
	PixelFormat    string
	Width          uint32
	Height         uint32
	FpsArg         string
	AudioEnabled   bool
	AudioURL       string
	// muxerArgs terminate the vector. Everything before them describes the raw
	// inputs and the encode, neither of which is HLS-specific.
	MuxerArgs []string
}

func FFmpegArgs(p FFmpegArgsParams) []string {
	args := []string{}
	if p.Debug {
		args = append(args, "-loglevel", "debug")
	}
	args = append(args, p.EncoderPlan.GlobalArgs...)
	args = append(
		args,
		"-fflags", "nobuffer",
		"-flags", "low_delay",
		"-probesize", "32",
		"-analyzeduration", "0",
		"-thread_queue_size", strconv.Itoa(p.VideoQueueSize),
		"-f", "rawvideo",
		"-pix_fmt", strings.ToLower(p.PixelFormat),
		"-s", fmt.Sprintf("%dx%d", p.Width, p.Height),
		"-r", p.FpsArg,
		"-i", "pipe:0",
	)
	if p.AudioEnabled {
		args = append(
			args,
			"-thread_queue_size", strconv.Itoa(p.AudioQueueSize),
			"-fflags", "nobuffer",
			"-probesize", "32",
			"-analyzeduration", "0",
			"-f", "s16le",
			"-ar", "48000",
			"-ac", "2",
			"-i", p.AudioURL,
			"-map", "0:v:0",
			"-map", "1:a:0",
		)
	} else {
		args = append(
			args,
			"-map", "0:v:0",
			"-an",
		)
	}

	args = append(
		args,
		"-r", p.FpsArg,
	)
	if strings.TrimSpace(p.EncoderPlan.VideoFilter) != "" {
		args = append(args, "-vf", p.EncoderPlan.VideoFilter)
	}
	args = append(args, p.EncoderPlan.CodecArgs...)
	if p.AudioEnabled {
		args = append(
			args,
			"-af", "aresample=async=1:min_hard_comp=0.100:first_pts=0",
			"-c:a", "aac",
			"-ar", "48000",
			"-ac", "2",
		)
	}
	args = append(args, p.MuxerArgs...)

	return args
}
