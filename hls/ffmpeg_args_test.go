package hls

import (
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The argument vector is the whole contract with ffmpeg: input format, pacing,
// mapping, encode and mux all live in it, and a single argument moved or
// dropped changes the stream without changing any type signature. Pin it
// exactly rather than asserting on properties of it.
func TestFFmpegArgs(t *testing.T) {
	const (
		baseFilter = "fps=60,scale='min(1280,iw)':'min(720,ih)':force_original_aspect_ratio=decrease,scale=trunc(iw/2)*2:trunc(ih/2)*2"
		tempDir    = "/tmp/screencast-hls-golden"
	)

	var (
		playlistPath = filepath.Join(tempDir, "playlist.m3u8")
		segmentPath  = filepath.Join(tempDir, "segment_%03d.ts")
		muxer        = hlsMuxerArgs(&Options{
			HLSTimeSeconds:     1,
			HLSListSize:        24,
			HLSDeleteThreshold: 36,
		}, tempDir, playlistPath)
		// The plan Start uses whenever no hardware encoder probes clean, which is
		// every machine without a usable GPU encoder.
		software = softwareEncoderPlan(baseFilter, "60", 1)
	)

	hlsMuxer := []string{
		"-f", "hls",
		"-hls_time", "1",
		"-hls_list_size", "24",
		"-hls_allow_cache", "0",
		"-hls_flags", "independent_segments+omit_endlist+delete_segments",
		"-hls_delete_threshold", "36",
		"-hls_segment_filename", segmentPath,
		playlistPath,
	}
	if !slices.Equal(muxer, hlsMuxer) {
		t.Fatalf("hlsMuxerArgs() = %q, want %q", muxer, hlsMuxer)
	}

	base := ffmpegArgsParams{
		encoderPlan:    software,
		videoQueueSize: 2048,
		audioQueueSize: 8192,
		pixelFormat:    "BGRA",
		width:          1920,
		height:         1080,
		fpsArg:         "60",
		muxerArgs:      muxer,
	}

	withAudio := func(p ffmpegArgsParams) ffmpegArgsParams {
		p.audioEnabled = true
		p.audioURL = "tcp://127.0.0.1:45123"
		return p
	}

	tests := []struct {
		name   string
		params ffmpegArgsParams
		want   []string
	}{
		{
			name:   "audio on",
			params: withAudio(base),
			want: []string{
				"-fflags", "nobuffer",
				"-flags", "low_delay",
				"-probesize", "32",
				"-analyzeduration", "0",
				"-thread_queue_size", "2048",
				"-f", "rawvideo",
				"-pix_fmt", "bgra",
				"-s", "1920x1080",
				"-r", "60",
				"-i", "pipe:0",
				"-thread_queue_size", "8192",
				"-fflags", "nobuffer",
				"-probesize", "32",
				"-analyzeduration", "0",
				"-f", "s16le",
				"-ar", "48000",
				"-ac", "2",
				"-i", "tcp://127.0.0.1:45123",
				"-map", "0:v:0",
				"-map", "1:a:0",
				"-r", "60",
				"-vf", baseFilter,
				"-c:v", "libx264",
				"-preset", "ultrafast",
				"-tune", "zerolatency",
				"-b:v", "4000k",
				"-maxrate", "5000k",
				"-bufsize", "10000k",
				"-pix_fmt", "yuv420p",
				"-g", "60",
				"-keyint_min", "60",
				"-sc_threshold", "0",
				"-force_key_frames", "expr:gte(t,n_forced*1)",
				"-af", "aresample=async=1:min_hard_comp=0.100:first_pts=0",
				"-c:a", "aac",
				"-ar", "48000",
				"-ac", "2",
				"-f", "hls",
				"-hls_time", "1",
				"-hls_list_size", "24",
				"-hls_allow_cache", "0",
				"-hls_flags", "independent_segments+omit_endlist+delete_segments",
				"-hls_delete_threshold", "36",
				"-hls_segment_filename", segmentPath,
				playlistPath,
			},
		},
		{
			// No second input at all: the audio socket is never mentioned, and the
			// stream is explicitly muxed without an audio track.
			name:   "audio off",
			params: base,
			want: []string{
				"-fflags", "nobuffer",
				"-flags", "low_delay",
				"-probesize", "32",
				"-analyzeduration", "0",
				"-thread_queue_size", "2048",
				"-f", "rawvideo",
				"-pix_fmt", "bgra",
				"-s", "1920x1080",
				"-r", "60",
				"-i", "pipe:0",
				"-map", "0:v:0",
				"-an",
				"-r", "60",
				"-vf", baseFilter,
				"-c:v", "libx264",
				"-preset", "ultrafast",
				"-tune", "zerolatency",
				"-b:v", "4000k",
				"-maxrate", "5000k",
				"-bufsize", "10000k",
				"-pix_fmt", "yuv420p",
				"-g", "60",
				"-keyint_min", "60",
				"-sc_threshold", "0",
				"-force_key_frames", "expr:gte(t,n_forced*1)",
				"-f", "hls",
				"-hls_time", "1",
				"-hls_list_size", "24",
				"-hls_allow_cache", "0",
				"-hls_flags", "independent_segments+omit_endlist+delete_segments",
				"-hls_delete_threshold", "36",
				"-hls_segment_filename", segmentPath,
				playlistPath,
			},
		},
		{
			// -loglevel comes first, and the encoder's own global arguments follow
			// it: -vaapi_device has to be in front of the input it applies to.
			name: "debug prefix and encoder global args",
			params: func() ffmpegArgsParams {
				p := base
				p.debug = true
				p.encoderPlan = videoEncoderPlan{
					label:       "h264_vaapi (/dev/dri/renderD128)",
					codec:       "h264_vaapi",
					hardware:    true,
					globalArgs:  []string{"-vaapi_device", "/dev/dri/renderD128"},
					videoFilter: baseFilter + ",format=nv12,hwupload",
					codecArgs:   []string{"-c:v", "h264_vaapi", "-g", "60"},
				}
				return p
			}(),
			want: []string{
				"-loglevel", "debug",
				"-vaapi_device", "/dev/dri/renderD128",
				"-fflags", "nobuffer",
				"-flags", "low_delay",
				"-probesize", "32",
				"-analyzeduration", "0",
				"-thread_queue_size", "2048",
				"-f", "rawvideo",
				"-pix_fmt", "bgra",
				"-s", "1920x1080",
				"-r", "60",
				"-i", "pipe:0",
				"-map", "0:v:0",
				"-an",
				"-r", "60",
				"-vf", baseFilter + ",format=nv12,hwupload",
				"-c:v", "h264_vaapi",
				"-g", "60",
				"-f", "hls",
				"-hls_time", "1",
				"-hls_list_size", "24",
				"-hls_allow_cache", "0",
				"-hls_flags", "independent_segments+omit_endlist+delete_segments",
				"-hls_delete_threshold", "36",
				"-hls_segment_filename", segmentPath,
				playlistPath,
			},
		},
		{
			// An empty filter must omit -vf entirely: ffmpeg rejects an empty
			// filtergraph rather than treating it as a passthrough.
			name: "empty filter",
			params: func() ffmpegArgsParams {
				p := base
				p.encoderPlan = videoEncoderPlan{
					label:     "libx264",
					codec:     "libx264",
					codecArgs: []string{"-c:v", "libx264"},
				}
				return p
			}(),
			want: []string{
				"-fflags", "nobuffer",
				"-flags", "low_delay",
				"-probesize", "32",
				"-analyzeduration", "0",
				"-thread_queue_size", "2048",
				"-f", "rawvideo",
				"-pix_fmt", "bgra",
				"-s", "1920x1080",
				"-r", "60",
				"-i", "pipe:0",
				"-map", "0:v:0",
				"-an",
				"-r", "60",
				"-c:v", "libx264",
				"-f", "hls",
				"-hls_time", "1",
				"-hls_list_size", "24",
				"-hls_allow_cache", "0",
				"-hls_flags", "independent_segments+omit_endlist+delete_segments",
				"-hls_delete_threshold", "36",
				"-hls_segment_filename", segmentPath,
				playlistPath,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ffmpegArgs(tc.params)
			if slices.Equal(got, tc.want) {
				return
			}
			t.Fatalf("ffmpegArgs() mismatch\n got: %s\nwant: %s\nfirst difference: %s",
				strings.Join(got, " "), strings.Join(tc.want, " "), firstDiff(got, tc.want))
		})
	}
}

func firstDiff(got, want []string) string {
	for i := 0; i < len(got) && i < len(want); i++ {
		if got[i] != want[i] {
			return "index " + strconv.Itoa(i) + ": got " + strconv.Quote(got[i]) + ", want " + strconv.Quote(want[i])
		}
	}
	if len(got) == len(want) {
		return "none"
	}
	if len(got) > len(want) {
		return "extra trailing argument " + strconv.Quote(got[len(want)])
	}
	return "missing argument " + strconv.Quote(want[len(got)])
}
