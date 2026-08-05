package hls

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"go2tv.app/screencast/capture"
	"go2tv.app/screencast/internal/pipeline"
)

// closeBound is what Session.Close is allowed to take. It is deliberately far
// looser than the 1500ms the implementation waits on the video input: the point
// is to catch a Close that never returns, not to pin the timeout.
const closeBound = 5 * time.Second

const fakeFFmpegEnv = "SCREENCAST_TEST_FAKE_FFMPEG"

// TestMain lets the test binary stand in for ffmpeg. Start execs whatever path
// it is handed, and a re-exec of this binary is the one stand-in that behaves
// the same on every platform the package builds for - no shell, no quoting, no
// dependency on a real encoder being installed.
func TestMain(m *testing.M) {
	if os.Getenv(fakeFFmpegEnv) != "" {
		os.Exit(runFakeFFmpeg(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// runFakeFFmpeg answers the encoder probe with a list holding no hardware
// encoder at all, so selection lands on the software plan without probing
// anything, then plays the part of a running muxer: it writes the one segment
// and playlist waitForPlaylistReady is looking for and drains stdin until it is
// killed.
func runFakeFFmpeg(args []string) int {
	if len(args) >= 2 && args[0] == "-hide_banner" && args[1] == "-encoders" {
		fmt.Println("Encoders:")
		fmt.Println(" V..... libx264              fake")
		return 0
	}
	if len(args) == 0 {
		return 1
	}

	playlist := args[len(args)-1]
	segment := filepath.Join(filepath.Dir(playlist), "segment_000.ts")
	if err := os.WriteFile(segment, []byte("fake segment"), 0o600); err != nil {
		return 1
	}
	playlistBody := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:1.000,\nsegment_000.ts\n"
	if err := os.WriteFile(playlist, []byte(playlistBody), 0o600); err != nil {
		return 1
	}

	_, _ = io.Copy(io.Discard, os.Stdin)
	return 0
}

func fakeFFmpegPath(t *testing.T) string {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locating the test binary: %v", err)
	}
	t.Setenv(fakeFFmpegEnv, "1")
	return exe
}

// stubSource is a capture backend reduced to its lifecycle behaviour: it
// produces video and audio until it is closed, and closing the video reader
// tears both down. Real backends work the same way, and Session.Close depends
// on it - closing the video input is what releases the audio pump's producer
// from a parked read.
type stubSource struct {
	width  uint32
	height uint32
	fps    uint32

	closed    chan struct{}
	closeOnce sync.Once

	// blockClose, when non-nil, holds the video Close open until it is closed.
	// Capture teardown can block on any of these platforms, which is why the
	// wait on it is bounded rather than direct.
	blockClose chan struct{}
}

func newStubSource(blockClose chan struct{}) *stubSource {
	return &stubSource{
		width:      64,
		height:     32,
		fps:        30,
		closed:     make(chan struct{}),
		blockClose: blockClose,
	}
}

func (s *stubSource) stream() *capture.Stream {
	return &capture.Stream{
		ReadCloser:  &stubVideo{src: s},
		Audio:       &stubAudio{src: s},
		Width:       s.width,
		Height:      s.height,
		FrameRate:   s.fps,
		PixelFormat: capture.PixelFormatBGRA,
	}
}

func (s *stubSource) signalClosed() {
	s.closeOnce.Do(func() { close(s.closed) })
}

// wait sleeps for d and reports whether the source is still open.
func (s *stubSource) wait(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-s.closed:
		return false
	case <-t.C:
		return true
	}
}

type stubVideo struct {
	src *stubSource
	buf []byte
}

func (v *stubVideo) Read(p []byte) (int, error) {
	if len(v.buf) == 0 {
		if !v.src.wait(time.Second / time.Duration(v.src.fps)) {
			return 0, io.EOF
		}
		v.buf = make([]byte, int(v.src.width)*int(v.src.height)*pipeline.BytesPerPixelBGRA)
	}

	n := copy(p, v.buf)
	v.buf = v.buf[n:]
	return n, nil
}

// Close ends the source first and only then blocks, the way a backend that
// hangs somewhere in teardown does: everything reading from it is released,
// but the caller is not.
func (v *stubVideo) Close() error {
	v.src.signalClosed()
	if v.src.blockClose != nil {
		<-v.src.blockClose
	}
	return nil
}

// stubAudio delivers 48kHz stereo s16le silence in 10ms chunks.
type stubAudio struct {
	src *stubSource
	buf []byte
}

func (a *stubAudio) Read(p []byte) (int, error) {
	if len(a.buf) == 0 {
		if !a.src.wait(10 * time.Millisecond) {
			return 0, io.EOF
		}
		a.buf = make([]byte, 1920)
	}

	n := copy(p, a.buf)
	a.buf = a.buf[n:]
	return n, nil
}

func (a *stubAudio) Close() error {
	a.src.signalClosed()
	return nil
}

// startWithStub starts a real session - real argument assembly, real process,
// real playlist wait - against a synthetic capture backend and a stand-in for
// ffmpeg.
func startWithStub(t *testing.T, src *stubSource) *Session {
	t.Helper()

	restore := openCapture
	openCapture = func(*capture.Options) (*capture.Stream, error) { return src.stream(), nil }
	t.Cleanup(func() { openCapture = restore })

	sess, err := Start(&Options{
		FFmpegPath:     fakeFFmpegPath(t),
		IncludeAudio:   true,
		HLSTimeSeconds: 1,
		StartupTimeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	return sess
}

// Close joins the video input's Close, which joins the capture backend's, and a
// backend can block there. It also has to survive being entered more than once:
// the caller, the ffmpeg-wait goroutine and the finalizer all call it.
func TestSessionCloseIsBoundedAndIdempotent(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once

	sess := startWithStub(t, newStubSource(release))
	// Registered after the session, so it runs before the session's own cleanup
	// Close: releasing the source last would deadlock that Close behind this
	// one and bury whatever this test was reporting.
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	dir := sess.Dir()
	if dir == "" {
		t.Fatal("Session.Dir() is empty after a successful Start")
	}

	for i := 1; i <= 2; i++ {
		done := make(chan error, 1)
		go func() { done <- sess.Close() }()

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Close() #%d error = %v", i, err)
			}
		case <-time.After(closeBound):
			t.Fatalf("Close() #%d did not return within %v while the capture source blocked in Close", i, closeBound)
		}
	}

	// Bounding the wait must not cost the teardown: the temp dir still goes,
	// and it goes after ffmpeg is dead rather than while it is still writing
	// segments into it.
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("session dir %s still present after Close: stat err = %v", dir, err)
	}
}

// Start hands out an ffmpeg process and a fan of goroutines - the pacer, its
// reader, the audio pump, the listener's accept loop, the process waiter - none
// of which the caller can see. Close is the only handle on any of it, so what
// it leaves behind is part of the contract.
func TestSessionStartCloseLeavesNothingRunning(t *testing.T) {
	baseline := settledGoroutines()

	sess := startWithStub(t, newStubSource(nil))

	if err := sess.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Done carries what cmd.Wait returned, so a receive here means the child is
	// dead and reaped rather than merely signalled.
	var waitErr error
	select {
	case waitErr = <-sess.Done():
	case <-time.After(closeBound):
		t.Fatal("ffmpeg was still running after Close: cmd.Wait never returned")
	}

	// It must have been killed rather than left to notice its inputs going
	// away. The stand-in returns 0 once its stdin reaches EOF, and Wait then
	// reports the broken stdin copy instead of an exit status, so an ExitError
	// is what says the process was killed while it was still reading.
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		t.Fatalf("ffmpeg was not killed by Close: cmd.Wait returned %v", waitErr)
	}

	assertNoExtraGoroutines(t, baseline)
}

// settledGoroutines waits for the goroutine count to stop moving, so a baseline
// taken here is not inflated by whatever the previous test is still unwinding.
func settledGoroutines() int {
	deadline := time.Now().Add(2 * time.Second)
	last := runtime.NumGoroutine()
	stable := 0

	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		n := runtime.NumGoroutine()
		if n != last {
			last, stable = n, 0
			continue
		}
		stable++
		if stable == 3 {
			break
		}
	}

	return last
}

func assertNoExtraGoroutines(t *testing.T, baseline int) {
	t.Helper()

	deadline := time.Now().Add(closeBound)
	for {
		n := runtime.NumGoroutine()
		if n <= baseline {
			return
		}
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			buf = buf[:runtime.Stack(buf, true)]
			t.Fatalf("%d goroutines still running after Close, baseline was %d:\n%s", n, baseline, buf)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
