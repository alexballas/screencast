package pipeline

import (
	"io"
	"sync"
	"time"
)

type silencePCMReader struct {
	bytesPerSecond int
	chunkBytes     int
	closed         chan struct{}
	closeOnce      sync.Once
}

func newSilencePCMReader(sampleRate, channels, bitsPerSample int, chunkDuration time.Duration) io.ReadCloser {
	bytesPerSecond := sampleRate * channels * (bitsPerSample / 8)
	if bytesPerSecond <= 0 {
		bytesPerSecond = 48000 * 2 * 2
	}
	chunkBytes := int((int64(bytesPerSecond) * chunkDuration.Milliseconds()) / 1000)
	if chunkBytes <= 0 {
		chunkBytes = 3840
	}
	return &silencePCMReader{
		bytesPerSecond: bytesPerSecond,
		chunkBytes:     chunkBytes,
		closed:         make(chan struct{}),
	}
}

func (r *silencePCMReader) Read(p []byte) (int, error) {
	select {
	case <-r.closed:
		return 0, io.EOF
	default:
	}
	if len(p) == 0 {
		return 0, nil
	}

	n := r.chunkBytes
	if n > len(p) {
		n = len(p)
	}
	if n <= 0 {
		n = len(p)
	}
	clear(p[:n])

	wait := time.Duration(int64(n) * int64(time.Second) / int64(r.bytesPerSecond))
	if wait <= 0 {
		return n, nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-r.closed:
		return 0, io.EOF
	case <-timer.C:
		return n, nil
	}
}

func (r *silencePCMReader) Close() error {
	r.closeOnce.Do(func() {
		close(r.closed)
	})
	return nil
}
