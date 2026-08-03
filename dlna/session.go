// Package dlna turns a desktop screencast into a live MPEG-TS stream (H.264 +
// AAC) for DLNA/UPnP media renderers. It shares the capture and audio/video
// sync machinery with the hls package but muxes to a single continuous
// transport stream instead of a segmented playlist, so the output is exposed
// as one ReadCloser rather than an HTTP directory.
//
// The muxer runs in m2ts mode (-mpegts_m2ts_mode 1): packets are 192 bytes
// with a 4-byte timestamp prefix, which is what the DLNA profile
// AVC_TS_MP_HD_AAC_MULT5 and the video/vnd.dlna.mpeg-tts media type require.
package dlna

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"go2tv.app/screencast/capture"
	"go2tv.app/screencast/internal/avsync"
	"go2tv.app/screencast/internal/debugutil"
	"go2tv.app/screencast/internal/ffmpegnc"
	"go2tv.app/screencast/internal/processutil"
)

const (
	defaultStartupTimeout = 60 * time.Second
	defaultMaxFrameRate   = 60
	defaultHighResCapFPS  = 30
	defaultMaxWidth       = 1280
	defaultMaxHeight      = 720
	// Buffered before the probe declares the stream ready: enough for a PAT and
	// PMT plus a few video packets, and it lets the first reads drain from
	// memory while ffmpeg keeps the pipe fed.
	defaultStartupBytes   = m2tsPacketSize * 16
	defaultVideoQueueSize = 2048
	defaultAudioQueueSize = 8192
)

// MPEG-TS constants for the m2ts container (-mpegts_m2ts_mode 1): every packet
// is 188 bytes prefixed with a 4-byte timestamp, and a valid stream opens with
// a PAT (PID 0) followed by the PMT it points at.
const (
	tsSyncByte     = 0x47
	tsPacketSize   = 188
	m2tsHeaderSize = 4
	m2tsPacketSize = tsPacketSize + m2tsHeaderSize
	patPID         = 0x0000
	tableIDPAT     = 0x00
	tableIDPMT     = 0x02
)

type Options struct {
	FFmpegPath   string
	IncludeAudio bool
	// StreamIndex selects the display/stream index passed to capture.Open (default 0).
	StreamIndex     int
	VideoQueueSize  int
	AudioQueueSize  int
	AudioChunkSize  int
	AudioRelayQueue int
	StartupTimeout  time.Duration
	LogOutput       io.Writer
	DebugCommand    bool
}

// Session is a live MPEG-TS screencast ready for a DLNA renderer to stream.
type Session struct {
	stream     io.ReadCloser
	videoInput io.ReadCloser
	audioSrc   io.ReadCloser
	audioPump  *avsync.Pump
	skew       *avsync.Skew
	ownAudio   bool
	cmd        *exec.Cmd
	audioL     net.Listener
	ffmpegDone chan error
	stderr     *avsync.LockedBuffer
	closeOnce  sync.Once
	closeErr   error
}

// Stream returns the live MPEG-TS output. Closing it is a no-op on purpose:
// closing the ffmpeg stdout pipe would SIGPIPE the encoder when a TV
// disconnects. The session owns the ffmpeg process lifetime.
func (s *Session) Stream() io.ReadCloser {
	if s == nil {
		return nil
	}
	return s.stream
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

	s.closeOnce.Do(func() {
		runtime.SetFinalizer(s, nil)
		if s.cmd != nil && s.cmd.Process != nil {
			if err := s.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				s.closeErr = errors.Join(s.closeErr, err)
			}
		}

		if s.audioL != nil {
			s.closeErr = errors.Join(s.closeErr, s.audioL.Close())
		}

		s.audioPump.Close()

		if s.videoInput != nil {
			done := make(chan error, 1)
			go func() {
				done <- s.videoInput.Close()
			}()
			select {
			case err := <-done:
				s.closeErr = errors.Join(s.closeErr, err)
			case <-time.After(1500 * time.Millisecond):
			}
		}

		if s.ownAudio && s.audioSrc != nil {
			s.closeErr = errors.Join(s.closeErr, s.audioSrc.Close())
		}
	})

	return s.closeErr
}

// openCapture is capture.Open behind a seam, so tests can drive Start with a
// synthetic frame source instead of the real screen.
var openCapture = capture.Open

func Start(options *Options) (*Session, error) {
	opts, err := normalizeOptions(options)
	if err != nil {
		return nil, err
	}
	debugEnabled := debugutil.Enabled()
	if debugEnabled {
		opts.LogOutput = debugutil.MergeWriter(opts.LogOutput)
		opts.DebugCommand = true
	}

	captureStream, err := openCapture(&capture.Options{
		StreamIndex:  opts.StreamIndex,
		IncludeAudio: opts.IncludeAudio,
	})
	if err != nil {
		return nil, fmt.Errorf("screencast open: %w", err)
	}

	cleanup := func() {
		_ = captureStream.Close()
	}

	fps := targetFPS(captureStream)
	if debugEnabled {
		debugutil.Printf(
			"screencast/dlna fps_target platform=%s width=%d height=%d source=%d target=%d",
			runtime.GOOS,
			captureStream.Width,
			captureStream.Height,
			captureStream.FrameRate,
			fps,
		)
	}
	fpsArg := strconv.FormatUint(uint64(fps), 10)
	gopArg := strconv.FormatUint(uint64(fps), 10)

	baseVideoFilter := fmt.Sprintf(
		"fps=%s,scale='min(%d,iw)':'min(%d,ih)':force_original_aspect_ratio=decrease,scale=trunc(iw/2)*2:trunc(ih/2)*2",
		fpsArg, defaultMaxWidth, defaultMaxHeight,
	)
	encoderPlan := ffmpegnc.SelectVideoEncoder(opts.FFmpegPath, baseVideoFilter, gopArg, 1, opts.LogOutput, debugEnabled)

	skew := avsync.NewSkew()
	videoInput, err := avsync.NewPacer(captureStream, fps, skew)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("screencast pacer: %w", err)
	}

	audioSource := captureStream.Audio
	ownAudioSource := false
	if opts.IncludeAudio && audioSource == nil {
		audioSource = avsync.NewSilencePCMReader(48000, 2, 16, 20*time.Millisecond)
		ownAudioSource = true
		if opts.LogOutput != nil {
			_, _ = fmt.Fprintln(opts.LogOutput, "screencast audio source: synthetic_silence")
		}
	}

	audioEnabled := opts.IncludeAudio && audioSource != nil
	audioURL := ""
	var (
		audioL net.Listener
		pump   *avsync.Pump
	)
	if audioEnabled {
		audioL, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			if ownAudioSource {
				_ = audioSource.Close()
			}
			_ = videoInput.Close()
			return nil, fmt.Errorf("screencast audio listener: %w", err)
		}

		pump = avsync.StartPump(audioSource, opts.AudioChunkSize, opts.AudioRelayQueue)

		go func(l net.Listener, pump *avsync.Pump) {
			defer l.Close()
			conn, acceptErr := l.Accept()
			if acceptErr != nil {
				return
			}
			defer conn.Close()
			dropped := pump.DiscardBuffered()
			if debugEnabled {
				debugutil.Printf(
					"screencast/dlna audio_preroll_dropped bytes=%d approx_ms=%d",
					dropped,
					int64(dropped)*1000/avsync.AudioBytesPerSecond,
				)
			}
			pump.Relay(conn, skew)
		}(audioL, pump)

		audioURL = fmt.Sprintf("tcp://%s", audioL.Addr().String())
		if opts.LogOutput != nil {
			_, _ = fmt.Fprintf(opts.LogOutput, "screencast audio relay: %s\n", audioURL)
		}
	}

	args := []string{}
	if debugEnabled {
		args = append(args, "-loglevel", "debug")
	}
	args = append(args, encoderPlan.GlobalArgs...)
	args = append(args,
		"-fflags", "nobuffer",
		"-flags", "low_delay",
		"-probesize", "32",
		"-analyzeduration", "0",
		"-thread_queue_size", strconv.Itoa(opts.VideoQueueSize),
		"-f", "rawvideo",
		"-pix_fmt", strings.ToLower(captureStream.PixelFormat),
		"-s", fmt.Sprintf("%dx%d", captureStream.Width, captureStream.Height),
		"-r", fpsArg,
		"-i", "pipe:0",
	)
	if audioEnabled {
		args = append(args,
			"-thread_queue_size", strconv.Itoa(opts.AudioQueueSize),
			"-fflags", "nobuffer",
			"-probesize", "32",
			"-analyzeduration", "0",
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
	)
	if strings.TrimSpace(encoderPlan.VideoFilter) != "" {
		args = append(args, "-vf", encoderPlan.VideoFilter)
	}
	args = append(args, encoderPlan.CodecArgs...)
	if audioEnabled {
		args = append(args,
			"-af", "aresample=async=1:min_hard_comp=0.100:first_pts=0",
			"-c:a", "aac",
			"-ar", "48000",
			"-ac", "2",
		)
	}
	args = append(args,
		"-f", "mpegts",
		"-mpegts_m2ts_mode", "1",
		"-muxdelay", "0",
		"pipe:1",
	)

	stderrBuf := avsync.NewLockedBuffer()
	stderrWriter := io.Writer(stderrBuf)
	if opts.LogOutput != nil {
		stderrWriter = io.MultiWriter(opts.LogOutput, stderrBuf)
	}
	if opts.DebugCommand {
		out := opts.LogOutput
		if out == nil {
			out = os.Stderr
		}
		_, _ = fmt.Fprintf(out, "screencast ffmpeg: %s %s\n", opts.FFmpegPath, strings.Join(args, " "))
	}

	pr, pw, err := os.Pipe()
	if err != nil {
		if audioL != nil {
			_ = audioL.Close()
		}
		pump.Close()
		if ownAudioSource {
			_ = audioSource.Close()
		}
		_ = videoInput.Close()
		return nil, fmt.Errorf("screencast pipe: %w", err)
	}

	cmd := exec.Command(opts.FFmpegPath, args...)
	cmd.Stdin = videoInput
	cmd.Stdout = pw
	cmd.Stderr = stderrWriter
	processutil.HideConsoleWindow(cmd)

	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		_ = pr.Close()
		if audioL != nil {
			_ = audioL.Close()
		}
		pump.Close()
		if ownAudioSource {
			_ = audioSource.Close()
		}
		_ = videoInput.Close()
		return nil, fmt.Errorf("screencast ffmpeg start: %w", err)
	}
	// The child owns the write end now.
	_ = pw.Close()

	probe := newStreamProbe()
	go probe.run(pr)

	ffmpegDone := make(chan error, 1)
	s := &Session{
		stream:     &prefixReader{probe: probe, src: pr},
		videoInput: videoInput,
		audioSrc:   audioSource,
		audioPump:  pump,
		skew:       skew,
		ownAudio:   ownAudioSource,
		cmd:        cmd,
		audioL:     audioL,
		ffmpegDone: ffmpegDone,
		stderr:     stderrBuf,
	}
	runtime.SetFinalizer(s, func(sess *Session) {
		_ = sess.Close()
	})

	go func(c *exec.Cmd, pr *os.File) {
		ffmpegDone <- c.Wait()
		close(ffmpegDone)
		_ = pr.Close()
		_ = s.Close()
	}(cmd, pr)

	if err := waitForStream(probe, s, opts.StartupTimeout); err != nil {
		_ = s.Close()
		return nil, err
	}

	return s, nil
}

// prefixReader yields the startup bytes collected by the probe before
// switching to the raw ffmpeg stdout pipe.
type prefixReader struct {
	probe *streamProbe
	src   io.ReadCloser
}

func (r *prefixReader) Read(p []byte) (int, error) {
	if n := r.probe.ReadFromBuffer(p); n > 0 {
		return n, nil
	}
	return r.src.Read(p)
}

func (r *prefixReader) Close() error {
	return nil
}

// streamProbe buffers the first bytes of the MPEG-TS stream so we can verify
// ffmpeg is actually producing a valid m2ts stream (sync bytes, a PAT and the
// PMT it points at) before telling the TV to play.
type streamProbe struct {
	mu         sync.Mutex
	buf        bytes.Buffer
	valid      bool
	ready      chan struct{}
	signalSent bool
}

func newStreamProbe() *streamProbe {
	return &streamProbe{ready: make(chan struct{})}
}

func (p *streamProbe) run(pr io.Reader) {
	defer p.signal()

	tmp := make([]byte, 32*1024)
	for {
		n, err := pr.Read(tmp)
		p.mu.Lock()
		if n > 0 {
			p.buf.Write(tmp[:n])
		}
		if p.buf.Len() >= defaultStartupBytes && p.hasPATAndPMT() {
			p.valid = true
		}
		p.mu.Unlock()
		if p.valid || err != nil {
			return
		}
	}
}

// Valid reports whether the buffered stream passed startup validation, i.e. the
// probe closed ready because it found a well-formed m2ts stream, not because
// ffmpeg exited early.
func (p *streamProbe) Valid() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.valid
}

// hasPATAndPMT reports whether the buffered bytes contain a PAT (PID 0) and a
// PMT section on one of the program PIDs the PAT lists. Everything is packet
// aligned, so a stream that fails this is not a valid m2ts stream.
func (p *streamProbe) hasPATAndPMT() bool {
	data := p.buf.Bytes()
	pmtPIDs := make(map[uint16]struct{})
	for off := 0; off+m2tsPacketSize <= len(data); off += m2tsPacketSize {
		pkt := data[off+m2tsHeaderSize : off+m2tsPacketSize]
		pid, sec, ok := psiSectionStart(pkt)
		if !ok || pid != patPID || pkt[sec] != tableIDPAT {
			continue
		}
		sectionLength := int(pkt[sec+1]&0x0F)<<8 | int(pkt[sec+2])
		end := sec + 3 + sectionLength
		for e := sec + 8; e+4 <= end && e+4 <= tsPacketSize; e += 4 {
			programNumber := uint16(pkt[e])<<8 | uint16(pkt[e+1])
			if programNumber != 0 {
				pmtPIDs[uint16(pkt[e+2]&0x1F)<<8|uint16(pkt[e+3])] = struct{}{}
			}
		}
	}
	if len(pmtPIDs) == 0 {
		return false
	}
	for off := 0; off+m2tsPacketSize <= len(data); off += m2tsPacketSize {
		pkt := data[off+m2tsHeaderSize : off+m2tsPacketSize]
		pid, sec, ok := psiSectionStart(pkt)
		if !ok {
			continue
		}
		if _, isPMT := pmtPIDs[pid]; isPMT && pkt[sec] == tableIDPMT {
			return true
		}
	}
	return false
}

// psiSectionStart returns the PID and the offset of the first PSI section byte
// (after pointer_field) for a payload-unit-start packet, or ok=false when the
// packet carries no PSI section start.
func psiSectionStart(pkt []byte) (pid uint16, sec int, ok bool) {
	if len(pkt) < tsPacketSize || pkt[0] != tsSyncByte {
		return 0, 0, false
	}
	pid = uint16(pkt[1]&0x1F)<<8 | uint16(pkt[2])
	if pkt[1]&0x40 == 0 {
		return pid, 0, false
	}
	switch afc := (pkt[3] >> 4) & 0x03; afc {
	case 2:
		return pid, 0, false
	case 3:
		payload := 4 + int(pkt[4]) + 1
		if payload+3 >= tsPacketSize {
			return pid, 0, false
		}
		sec = payload + 1 + int(pkt[payload])
	default:
		sec = 4 + 1 + int(pkt[4])
	}
	if sec+3 > tsPacketSize {
		return pid, 0, false
	}
	return pid, sec, true
}

func (p *streamProbe) signal() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.signalSent {
		p.signalSent = true
		close(p.ready)
	}
}

// ReadFromBuffer drains the buffered startup bytes (thread-safe).
func (p *streamProbe) ReadFromBuffer(dst []byte) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.buf.Len() == 0 || len(dst) == 0 {
		return 0
	}
	n := copy(dst, p.buf.Bytes())
	p.buf.Next(n)
	return n
}

func (p *streamProbe) Buffered() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.buf.Len()
}

func waitForStream(probe *streamProbe, s *Session, timeout time.Duration) error {
	tail := func() string {
		if s == nil {
			return "no ffmpeg stderr output"
		}
		return s.StderrTail(300)
	}

	select {
	case <-probe.ready:
		if probe.Valid() {
			return nil
		}
		// ffmpeg closed the stream before producing a valid m2ts stream.
		select {
		case err, ok := <-s.ffmpegDone:
			if !ok || err == nil {
				return errors.New("screencast stream not initialized")
			}
			return fmt.Errorf("screencast ffmpeg exited: %w: %s", err, tail())
		case <-time.After(2 * time.Second):
		}
		return fmt.Errorf("screencast stream not initialized: %s", tail())
	case err, ok := <-s.ffmpegDone:
		if !ok {
			return errors.New("screencast stream not initialized")
		}
		if err != nil {
			return fmt.Errorf("screencast ffmpeg exited: %w: %s", err, tail())
		}
		return errors.New("screencast stream not initialized")
	case <-time.After(timeout):
		return fmt.Errorf("screencast stream not initialized: %s", tail())
	}
}

func normalizeOptions(options *Options) (*Options, error) {
	if options == nil {
		return nil, errors.New("nil options")
	}
	if strings.TrimSpace(options.FFmpegPath) == "" {
		return nil, errors.New("ffmpeg path is required")
	}

	opts := *options
	if opts.VideoQueueSize == 0 {
		opts.VideoQueueSize = defaultVideoQueueSize
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
		opts.AudioChunkSize = avsync.DefaultChunkSize
	} else if opts.AudioChunkSize < 512 {
		opts.AudioChunkSize = 512
	}
	if opts.AudioChunkSize > 32768 {
		opts.AudioChunkSize = 32768
	}
	if opts.AudioRelayQueue == 0 {
		opts.AudioRelayQueue = avsync.DefaultRelayQueue
	} else if opts.AudioRelayQueue < 8 {
		opts.AudioRelayQueue = 8
	}
	if opts.AudioRelayQueue > 4096 {
		opts.AudioRelayQueue = 4096
	}
	if opts.StartupTimeout <= 0 {
		opts.StartupTimeout = defaultStartupTimeout
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
