package hls

import (
	"bytes"
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
)

const (
	defaultDeleteThreshold = 36
	defaultStartupTimeout  = 25 * time.Second
	defaultTempDirPrefix   = "screencast-hls-"
	defaultMaxFrameRate    = 60
	defaultHighResCapFPS   = 30
	defaultVideoQueueSize  = 2048
	defaultAudioQueueSize  = 8192
	defaultAudioChunkSize  = 4096
	defaultAudioRelayQueue = 384
	defaultHLSTimeSeconds  = 2
	defaultHLSListSize     = 24
)

type Options struct {
	FFmpegPath         string
	IncludeAudio       bool
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
	cmd        *exec.Cmd
	audioL     net.Listener
	ffmpegDone chan error
	stderr     *lockedBuffer
	closeOnce  sync.Once
}

func Start(options *Options) (*Session, error) {
	opts, err := normalizeOptions(options)
	if err != nil {
		return nil, err
	}
	debugEnabled := envDebugEnabled()
	if debugEnabled {
		// Umbrella debug mode: emit ffmpeg stderr and print the full command.
		if opts.LogOutput == nil {
			opts.LogOutput = envDebugOutput()
		}
		opts.DebugCommand = true
	}

	cleanupOldTempDirs(opts.TempDirPrefix, 12*time.Hour)

	stream, err := capture.Open(&capture.Options{IncludeAudio: opts.IncludeAudio})
	if err != nil {
		return nil, fmt.Errorf("screencast open: %w", err)
	}

	fps := targetFPS(stream)
	fpsArg := strconv.FormatUint(uint64(fps), 10)
	gopFrames := uint64(fps) * uint64(opts.HLSTimeSeconds)
	if gopFrames == 0 {
		gopFrames = uint64(fps)
	}
	gopArg := strconv.FormatUint(gopFrames, 10)
	tempDir, err := os.MkdirTemp("", opts.TempDirPrefix)
	if err != nil {
		_ = stream.Close()
		return nil, fmt.Errorf("screencast temp dir: %w", err)
	}

	playlistPath := filepath.Join(tempDir, "playlist.m3u8")
	vf := fmt.Sprintf(
		"fps=%s,scale='min(1280,iw)':'min(720,ih)':force_original_aspect_ratio=decrease,scale=trunc(iw/2)*2:trunc(ih/2)*2",
		fpsArg,
	)

	audioEnabled := opts.IncludeAudio && stream.Audio != nil
	audioURL := ""
	var audioL net.Listener
	if audioEnabled {
		audioL, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			_ = stream.Close()
			_ = os.RemoveAll(tempDir)
			return nil, fmt.Errorf("screencast audio listener: %w", err)
		}

		go func(l net.Listener, audio io.ReadCloser) {
			defer l.Close()
			conn, acceptErr := l.Accept()
			if acceptErr != nil {
				return
			}
			defer conn.Close()
			relayAudioWithDrop(conn, audio, opts.AudioChunkSize, opts.AudioRelayQueue)
		}(audioL, stream.Audio)

		audioURL = fmt.Sprintf("tcp://%s", audioL.Addr().String())
		if opts.LogOutput != nil {
			_, _ = fmt.Fprintf(opts.LogOutput, "screencast audio relay: %s\n", audioURL)
		}
	}

	args := []string{
		"-fflags", "nobuffer",
		"-flags", "low_delay",
		"-probesize", "32",
		"-analyzeduration", "0",
		"-thread_queue_size", strconv.Itoa(opts.VideoQueueSize),
		"-f", "rawvideo",
		"-pix_fmt", strings.ToLower(stream.PixelFormat),
		"-s", fmt.Sprintf("%dx%d", stream.Width, stream.Height),
		"-r", fpsArg,
		"-i", "pipe:0",
	}
	if debugEnabled {
		args = append([]string{"-loglevel", "debug"}, args...)
	}
	if audioEnabled {
		args = append(args,
			"-thread_queue_size", strconv.Itoa(opts.AudioQueueSize),
			"-f", "s16le",
			"-ar", "48000",
			"-ac", "2",
			"-i", audioURL,
			"-map", "0:v:0",
			"-map", "1:a:0",
		)
	} else {
		args = append(args,
			"-map", "0:v:0",
			"-an",
		)
	}

	args = append(args,
		"-r", fpsArg,
		"-c:v", "libx264",
		"-preset", "ultrafast",
		"-tune", "zerolatency",
		"-b:v", "2500k",
		"-maxrate", "3000k",
		"-bufsize", "6000k",
		"-pix_fmt", "yuv420p",
		"-vf", vf,
		"-g", gopArg,
		"-keyint_min", gopArg,
		"-sc_threshold", "0",
		"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", opts.HLSTimeSeconds),
	)
	if audioEnabled {
		args = append(args,
			"-af", "aresample=async=1:min_hard_comp=0.100:first_pts=0",
			"-c:a", "aac",
			"-ar", "48000",
			"-ac", "2",
		)
	}
	args = append(args,
		"-f", "hls",
		"-hls_time", strconv.Itoa(opts.HLSTimeSeconds),
		"-hls_list_size", strconv.Itoa(opts.HLSListSize),
		"-hls_allow_cache", "0",
		"-hls_flags", "independent_segments+omit_endlist+delete_segments",
		"-hls_delete_threshold", strconv.Itoa(opts.HLSDeleteThreshold),
		"-hls_segment_filename", filepath.Join(tempDir, "segment_%03d.ts"),
		playlistPath,
	)

	stderrBuf := &lockedBuffer{}
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
	cmd.Stdin = stream
	cmd.Stderr = stderrWriter
	if err := cmd.Start(); err != nil {
		if audioL != nil {
			_ = audioL.Close()
		}
		_ = stream.Close()
		_ = os.RemoveAll(tempDir)
		return nil, fmt.Errorf("screencast ffmpeg start: %w", err)
	}

	s := &Session{
		dir:        tempDir,
		stream:     stream,
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
		opts.AudioChunkSize = defaultAudioChunkSize
	} else if opts.AudioChunkSize < 512 {
		opts.AudioChunkSize = 512
	}
	if opts.AudioChunkSize > 32768 {
		opts.AudioChunkSize = 32768
	}
	if opts.AudioRelayQueue == 0 {
		opts.AudioRelayQueue = defaultAudioRelayQueue
	} else if opts.AudioRelayQueue < 8 {
		opts.AudioRelayQueue = 8
	}
	if opts.AudioRelayQueue > 4096 {
		opts.AudioRelayQueue = 4096
	}

	return &opts, nil
}

func targetFPS(stream *capture.Stream) uint32 {
	frameRate := stream.FrameRate
	if frameRate == 0 {
		frameRate = defaultMaxFrameRate
	}

	target := frameRate
	if target > defaultMaxFrameRate {
		target = defaultMaxFrameRate
	}
	if stream.Width*stream.Height > 1920*1080 && target > defaultHighResCapFPS {
		target = defaultHighResCapFPS
	}

	return target
}

func waitForPlaylistReady(path, baseDir string, timeout time.Duration, ffmpegDone <-chan error, ffmpegStderr *lockedBuffer) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	t := time.NewTicker(150 * time.Millisecond)
	defer t.Stop()

	for {
		select {
		case err := <-ffmpegDone:
			if err != nil {
				return fmt.Errorf("screencast ffmpeg exited: %w: %s", err, ffmpegStderr.Tail(300))
			}
			return errors.New("screencast stream not initialized")
		case <-ctx.Done():
			return fmt.Errorf("screencast stream not initialized: %s", ffmpegStderr.Tail(300))
		case <-t.C:
			if playlistReady(path, baseDir) {
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

func relayAudioWithDrop(dst io.Writer, src io.Reader, chunkSize, queueSize int) {
	if chunkSize <= 0 {
		chunkSize = defaultAudioChunkSize
	}
	if queueSize <= 0 {
		queueSize = defaultAudioRelayQueue
	}

	ch := make(chan []byte, queueSize)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		silence := make([]byte, 3840)
		lastWrite := time.Now().Add(-time.Second)
		t := time.NewTicker(20 * time.Millisecond)
		defer t.Stop()

		for {
			select {
			case b, ok := <-ch:
				if !ok {
					return
				}
				if len(b) == 0 {
					continue
				}
				if _, err := dst.Write(b); err != nil {
					return
				}
				lastWrite = time.Now()
			case <-t.C:
				if time.Since(lastWrite) < 40*time.Millisecond {
					continue
				}
				if _, err := dst.Write(silence); err != nil {
					return
				}
				lastWrite = time.Now()
			}
		}
	}()

	buf := make([]byte, chunkSize)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			b := make([]byte, n)
			copy(b, buf[:n])
			select {
			case ch <- b:
			default:
				select {
				case <-ch:
				default:
				}
				select {
				case ch <- b:
				default:
				}
			}
		}
		if err != nil {
			break
		}
	}

	close(ch)
	wg.Wait()
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

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) Tail(n int) string {
	b.mu.Lock()
	defer b.mu.Unlock()

	s := strings.TrimSpace(b.buf.String())
	if s == "" {
		return "no ffmpeg stderr output"
	}
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
