package avsync

import (
	"io"
	"sync"
	"time"
)

// Pump owns the capture audio source for the whole session lifetime.
//
// It starts draining as soon as the session starts, not when ffmpeg connects:
// capture backends buffer whatever they produce, so every millisecond spent
// probing encoders and spawning ffmpeg would otherwise be handed to ffmpeg as
// pre-roll. ffmpeg timestamps raw PCM by byte count, so that pre-roll becomes a
// permanent audio-behind-video offset. DiscardBuffered drops it at connect time.
// The producer goroutine is not waited on: Session.Close closes the pump before
// it closes the capture source, so a producer parked in src.Read could only be
// released by a later step. Closing the source is what ends it; p.closed just
// stops it promptly when a read happens to return first.
type Pump struct {
	ch        chan []byte
	closed    chan struct{}
	done      chan struct{} // closed once the producer goroutine has exited
	closeOnce sync.Once
}

// StartPump starts draining src in chunks of chunkSize into a bounded queue.
func StartPump(src io.Reader, chunkSize, queueSize int) *Pump {
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}
	if queueSize <= 0 {
		queueSize = DefaultRelayQueue
	}

	p := &Pump{
		ch:     make(chan []byte, queueSize),
		closed: make(chan struct{}),
		done:   make(chan struct{}),
	}

	go func() {
		// done closes last, so a receive on it means nothing can still be
		// offered.
		defer close(p.done)
		defer close(p.ch)

		// Reads can split anywhere, but ffmpeg consumes the relay as one
		// contiguous s16le stream: emitting a partial PCM frame shifts every
		// later sample by that many bytes for the rest of the session. Hold the
		// remainder back until the next read completes it, so every buffer in
		// the pump - and so every trim offset computed against one - is whole.
		buf := make([]byte, chunkSize+AudioFrameBytes)
		carry := 0
		for {
			n, err := src.Read(buf[carry:])
			if n > 0 {
				total := carry + n
				if aligned := alignAudio(total); aligned > 0 {
					b := make([]byte, aligned)
					copy(b, buf[:aligned])
					p.offer(b)
					carry = copy(buf, buf[aligned:total])
				} else {
					carry = total
				}
			}
			if err != nil {
				return
			}
			select {
			case <-p.closed:
				return
			default:
			}
		}
	}()

	return p
}

// offer queues b, dropping the oldest buffer when the reader cannot keep up.
func (p *Pump) offer(b []byte) {
	select {
	case p.ch <- b:
		return
	default:
	}

	select {
	case <-p.ch:
	default:
	}
	select {
	case p.ch <- b:
	default:
	}
}

// DiscardBuffered drops everything captured before ffmpeg attached.
func (p *Pump) DiscardBuffered() int {
	dropped := 0
	for {
		select {
		case b, ok := <-p.ch:
			if !ok {
				return dropped
			}
			dropped += len(b)
		default:
			return dropped
		}
	}
}

// Close stops the producer.
func (p *Pump) Close() {
	if p == nil {
		return
	}
	p.closeOnce.Do(func() {
		close(p.closed)
	})
}

// Relay writes the capture audio to dst anchored to the wall clock, so the
// number of PCM bytes handed to ffmpeg always matches elapsed real time. The
// video side is paced the same way (Pacer) from the same anchor, which keeps
// both ffmpeg timelines - each derived from counts, not timestamps - on one
// clock measured from one instant.
//
// skew is the one case where real time is not the target: when the pacer gives
// up on a span of video it can never emit, the audio clock has to skip the same
// span or the stream desyncs by that much forever. Nothing can unwrite the bytes
// already sent, so the relay stops writing until the shortened clock catches up
// to them - both timelines end up equally short.
func (p *Pump) Relay(dst io.Writer, skew *Skew) {
	silenceBytes := alignAudio(audioBytesFor(audioFillChunk))
	if silenceBytes <= 0 {
		silenceBytes = AudioFrameBytes
	}
	silence := make([]byte, silenceBytes)

	fillThreshold := alignAudio(audioBytesFor(audioFillThreshold))
	trimThreshold := alignAudio(audioBytesFor(audioTrimThreshold))

	ticker := time.NewTicker(audioFillChunk / 2)
	defer ticker.Stop()

	// Measure from the instant video pts 0 was captured, not from the connect:
	// ffmpeg drains a video frame before it opens this socket, and it stamps
	// both inputs from zero regardless. Starting the clock here means the first
	// tick already owes that gap, so the fill path below pads it out and audio
	// byte 0 lines up with video frame 0. Falls back to now when there is no
	// pacer to anchor against.
	start := skew.StartedAt(time.Now())
	var written int64

	// elapsed is time since video pts 0, minus the video the pacer will never
	// emit. Both terms are measured from the same anchor, so every span the
	// pacer abandoned counts - including one it recorded before we connected,
	// which is inside our window too. That also keeps this non-negative: the
	// pacer can only abandon time that has already elapsed since the anchor.
	elapsed := func() time.Duration {
		return time.Since(start) - skew.Abandoned()
	}

	for {
		select {
		case <-p.closed:
			return
		case b, ok := <-p.ch:
			if !ok {
				return
			}
			if len(b) == 0 {
				continue
			}
			if excess := written - audioExpectedBytes(elapsed()); excess > trimThreshold {
				b = b[alignAudio(int(min(excess, int64(len(b))))):]
				if len(b) == 0 {
					continue
				}
			}
			n, err := dst.Write(b)
			written += int64(n)
			if err != nil {
				return
			}
		case <-ticker.C:
			deficit := audioExpectedBytes(elapsed()) - written
			if deficit < fillThreshold {
				continue
			}
			n, err := dst.Write(silence)
			written += int64(n)
			if err != nil {
				return
			}
		}
	}
}

// audioBytesFor stays int64 end to end: byte counts pass 2^31 after ~3.1h of
// relay, and truncating there would flip the clock comparisons negative on
// 32-bit builds - silence would never fill and every buffer would trim to empty.
func audioBytesFor(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	// Scale via milliseconds: nanoseconds * bytes-per-second overflows int64 on
	// long sessions.
	return d.Milliseconds() * AudioBytesPerSecond / 1000
}

func audioExpectedBytes(elapsed time.Duration) int64 {
	return alignAudio(audioBytesFor(elapsed))
}

// alignAudio rounds down to a whole PCM frame so inserts and trims never
// shift the stereo channels against each other.
func alignAudio[T int | int64](n T) T {
	if n <= 0 {
		return 0
	}
	return n - n%AudioFrameBytes
}
