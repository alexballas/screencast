// Package ts turns a desktop screencast into a live MPEG-TS stream (H.264, plus
// AAC when IncludeAudio is set). It shares the capture and audio/video sync
// machinery with the hls package through internal/pipeline, but muxes to a
// single continuous transport stream instead of a segmented playlist, so the
// output is exposed as one ReadCloser rather than an HTTP directory. Nothing
// here speaks a transport protocol: the stream suits any consumer of a live TS,
// whether that is a DLNA renderer, a Chromecast, a raw socket or a local
// player.
//
// The muxer emits plain ISO MPEG-2 TS: 188-byte packets, no timestamp prefix.
// That matches the DLNA profile AVC_TS_MP_HD_AAC_MULT5_ISO, so a DLNA caller
// can advertise that profile - but only with IncludeAudio set, since the
// profile promises an AAC track and a video-only stream has none.
//
// It deliberately does not run in m2ts mode (-mpegts_m2ts_mode 1), even though
// the 192-byte timestamped packets that mode produces are what the suffixless
// profiles ask for. In m2ts mode ffmpeg follows Blu-ray convention and declares
// AAC in the PMT as stream_type 0x06, PES private data, on PID 0x1100 - with no
// registration descriptor to say what that private data is, because a Blu-ray
// player is expected to resolve it from the disc's clip database, which a
// stream has none of. ffmpeg's own demuxer probes the elementary stream and
// plays the audio regardless, so ffprobe and VLC hide the bug, but a renderer
// that trusts the PMT drops the track: GStreamer's tsdemux creates no pad for
// it, leaving every GStreamer-based renderer (gmediarender among them) playing
// the picture in silence. In plain TS the same AAC is declared 0x0F, ADTS AAC,
// and demuxes everywhere.
package ts

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"go2tv.app/screencast/capture"
	"go2tv.app/screencast/internal/pipeline"
)

const (
	defaultStartupTimeout = 60 * time.Second
	// Buffered before the probe declares the stream ready: enough for a PAT and
	// PMT plus a few video packets, and it lets the first reads drain from
	// memory while ffmpeg keeps the pipe fed.
	defaultStartupBytes = tsPacketSize * 16
	// A valid stream announces its PAT and PMT in the first few KiB. Once this
	// much has arrived without one, the output is not the TS we asked for and
	// waiting out StartupTimeout only buffers megabytes to throw away.
	maxProbeBytes = 1 << 20
	// Nothing cuts this stream into segments, so a keyframe every second is
	// enough to let a renderer join quickly.
	gopSeconds = 1
)

// MPEG-TS constants: every packet is 188 bytes opening with the sync byte, and
// a valid stream announces a PAT (PID 0) followed by the PMT it points at.
const (
	tsSyncByte   = 0x47
	tsPacketSize = 188
	patPID       = 0x0000
	tableIDPAT   = 0x00
	tableIDPMT   = 0x02
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

// Session is a live MPEG-TS screencast ready for a renderer to stream.
type Session struct {
	stream io.ReadCloser
	pipe   *pipeline.Session
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

func (s *Session) Close() error {
	if s == nil {
		return nil
	}
	return s.pipe.Close()
}

// openCapture is capture.Open behind a seam, so tests can drive Start with a
// synthetic frame source instead of the real screen.
var openCapture = capture.Open

func Start(options *Options) (*Session, error) {
	opts, err := normalizeOptions(options)
	if err != nil {
		return nil, err
	}

	prep, err := pipeline.Prepare(&pipeline.Config{
		FFmpegPath:      opts.FFmpegPath,
		IncludeAudio:    opts.IncludeAudio,
		StreamIndex:     opts.StreamIndex,
		GOPSeconds:      gopSeconds,
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

	pipe, err := prep.Launch(pipeline.LaunchOptions{
		MuxerArgs: tsMuxerArgs(),
		Stdout:    true,
	})
	if err != nil {
		return nil, err
	}

	// The probe reads the head of the stream to prove ffmpeg is really emitting
	// a transport stream, then hands those same bytes on to the caller.
	probe := newStreamProbe()
	go probe.run(pipe.Stdout())

	s := &Session{
		stream: &prefixReader{probe: probe, src: pipe.Stdout()},
		pipe:   pipe,
	}

	if err := waitForStream(probe, pipe, opts.StartupTimeout); err != nil {
		_ = s.Close()
		return nil, err
	}

	return s, nil
}

// tsMuxerArgs terminate the ffmpeg command line: a plain ISO transport stream
// of 188-byte packets, which is what the _ISO DLNA transport stream profiles
// expect. See the package comment for why m2ts mode is not used.
func tsMuxerArgs() []string {
	return []string{
		"-f", "mpegts",
		"-muxdelay", "0",
		"pipe:1",
	}
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
// ffmpeg is actually producing a valid transport stream (sync bytes, a PAT and
// the PMT it points at) before telling the TV to play.
type streamProbe struct {
	mu         sync.Mutex
	buf        bytes.Buffer
	valid      bool
	reason     string
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
		gaveUp := !p.valid && p.buf.Len() >= maxProbeBytes
		if gaveUp {
			p.reason = fmt.Sprintf(
				"no PAT/PMT in the first %d bytes (ffmpeg may not be producing an MPEG-TS stream)",
				maxProbeBytes,
			)
		}
		p.mu.Unlock()
		if p.valid || gaveUp || err != nil {
			return
		}
	}
}

// Valid reports whether the buffered stream passed startup validation, i.e. the
// probe closed ready because it found a well-formed transport stream, not
// because ffmpeg exited early.
func (p *streamProbe) Valid() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.valid
}

// Reason explains why validation was abandoned, or "" when the probe simply ran
// out of input.
func (p *streamProbe) Reason() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reason
}

// hasPATAndPMT reports whether the buffered bytes contain a PAT (PID 0) and a
// PMT section on one of the program PIDs the PAT lists. Everything is packet
// aligned, so a stream that fails this is not a valid transport stream.
func (p *streamProbe) hasPATAndPMT() bool {
	data := p.buf.Bytes()
	pmtPIDs := make(map[uint16]struct{})
	for off := 0; off+tsPacketSize <= len(data); off += tsPacketSize {
		pkt := data[off : off+tsPacketSize]
		pid, sec, ok := psiSectionStart(pkt)
		if !ok || pid != patPID || pkt[sec] != tableIDPAT {
			continue
		}
		sectionLength := int(pkt[sec+1]&0x0F)<<8 | int(pkt[sec+2])
		// section_length covers everything after it including the trailing
		// CRC32, so the program entries stop 4 bytes short of the section end.
		end := sec + 3 + sectionLength - 4
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
	for off := 0; off+tsPacketSize <= len(data); off += tsPacketSize {
		pkt := data[off : off+tsPacketSize]
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
	case 0, 2:
		// 0 is reserved and 2 is adaptation field only: neither carries a
		// payload, so pkt[4] is not a pointer_field.
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

func waitForStream(probe *streamProbe, pipe *pipeline.Session, timeout time.Duration) error {
	tail := func() string { return pipe.StderrTail(300) }

	select {
	case <-probe.ready:
		if probe.Valid() {
			if pipeline.DebugEnabled() {
				pipeline.DebugPrintf("screencast/ts wait_stream ready buffered=%d", probe.Buffered())
			}
			return nil
		}
		// The probe gave up on a stream ffmpeg is still feeding it, so its own
		// diagnosis beats waiting for an exit that is not coming.
		if reason := probe.Reason(); reason != "" {
			if pipeline.DebugEnabled() {
				pipeline.DebugPrintf("screencast/ts wait_stream abandoned reason=%q", reason)
			}
			return fmt.Errorf("screencast stream not initialized: %s: %s", reason, tail())
		}
		// ffmpeg closed the stream before producing a valid transport stream.
		select {
		case err, ok := <-pipe.Done():
			if pipeline.DebugEnabled() {
				pipeline.DebugPrintf("screencast/ts wait_stream ffmpeg_exit err=%v", err)
			}
			if !ok || err == nil {
				return errors.New("screencast stream not initialized")
			}
			return fmt.Errorf("screencast ffmpeg exited: %w: %s", err, tail())
		case <-time.After(2 * time.Second):
		}
		return fmt.Errorf("screencast stream not initialized: %s", tail())
	case err, ok := <-pipe.Done():
		if pipeline.DebugEnabled() {
			pipeline.DebugPrintf("screencast/ts wait_stream ffmpeg_exit err=%v", err)
		}
		if !ok {
			return errors.New("screencast stream not initialized")
		}
		if err != nil {
			return fmt.Errorf("screencast ffmpeg exited: %w: %s", err, tail())
		}
		return errors.New("screencast stream not initialized")
	case <-time.After(timeout):
		if pipeline.DebugEnabled() {
			pipeline.DebugPrintf("screencast/ts wait_stream timeout=%s stderr_tail=%q", timeout, tail())
		}
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
	if opts.StreamIndex < 0 {
		return nil, errors.New("stream index must be >= 0")
	}
	if opts.StartupTimeout <= 0 {
		opts.StartupTimeout = defaultStartupTimeout
	}

	queues := pipeline.QueueSizes{
		Video:      opts.VideoQueueSize,
		Audio:      opts.AudioQueueSize,
		AudioChunk: opts.AudioChunkSize,
		AudioRelay: opts.AudioRelayQueue,
	}.Normalize()
	opts.VideoQueueSize = queues.Video
	opts.AudioQueueSize = queues.Audio
	opts.AudioChunkSize = queues.AudioChunk
	opts.AudioRelayQueue = queues.AudioRelay

	return &opts, nil
}
