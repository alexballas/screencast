package hls

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go2tv.app/screencast/capture"
	"go2tv.app/screencast/internal/pipeline"
)

const (
	defaultDeleteThreshold = 36
	defaultStartupTimeout  = 60 * time.Second
	defaultTempDirPrefix   = "screencast-hls-"
	defaultVideoQueueSize  = 2048
	defaultAudioQueueSize  = 8192
	defaultHLSTimeSeconds  = 1
	defaultHLSListSize     = 24
)

type Options struct {
	FFmpegPath   string
	IncludeAudio bool
	// StreamIndex selects the display/stream index passed to capture.Open (default 0).
	StreamIndex        int
	HLSDeleteThreshold int
	HLSTimeSeconds     int
	HLSListSize        int
	VideoQueueSize     int
	AudioQueueSize     int
	AudioChunkSize     int
	AudioRelayQueue    int
	StartupTimeout     time.Duration
	TempDirPrefix      string
	LogOutput          io.Writer
	DebugCommand       bool
}

// Session is an HLS output on top of a pipeline session: the temp directory
// the segments and playlist are written into, and the running pipeline that
// writes them.
type Session struct {
	dir  string
	pipe *pipeline.Session
}

// openCapture is capture.Open behind a seam, so tests can drive Start with a
// synthetic frame source instead of the real screen.
var openCapture = capture.Open

func Start(options *Options) (*Session, error) {
	opts, err := normalizeOptions(options)
	if err != nil {
		return nil, err
	}

	cleanupOldTempDirs(opts.TempDirPrefix, 12*time.Hour)

	prep, err := pipeline.Prepare(&pipeline.Config{
		FFmpegPath:      opts.FFmpegPath,
		IncludeAudio:    opts.IncludeAudio,
		StreamIndex:     opts.StreamIndex,
		GOPSeconds:      opts.HLSTimeSeconds,
		VideoQueueSize:  opts.VideoQueueSize,
		AudioQueueSize:  opts.AudioQueueSize,
		AudioChunkSize:  opts.AudioChunkSize,
		AudioRelayQueue: opts.AudioRelayQueue,
		LogOutput:       opts.LogOutput,
		DebugCommand:    opts.DebugCommand,
		OpenCapture:     openCapture,
	})
	if err != nil {
		return nil, err
	}

	// After the pipeline, not before: the muxer arguments are the only thing
	// that needs the directory, and a failure here is what the abort path is
	// for.
	tempDir, err := os.MkdirTemp("", opts.TempDirPrefix)
	if err != nil {
		_ = prep.Abort()
		return nil, fmt.Errorf("screencast temp dir: %w", err)
	}

	playlistPath := filepath.Join(tempDir, "playlist.m3u8")
	pipe, err := prep.Launch(pipeline.LaunchOptions{
		MuxerArgs: hlsMuxerArgs(opts, tempDir, playlistPath),
		Cleanup:   func() error { return os.RemoveAll(tempDir) },
	})
	if err != nil {
		return nil, err
	}

	s := &Session{dir: tempDir, pipe: pipe}
	if err := waitForPlaylistReady(playlistPath, tempDir, opts.StartupTimeout, pipe); err != nil {
		_ = s.Close()
		return nil, err
	}

	return s, nil
}

func hlsMuxerArgs(opts *Options, tempDir, playlistPath string) []string {
	return []string{
		"-f", "hls",
		"-hls_time", strconv.Itoa(opts.HLSTimeSeconds),
		"-hls_list_size", strconv.Itoa(opts.HLSListSize),
		"-hls_allow_cache", "0",
		"-hls_flags", "independent_segments+omit_endlist+delete_segments",
		"-hls_delete_threshold", strconv.Itoa(opts.HLSDeleteThreshold),
		"-hls_segment_filename", filepath.Join(tempDir, "segment_%03d.ts"),
		playlistPath,
	}
}

func (s *Session) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
}

func (s *Session) Done() <-chan error {
	if s == nil {
		return nil
	}
	return s.pipe.Done()
}

// DroppedFrames reports how many video frames the pacer gave up on because
// ffmpeg could not keep up. Audio is shortened to match, so the stream stays in
// sync, but the wall clock does not: a count that keeps climbing means the
// capture is too big or too fast for this machine and the caller should lower
// the frame rate or resolution.
func (s *Session) DroppedFrames() int64 {
	if s == nil {
		return 0
	}
	return s.pipe.DroppedFrames()
}

func (s *Session) StderrTail(n int) string {
	if s == nil {
		return ""
	}
	return s.pipe.StderrTail(n)
}

// Close stops the pipeline and removes the segment directory. The removal is
// the Cleanup hook handed to Launch, so it runs after ffmpeg is dead rather
// than out from under it.
func (s *Session) Close() error {
	if s == nil {
		return nil
	}
	return s.pipe.Close()
}

func normalizeOptions(options *Options) (*Options, error) {
	if options == nil {
		return nil, errors.New("nil options")
	}
	if strings.TrimSpace(options.FFmpegPath) == "" {
		return nil, errors.New("ffmpeg path is required")
	}

	opts := *options
	if opts.StreamIndex < 0 {
		return nil, errors.New("stream index must be >= 0")
	}
	if opts.StartupTimeout <= 0 {
		opts.StartupTimeout = defaultStartupTimeout
	}
	if opts.TempDirPrefix == "" {
		opts.TempDirPrefix = defaultTempDirPrefix
	}
	if opts.HLSDeleteThreshold < 1 {
		opts.HLSDeleteThreshold = defaultDeleteThreshold
	}
	if opts.HLSDeleteThreshold > 120 {
		opts.HLSDeleteThreshold = 120
	}
	if opts.HLSTimeSeconds == 0 {
		opts.HLSTimeSeconds = defaultHLSTimeSeconds
	} else if opts.HLSTimeSeconds < 1 {
		opts.HLSTimeSeconds = 1
	}
	if opts.HLSTimeSeconds > 6 {
		opts.HLSTimeSeconds = 6
	}
	if opts.HLSListSize == 0 {
		opts.HLSListSize = defaultHLSListSize
	} else if opts.HLSListSize < 3 {
		opts.HLSListSize = 3
	}
	if opts.HLSListSize > 120 {
		opts.HLSListSize = 120
	}
	if opts.VideoQueueSize == 0 {
		opts.VideoQueueSize = defaultVideoQueueSize
	} else if opts.VideoQueueSize < 128 {
		opts.VideoQueueSize = 128
	}
	if opts.VideoQueueSize > 16384 {
		opts.VideoQueueSize = 16384
	}
	if opts.AudioQueueSize == 0 {
		opts.AudioQueueSize = defaultAudioQueueSize
	} else if opts.AudioQueueSize < 256 {
		opts.AudioQueueSize = 256
	}
	if opts.AudioQueueSize > 32768 {
		opts.AudioQueueSize = 32768
	}
	if opts.AudioChunkSize == 0 {
		opts.AudioChunkSize = pipeline.DefaultAudioChunkSize
	} else if opts.AudioChunkSize < 512 {
		opts.AudioChunkSize = 512
	}
	if opts.AudioChunkSize > 32768 {
		opts.AudioChunkSize = 32768
	}
	if opts.AudioRelayQueue == 0 {
		opts.AudioRelayQueue = pipeline.DefaultAudioRelayQueue
	} else if opts.AudioRelayQueue < 8 {
		opts.AudioRelayQueue = 8
	}
	if opts.AudioRelayQueue > 4096 {
		opts.AudioRelayQueue = 4096
	}

	return &opts, nil
}

func waitForPlaylistReady(path, baseDir string, timeout time.Duration, pipe *pipeline.Session) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	t := time.NewTicker(150 * time.Millisecond)
	defer t.Stop()
	diagT := time.NewTicker(2 * time.Second)
	defer diagT.Stop()

	for {
		select {
		case err := <-pipe.Done():
			if err != nil {
				if pipeline.DebugEnabled() {
					pipeline.DebugPrintf("screencast/hls wait_playlist ffmpeg_exit err=%v", err)
				}
				return fmt.Errorf("screencast ffmpeg exited: %w: %s", err, pipe.StderrTail(300))
			}
			if pipeline.DebugEnabled() {
				pipeline.DebugPrintf("screencast/hls wait_playlist ffmpeg_exit_without_error")
			}
			return errors.New("screencast stream not initialized")
		case <-ctx.Done():
			if pipeline.DebugEnabled() {
				pipeline.DebugPrintf("screencast/hls wait_playlist timeout=%s stderr_tail=%q", timeout, pipe.StderrTail(300))
			}
			return fmt.Errorf("screencast stream not initialized: %s", pipe.StderrTail(300))
		case <-diagT.C:
			if pipeline.DebugEnabled() {
				info, err := os.Stat(path)
				if err != nil {
					pipeline.DebugPrintf("screencast/hls wait_playlist pending playlist=%s stat_err=%q", path, err)
				} else {
					pipeline.DebugPrintf("screencast/hls wait_playlist pending playlist=%s bytes=%d mtime=%s", path, info.Size(), info.ModTime().Format(time.RFC3339Nano))
				}
			}
		case <-t.C:
			if playlistReady(path, baseDir) {
				if pipeline.DebugEnabled() {
					pipeline.DebugPrintf("screencast/hls wait_playlist ready playlist=%s", path)
				}
				return nil
			}
		}
	}
}

func playlistReady(path, baseDir string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		segmentPath := filepath.Join(baseDir, line)
		info, statErr := os.Stat(segmentPath)
		if statErr == nil && !info.IsDir() && info.Size() > 0 {
			return true
		}
	}

	return false
}

func cleanupOldTempDirs(prefix string, maxAge time.Duration) {
	if prefix == "" {
		prefix = defaultTempDirPrefix
	}

	pattern := filepath.Join(os.TempDir(), prefix+"*")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return
	}

	for _, dir := range matches {
		info, statErr := os.Stat(dir)
		if statErr != nil || !info.IsDir() {
			continue
		}
		if time.Since(info.ModTime()) < maxAge {
			continue
		}
		_ = os.RemoveAll(dir)
	}
}
