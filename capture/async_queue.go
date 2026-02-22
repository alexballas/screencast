package capture

import (
	"io"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultCallbackVideoQueue = 4
	defaultCallbackAudioQueue = 256
)

type asyncPipeWriter struct {
	platform string
	streamID int
	kind     string
	dst      *io.PipeWriter

	queue chan []byte
	done  chan struct{}

	closeOnce sync.Once
	wg        sync.WaitGroup

	lastSlowLog atomic.Int64
	lastDropLog atomic.Int64
	dropped     atomic.Uint64
}

func newAsyncPipeWriter(platform string, streamID int, kind string, dst *io.PipeWriter, queueSize int) *asyncPipeWriter {
	if dst == nil || queueSize <= 0 {
		return nil
	}
	w := &asyncPipeWriter{
		platform: platform,
		streamID: streamID,
		kind:     kind,
		dst:      dst,
		queue:    make(chan []byte, queueSize),
		done:     make(chan struct{}),
	}
	w.wg.Add(1)
	go w.loop()
	return w
}

func (w *asyncPipeWriter) Enqueue(frame []byte) {
	if w == nil || len(frame) == 0 {
		return
	}

	select {
	case <-w.done:
		return
	default:
	}

	select {
	case w.queue <- frame:
		return
	default:
	}

	// Queue full: drop oldest to keep callback path non-blocking and low-latency.
	dropped := false
	select {
	case <-w.queue:
		dropped = true
	default:
	}
	if dropped {
		total := w.dropped.Add(1)
		if captureShouldLogSlowWrite(&w.lastDropLog, time.Second) {
			captureDebugf(
				"platform=%s stream=%d dropped_%s_frame total=%d queue=%d",
				w.platform,
				w.streamID,
				w.kind,
				total,
				len(w.queue),
			)
		}
	}

	select {
	case w.queue <- frame:
	default:
		total := w.dropped.Add(1)
		if captureShouldLogSlowWrite(&w.lastDropLog, time.Second) {
			captureDebugf(
				"platform=%s stream=%d dropped_%s_frame total=%d queue=%d",
				w.platform,
				w.streamID,
				w.kind,
				total,
				len(w.queue),
			)
		}
	}
}

func (w *asyncPipeWriter) Close() {
	if w == nil {
		return
	}
	w.closeOnce.Do(func() {
		close(w.done)
		w.wg.Wait()
	})
}

func (w *asyncPipeWriter) loop() {
	defer w.wg.Done()

	for {
		select {
		case <-w.done:
			return
		case b := <-w.queue:
			if len(b) == 0 {
				continue
			}
			start := time.Now()
			_, err := w.dst.Write(b)
			if err != nil {
				captureDebugf("platform=%s stream=%d %s_write_err=%v", w.platform, w.streamID, w.kind, err)
				return
			}
			d := time.Since(start)
			if d > 50*time.Millisecond && captureShouldLogSlowWrite(&w.lastSlowLog, time.Second) {
				captureDebugf(
					"platform=%s stream=%d slow_%s_write duration=%s bytes=%d queue=%d",
					w.platform,
					w.streamID,
					w.kind,
					d,
					len(b),
					len(w.queue),
				)
			}
		}
	}
}
