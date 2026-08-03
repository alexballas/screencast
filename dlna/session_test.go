package dlna

import (
	"bytes"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"go2tv.app/screencast/capture"
)

func TestTargetFPS(t *testing.T) {
	tests := []struct {
		name     string
		stream   *capture.Stream
		expected uint32
	}{
		{
			name:     "1440p capped to 30",
			stream:   &capture.Stream{Width: 2560, Height: 1440, FrameRate: 30, PixelFormat: capture.PixelFormatBGRA},
			expected: 30,
		},
		{
			name:     "4k capped to 30",
			stream:   &capture.Stream{Width: 3840, Height: 2160, FrameRate: 60, PixelFormat: capture.PixelFormatBGRA},
			expected: 30,
		},
		{
			name:     "1080p keeps max frame rate",
			stream:   &capture.Stream{Width: 1920, Height: 1080, FrameRate: 60, PixelFormat: capture.PixelFormatBGRA},
			expected: 60,
		},
		{
			name:     "unknown frame rate falls back to max frame rate",
			stream:   &capture.Stream{Width: 1280, Height: 720, FrameRate: 0, PixelFormat: capture.PixelFormatBGRA},
			expected: 60,
		},
		{
			name:     "high source frame rate capped to max frame rate",
			stream:   &capture.Stream{Width: 1280, Height: 720, FrameRate: 144, PixelFormat: capture.PixelFormatBGRA},
			expected: 60,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := targetFPS(tc.stream); got != tc.expected {
				t.Fatalf("targetFPS() = %d, want %d", got, tc.expected)
			}
		})
	}
}

func TestNormalizeOptions(t *testing.T) {
	if _, err := normalizeOptions(nil); err == nil {
		t.Fatal("normalizeOptions(nil) = nil error, want error")
	}
	if _, err := normalizeOptions(&Options{}); err == nil {
		t.Fatal("normalizeOptions without ffmpeg path = nil error, want error")
	}

	opts, err := normalizeOptions(&Options{FFmpegPath: "/usr/bin/ffmpeg"})
	if err != nil {
		t.Fatalf("normalizeOptions() error = %v", err)
	}
	if opts.VideoQueueSize != defaultVideoQueueSize {
		t.Fatalf("VideoQueueSize = %d, want %d", opts.VideoQueueSize, defaultVideoQueueSize)
	}
	if opts.AudioQueueSize != defaultAudioQueueSize {
		t.Fatalf("AudioQueueSize = %d, want %d", opts.AudioQueueSize, defaultAudioQueueSize)
	}
	if opts.StartupTimeout != defaultStartupTimeout {
		t.Fatalf("StartupTimeout = %v, want %v", opts.StartupTimeout, defaultStartupTimeout)
	}
}

// m2tsPacket builds one 192-byte m2ts packet (4-byte timestamp prefix, 188-byte
// TS packet) with a payload-unit-start header for the given PID.
func m2tsPacket(payload []byte, pid uint16, payloadStart bool) []byte {
	pkt := make([]byte, m2tsPacketSize)
	for i := range pkt {
		pkt[i] = 0xFF
	}
	pkt[0], pkt[1], pkt[2], pkt[3] = 0x00, 0x0F, 0xFE, 0x80
	pkt[4] = tsSyncByte
	b1 := byte(pid>>8) & 0x1F
	if payloadStart {
		b1 |= 0x40
	}
	pkt[5] = b1
	pkt[6] = byte(pid)
	pkt[7] = 0x10
	copy(pkt[8:], payload)
	return pkt
}

// patPacket is a PAT (PID 0) pointing program 1 at PMT PID 0x0100.
func patPacket() []byte {
	return m2tsPacket([]byte{
		0x00, 0x00, 0xB0, 0x0D,
		0x00, 0x01, 0xC1, 0x00, 0x00,
		0x00, 0x01, 0x01, 0x00,
		0x00, 0x00, 0x00, 0x00,
	}, patPID, true)
}

// pmtPacket is a PMT on PID 0x0100 declaring an AVC elementary stream.
func pmtPacket() []byte {
	return m2tsPacket([]byte{
		0x00, 0x02, 0xB0, 0x11,
		0x00, 0x01, 0xC1, 0x00, 0x00,
		0xE1, 0x00,
		0xF0, 0x00,
		0x1B, 0xE1, 0x01, 0xF0, 0x00,
		0x00, 0x00, 0x00, 0x00,
	}, 0x0100, true)
}

// TestStreamProbe verifies the startup buffer accumulates enough valid MPEG-TS
// bytes (PAT + PMT) to signal readiness and that the prefix reader drains it
// before falling through to the live source.
func TestStreamProbe(t *testing.T) {
	var in bytes.Buffer
	for range 32 {
		_, _ = in.Write(patPacket())
		_, _ = in.Write(pmtPacket())
	}

	probe := newStreamProbe()
	go probe.run(bytes.NewReader(in.Bytes()))

	select {
	case <-probe.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("probe never became ready")
	}

	buffered := probe.Buffered()
	if buffered < defaultStartupBytes {
		t.Fatalf("probe buffered %d bytes, want at least %d", buffered, defaultStartupBytes)
	}

	src := io.NopCloser(bytes.NewReader(patPacket()))
	prefix := &prefixReader{probe: probe, src: src}

	all, err := io.ReadAll(prefix)
	if err != nil {
		t.Fatalf("prefixReader read error = %v", err)
	}
	if len(all) != buffered+m2tsPacketSize {
		t.Fatalf("prefixReader yielded %d bytes, want %d (buffer then live source)", len(all), buffered+m2tsPacketSize)
	}
}

// TestStreamProbeRejectsGarbage verifies a probe fed byte-aligned but PSI-less
// packets never signals readiness.
func TestStreamProbeRejectsGarbage(t *testing.T) {
	var in bytes.Buffer
	for range 64 {
		pkt := make([]byte, m2tsPacketSize)
		pkt[0] = 0x47
		_, _ = in.Write(pkt)
	}

	probe := newStreamProbe()
	go probe.run(bytes.NewReader(in.Bytes()))

	select {
	case <-probe.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("probe never signaled")
	}
	if probe.Valid() {
		t.Fatal("probe validated a stream without PAT/PMT")
	}
}

// TestProbeHandlesMalformedPackets feeds adversarial packet bytes through the
// PSI parser to make sure it never panics on out-of-range reads or accepts
// garbage as a valid stream.
func TestProbeHandlesMalformedPackets(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	probe := newStreamProbe()
	for i := range 4096 {
		pkt := make([]byte, m2tsPacketSize)
		if _, err := rng.Read(pkt); err != nil {
			t.Fatalf("rand read: %v", err)
		}
		if i%2 == 0 {
			pkt[4] = tsSyncByte
		}
		_, _ = probe.buf.Write(pkt)
	}
	if probe.hasPATAndPMT() {
		t.Fatal("malformed data validated as a TS stream")
	}
}

// TestStartStreamsM2TS drives Start end to end with a fake ffmpeg so no real
// ffmpeg or display is needed: the seam replaces capture.Open and the fake
// binary emits valid 192-byte m2ts packets (PAT + PMT with the sync byte at
// offset 4, the m2ts shape).
func TestStartStreamsM2TS(t *testing.T) {
	oldOpen := openCapture
	t.Cleanup(func() { openCapture = oldOpen })

	srcR, _ := io.Pipe()
	openCapture = func(_ *capture.Options) (*capture.Stream, error) {
		return &capture.Stream{
			ReadCloser:  srcR,
			Width:       4,
			Height:      4,
			FrameRate:   60,
			PixelFormat: capture.PixelFormatBGRA,
		}, nil
	}

	s, err := Start(&Options{
		FFmpegPath:     writeFakeFFmpeg(t),
		StartupTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// The startup probe must have buffered real TS bytes before Start returned.
	buf := make([]byte, defaultStartupBytes+m2tsPacketSize)
	n, err := io.ReadFull(s.Stream(), buf)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if n < defaultStartupBytes {
		t.Fatalf("read %d bytes, want at least %d", n, defaultStartupBytes)
	}
	for i := 0; i+m2tsPacketSize <= n; i += m2tsPacketSize {
		if buf[i+m2tsHeaderSize] != 0x47 {
			t.Fatalf("packet at offset %d missing TS sync byte: got 0x%02x, want 0x47", i, buf[i+m2tsHeaderSize])
		}
	}
}

// writeFakeFFmpeg builds a tiny Go program that answers the encoder probes the
// way a real ffmpeg would (reports libx264, rejects the lavfi hardware probe)
// and otherwise streams valid PAT/PMT m2ts packets to stdout forever.
func writeFakeFFmpeg(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(fakeFFmpegSrc), 0o644); err != nil {
		t.Fatalf("write fake ffmpeg source: %v", err)
	}

	bin := filepath.Join(dir, "fake-ffmpeg")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput()
	if err != nil {
		t.Fatalf("build fake ffmpeg: %v: %s", err, out)
	}
	return bin
}

const fakeFFmpegSrc = `package main

import (
	"fmt"
	"os"
	"strings"
)

const (
	tsSyncByte     = 0x47
	tsPacketSize   = 188
	m2tsHeaderSize = 4
)

var patPayload = []byte{
	0x00, 0x00, 0xB0, 0x0D,
	0x00, 0x01, 0xC1, 0x00, 0x00,
	0x00, 0x01, 0x01, 0x00,
	0x00, 0x00, 0x00, 0x00,
}

var pmtPayload = []byte{
	0x00, 0x02, 0xB0, 0x11,
	0x00, 0x01, 0xC1, 0x00, 0x00,
	0xE1, 0x00,
	0xF0, 0x00,
	0x1B, 0xE1, 0x01, 0xF0, 0x00,
	0x00, 0x00, 0x00, 0x00,
}

func packet(payload []byte, pid uint16, payloadStart bool) []byte {
	pkt := make([]byte, tsPacketSize+m2tsHeaderSize)
	for i := range pkt {
		pkt[i] = 0xFF
	}
	pkt[0], pkt[1], pkt[2], pkt[3] = 0x00, 0x0F, 0xFE, 0x80
	pkt[4] = tsSyncByte
	b1 := byte(pid>>8) & 0x1F
	if payloadStart {
		b1 |= 0x40
	}
	pkt[5] = b1
	pkt[6] = byte(pid)
	pkt[7] = 0x10
	copy(pkt[8:], payload)
	return pkt
}

func main() {
	args := strings.Join(os.Args[1:], " ")
	if strings.Contains(args, "lavfi") {
		os.Exit(1)
	}
	if strings.Contains(args, "-encoders") {
		fmt.Fprintln(os.Stdout, " V..... libx264           fake")
		return
	}
	pat := packet(patPayload, 0x0000, true)
	pmt := packet(pmtPayload, 0x0100, true)
	out := os.Stdout
	for {
		if _, err := out.Write(pat); err != nil {
			return
		}
		if _, err := out.Write(pmt); err != nil {
			return
		}
	}
}
`
