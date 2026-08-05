package hls

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"go2tv.app/screencast/capture"
	"go2tv.app/screencast/internal/pipeline"
	"go2tv.app/screencast/internal/processutil"
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

type Session struct {
	dir        string
	stream     io.ReadCloser
	audioSrc   io.ReadCloser
	audioPump  *pipeline.AudioPump
	skew       *pipeline.TimelineSkew
	ownAudio   bool
	cmd        *exec.Cmd
	audioL     net.Listener
	ffmpegDone chan error
	stderr     *pipeline.LockedBuffer
	closeOnce  sync.Once
}

// openCapture is capture.Open behind a seam, so tests can drive Start with a
// synthetic frame source instead of the real screen.
var openCapture = capture.Open

func Start(options *Options) (*Session, error) {
	opts, err := normalizeOptions(options)
	if err != nil {
		return nil, err
	}
	debugEnabled := pipeline.DebugEnabled()
	if debugEnabled {
		// Umbrella debug mode: emit ffmpeg stderr and print the full command.
		opts.LogOutput = pipeline.MergeDebugWriter(opts.LogOutput)
		opts.DebugCommand = true
	}

	cleanupOldTempDirs(opts.TempDirPrefix, 12*time.Hour)

	captureStream, err := openCapture(&capture.Options{
		StreamIndex:  opts.StreamIndex,
		IncludeAudio: opts.IncludeAudio,
	})
	if err != nil {
		return nil, fmt.Errorf("screencast open: %w", err)
	}

	fps := pipeline.TargetFPS(captureStream)
	if debugEnabled {
		pipeline.DebugPrintf(
			"screencast/hls fps_target platform=%s width=%d height=%d source=%d target=%d",
			runtime.GOOS,
			captureStream.Width,
			captureStream.Height,
			captureStream.FrameRate,
			fps,
		)
	}
	fpsArg := strconv.FormatUint(uint64(fps), 10)
	gopFrames := uint64(fps) * uint64(opts.HLSTimeSeconds)
	if gopFrames == 0 {
		gopFrames = uint64(fps)
	}
	gopArg := strconv.FormatUint(gopFrames, 10)
	tempDir, err := os.MkdirTemp("", opts.TempDirPrefix)
	if err != nil {
		_ = captureStream.Close()
		return nil, fmt.Errorf("screencast temp dir: %w", err)
	}

	playlistPath := filepath.Join(tempDir, "playlist.m3u8")
	encoderPlan := pipeline.SelectVideoEncoder(opts.FFmpegPath, pipeline.BaseVideoFilter(fpsArg), gopArg, opts.HLSTimeSeconds, opts.LogOutput, debugEnabled)

	// ffmpeg derives rawvideo timestamps from the frame count and -r, so the
	// video timeline only tracks real time if we actually feed it fps frames per
	// second. Every backend is damage-driven and delivers fewer, so pace on all
	// platforms - otherwise video drifts behind the audio without bound.
	skew := &pipeline.TimelineSkew{}
	videoInput, err := pipeline.NewFramePacer(captureStream, fps, skew)
	if err != nil {
		_ = captureStream.Close()
		_ = os.RemoveAll(tempDir)
		return nil, fmt.Errorf("screencast pacer: %w", err)
	}

	audioSource := captureStream.Audio
	ownAudioSource := false
	if opts.IncludeAudio && audioSource == nil {
		audioSource = pipeline.NewSilencePCMReader(48000, 2, 16, 20*time.Millisecond)
		ownAudioSource = true
		if opts.LogOutput != nil {
			_, _ = fmt.Fprintln(opts.LogOutput, "screencast audio source: synthetic_silence")
		}
		if debugEnabled {
			pipeline.DebugPrintf("screencast/hls audio_source=synthetic_silence")
		}
	}

	audioEnabled := opts.IncludeAudio && audioSource != nil
	audioURL := ""
	var (
		audioL net.Listener
		pump   *pipeline.AudioPump
	)
	if audioEnabled {
		audioL, pump, audioURL, err = pipeline.StartAudioRelay(audioSource, opts.AudioChunkSize, opts.AudioRelayQueue, skew, debugEnabled)
		if err != nil {
			if ownAudioSource && audioSource != nil {
				_ = audioSource.Close()
			}
			_ = videoInput.Close()
			_ = os.RemoveAll(tempDir)
			return nil, fmt.Errorf("screencast audio listener: %w", err)
		}

		if opts.LogOutput != nil {
			_, _ = fmt.Fprintf(opts.LogOutput, "screencast audio relay: %s\n", audioURL)
		}
	}

	args := pipeline.FFmpegArgs(pipeline.FFmpegArgsParams{
		Debug:          debugEnabled,
		EncoderPlan:    encoderPlan,
		VideoQueueSize: opts.VideoQueueSize,
		AudioQueueSize: opts.AudioQueueSize,
		PixelFormat:    captureStream.PixelFormat,
		Width:          captureStream.Width,
		Height:         captureStream.Height,
		FpsArg:         fpsArg,
		AudioEnabled:   audioEnabled,
		AudioURL:       audioURL,
		MuxerArgs:      hlsMuxerArgs(opts, tempDir, playlistPath),
	})

	stderrBuf := &pipeline.LockedBuffer{}
	stderrWriter := io.Writer(stderrBuf)
	if opts.LogOutput != nil {
		stderrWriter = io.MultiWriter(opts.LogOutput, stderrWriter)
	}
	if opts.DebugCommand {
		out := opts.LogOutput
		if out == nil {
			out = os.Stderr
		}
		_, _ = fmt.Fprintf(out, "screencast ffmpeg: %s %s\n", opts.FFmpegPath, strings.Join(args, " "))
	}

	cmd := exec.Command(opts.FFmpegPath, args...)
	cmd.Stdin = videoInput
	cmd.Stderr = stderrWriter
	processutil.HideConsoleWindow(cmd)
	if err := cmd.Start(); err != nil {
		if audioL != nil {
			_ = audioL.Close()
		}
		pump.Close()
		if ownAudioSource && audioSource != nil {
			_ = audioSource.Close()
		}
		_ = videoInput.Close()
		_ = os.RemoveAll(tempDir)
		return nil, fmt.Errorf("screencast ffmpeg start: %w", err)
	}

	s := &Session{
		dir:        tempDir,
		stream:     videoInput,
		audioSrc:   audioSource,
		audioPump:  pump,
		skew:       skew,
		ownAudio:   ownAudioSource,
		cmd:        cmd,
		audioL:     audioL,
		ffmpegDone: make(chan error, 1),
		stderr:     stderrBuf,
	}
	runtime.SetFinalizer(s, func(sess *Session) {
		_ = sess.Close()
	})

	go func(c *exec.Cmd, done chan error) {
		done <- c.Wait()
		close(done)
		// Ensure resources are reclaimed even if caller forgets to Close after ffmpeg exits.
		_ = s.Close()
	}(cmd, s.ffmpegDone)

	if err := waitForPlaylistReady(playlistPath, tempDir, opts.StartupTimeout, s.ffmpegDone, s.stderr); err != nil {
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
	return s.ffmpegDone
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
	return s.skew.DroppedFrames()
}

func (s *Session) StderrTail(n int) string {
	if s == nil || s.stderr == nil {
		return ""
	}
	return s.stderr.Tail(n)
}

func (s *Session) Close() error {
	if s == nil {
		return nil
	}

	var out error
	s.closeOnce.Do(func() {
		runtime.SetFinalizer(s, nil)

		if s.cmd != nil && s.cmd.Process != nil {
			err := s.cmd.Process.Kill()
			if err != nil && !errors.Is(err, os.ErrProcessDone) {
				out = errors.Join(out, err)
			}
		}

		if s.audioL != nil {
			out = errors.Join(out, s.audioL.Close())
		}

		s.audioPump.Close()

		if s.stream != nil {
			done := make(chan error, 1)
			go func() {
				done <- s.stream.Close()
			}()
			select {
			case err := <-done:
				out = errors.Join(out, err)
			case <-time.After(1500 * time.Millisecond):
			}
		}
		if s.ownAudio && s.audioSrc != nil {
			out = errors.Join(out, s.audioSrc.Close())
		}

		if s.dir != "" {
			out = errors.Join(out, os.RemoveAll(s.dir))
		}
	})

	return out
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

func waitForPlaylistReady(path, baseDir string, timeout time.Duration, ffmpegDone <-chan error, ffmpegStderr *pipeline.LockedBuffer) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	t := time.NewTicker(150 * time.Millisecond)
	defer t.Stop()
	diagT := time.NewTicker(2 * time.Second)
	defer diagT.Stop()

	for {
		select {
		case err := <-ffmpegDone:
			if err != nil {
				if pipeline.DebugEnabled() {
					pipeline.DebugPrintf("screencast/hls wait_playlist ffmpeg_exit err=%v", err)
				}
				return fmt.Errorf("screencast ffmpeg exited: %w: %s", err, ffmpegStderr.Tail(300))
			}
			if pipeline.DebugEnabled() {
				pipeline.DebugPrintf("screencast/hls wait_playlist ffmpeg_exit_without_error")
			}
			return errors.New("screencast stream not initialized")
		case <-ctx.Done():
			if pipeline.DebugEnabled() {
				pipeline.DebugPrintf("screencast/hls wait_playlist timeout=%s stderr_tail=%q", timeout, ffmpegStderr.Tail(300))
			}
			return fmt.Errorf("screencast stream not initialized: %s", ffmpegStderr.Tail(300))
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
