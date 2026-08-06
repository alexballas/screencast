package ts

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

// patPacketCRC is patPacket carrying an explicit CRC32 instead of zeros, so a
// test can tell a parsed program entry apart from the trailing checksum.
func patPacketCRC(crc [4]byte) []byte {
	return m2tsPacket([]byte{
		0x00, 0x00, 0xB0, 0x0D,
		0x00, 0x01, 0xC1, 0x00, 0x00,
		0x00, 0x01, 0x01, 0x00,
		crc[0], crc[1], crc[2], crc[3],
	}, patPID, true)
}

// pmtPacket is a PMT on PID 0x0100 declaring an AVC elementary stream.
func pmtPacket() []byte {
	return pmtPacketOn(0x0100)
}

// pmtPacketOn is pmtPacket on an arbitrary PID.
func pmtPacketOn(pid uint16) []byte {
	return m2tsPacket([]byte{
		0x00, 0x02, 0xB0, 0x11,
		0x00, 0x01, 0xC1, 0x00, 0x00,
		0xE1, 0x00,
		0xF0, 0x00,
		0x1B, 0xE1, 0x01, 0xF0, 0x00,
		0x00, 0x00, 0x00, 0x00,
	}, pid, true)
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

// TestStreamProbeIgnoresPATChecksum guards the PAT program loop against running
// one entry too far into the section CRC32. The CRC here decodes to program
// 0xABCD on PID 0x0123, and the stream plants a PMT on exactly that PID: if the
// checksum is parsed as a program entry the bogus PMT satisfies the probe, even
// though the only real program (PID 0x0100) never announces one.
func TestStreamProbeIgnoresPATChecksum(t *testing.T) {
	const crcPID = 0x0123
	crc := [4]byte{0xAB, 0xCD, 0xE1, 0x23}

	var in bytes.Buffer
	for range 32 {
		_, _ = in.Write(patPacketCRC(crc))
		_, _ = in.Write(pmtPacketOn(crcPID))
	}

	probe := newStreamProbe()
	go probe.run(bytes.NewReader(in.Bytes()))

	select {
	case <-probe.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("probe never signaled")
	}
	if probe.Valid() {
		t.Fatal("probe parsed the PAT CRC32 as a program entry and validated a stream with no PMT")
	}
}

// TestPSISectionStartRejectsNoPayload covers adaptation_field_control 0, which
// is reserved and carries no payload: pkt[4] is not a pointer_field there, so
// treating it as one can synthesise a PSI section out of arbitrary bytes.
func TestPSISectionStartRejectsNoPayload(t *testing.T) {
	pkt := make([]byte, tsPacketSize)
	pkt[0] = tsSyncByte
	pkt[1] = 0x40 // payload_unit_start on PID 0
	pkt[2] = 0x00
	pkt[3] = 0x00 // adaptation_field_control = 0, no payload
	pkt[4] = 0x00 // would read as pointer_field = 0
	pkt[5] = tableIDPAT

	if _, _, ok := psiSectionStart(pkt); ok {
		t.Fatal("psiSectionStart accepted a packet with no payload")
	}
}

// TestStreamProbeGivesUpOnUnboundedGarbage verifies the probe stops at
// maxProbeBytes instead of buffering a never-validating stream until the
// startup timeout expires.
func TestStreamProbeGivesUpOnUnboundedGarbage(t *testing.T) {
	probe := newStreamProbe()
	go probe.run(endlessReader{})

	select {
	case <-probe.ready:
	case <-time.After(10 * time.Second):
		t.Fatal("probe never gave up on an endless invalid stream")
	}
	if probe.Valid() {
		t.Fatal("probe validated garbage")
	}
	if probe.Reason() == "" {
		t.Fatal("probe gave no reason for abandoning the stream")
	}
	if got := probe.Buffered(); got > maxProbeBytes+32*1024 {
		t.Fatalf("probe buffered %d bytes, want at most ~%d", got, maxProbeBytes)
	}
}

// endlessReader is a stream that never ends and never validates.
type endlessReader struct{}

func (endlessReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0xFF
	}
	return len(p), nil
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
