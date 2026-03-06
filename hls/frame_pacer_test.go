package hls

import (
	"bytes"
	"io"
	"testing"
	"time"

	"go2tv.app/screencast/capture"
)

func TestFramePacerRepeatsLatestFrame(t *testing.T) {
	const (
		width  = 2
		height = 1
		fps    = 50
	)

	frameSize := width * height * bytesPerPixelBGRA
	srcR, srcW := io.Pipe()
	stream := &capture.Stream{
		ReadCloser:  srcR,
		Width:       width,
		Height:      height,
		FrameRate:   fps,
		PixelFormat: capture.PixelFormatBGRA,
	}

	paced, err := newFramePacer(stream, fps)
	if err != nil {
		t.Fatalf("newFramePacer() error = %v", err)
	}
	defer paced.Close()

	first := bytes.Repeat([]byte{0x11}, frameSize)
	second := bytes.Repeat([]byte{0x22}, frameSize)
	done := make(chan struct{})

	go func() {
		_, _ = srcW.Write(first)
		time.Sleep(35 * time.Millisecond)
		_, _ = srcW.Write(second)
		<-done
		_ = srcW.Close()
	}()

	gotFirst := readExactWithTimeout(t, paced, frameSize, 300*time.Millisecond)
	gotRepeat := readExactWithTimeout(t, paced, frameSize, 300*time.Millisecond)
	gotSecond := readExactWithTimeout(t, paced, frameSize, 300*time.Millisecond)

	if !bytes.Equal(gotFirst, first) {
		t.Fatalf("first frame = %v, want %v", gotFirst, first)
	}
	if !bytes.Equal(gotRepeat, first) {
		t.Fatalf("repeat frame = %v, want %v", gotRepeat, first)
	}
	if !bytes.Equal(gotSecond, second) {
		t.Fatalf("second frame = %v, want %v", gotSecond, second)
	}
	close(done)
}

func TestRawFrameSizeBGRA(t *testing.T) {
	got, err := rawFrameSize(1280, 720, capture.PixelFormatBGRA)
	if err != nil {
		t.Fatalf("rawFrameSize() error = %v", err)
	}
	if want := 1280 * 720 * bytesPerPixelBGRA; got != want {
		t.Fatalf("rawFrameSize() = %d, want %d", got, want)
	}
}

func readExactWithTimeout(t *testing.T, r io.Reader, n int, timeout time.Duration) []byte {
	t.Helper()

	type result struct {
		buf []byte
		err error
	}

	done := make(chan result, 1)
	go func() {
		buf := make([]byte, n)
		_, err := io.ReadFull(r, buf)
		done <- result{buf: buf, err: err}
	}()

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("io.ReadFull() error = %v", res.err)
		}
		return res.buf
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for %d bytes", n)
		return nil
	}
}
