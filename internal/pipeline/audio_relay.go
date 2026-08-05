package pipeline

import (
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const (
	// Capture audio is always negotiated as 48kHz stereo s16le.
	AudioBytesPerSecond = 48000 * 2 * 2
	// One PCM frame (both channels). Every insert/trim must stay aligned to it.
	AudioFrameBytes = 4

	// Silence is only injected once the audio timeline falls this far behind the
	// wall clock, so normal per-buffer jitter never lengthens the track.
	audioFillThreshold = 60 * time.Millisecond
	// Trim once the timeline runs this far ahead of the wall clock (post-stall
	// burst); stretching past the video is what desyncs the stream.
	audioTrimThreshold = 120 * time.Millisecond
	audioFillChunk     = 20 * time.Millisecond

	DefaultAudioChunkSize = 4096
	// ~0.7s of jitter slack. Anything deeper just delays the moment the relay
	// notices it is behind the wall clock.
	DefaultAudioRelayQueue = 32
)

// audioPump owns the capture audio source for the whole session lifetime.
//
// It starts draining as soon as the session starts, not when ffmpeg connects:
// capture backends buffer whatever they produce, so every millisecond spent
// probing encoders and spawning ffmpeg would otherwise be handed to ffmpeg as
// pre-roll. ffmpeg timestamps raw PCM by byte count, so that pre-roll becomes a
// permanent audio-behind-video offset. discardBuffered drops it at connect time.
// The producer goroutine is not waited on: Session.Close closes the pump before
// it closes the capture source, so a producer parked in src.Read could only be
// released by a later step. Closing the source is what ends it; p.closed just
// stops it promptly when a read happens to return first.
type audioPump struct {
	ch        chan []byte
	closed    chan struct{}
	done      chan struct{} // closed once the producer goroutine has exited
	closeOnce sync.Once
}

func startAudioPump(src io.Reader, chunkSize, queueSize int) *audioPump {
	if chunkSize <= 0 {
		chunkSize = DefaultAudioChunkSize
	}
	if queueSize <= 0 {
		queueSize = DefaultAudioRelayQueue
	}

	p := &audioPump{
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

// startAudioRelay opens the loopback socket ffmpeg reads PCM from and starts
// draining src into it. The URL is what goes on the ffmpeg command line; the
// listener and the pump are the caller's to close.
func startAudioRelay(src io.Reader, chunkSize, queueSize int, skew *timelineSkew, debugEnabled bool) (net.Listener, *audioPump, string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, "", err
	}

	// Drain the capture source now, not on accept: audio produced while we
	// probe encoders and spawn ffmpeg is pre-roll, and ffmpeg would stamp it
	// from byte zero - a permanent audio-behind-video offset.
	pump := startAudioPump(src, chunkSize, queueSize)

	go func(l net.Listener, pump *audioPump) {
		defer l.Close()
		conn, acceptErr := l.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		// Load-bearing, not debug bookkeeping: everything queued before this
		// point is pre-roll ffmpeg would stamp from byte zero, putting the
		// audio track behind the video by the spawn and probe time for the
		// rest of the session.
		dropped := pump.discardBuffered()
		if debugEnabled {
			DebugPrintf(
				"screencast/hls audio_preroll_dropped bytes=%d approx_ms=%d",
				dropped,
				int64(dropped)*1000/AudioBytesPerSecond,
			)
		}
		pump.relay(conn, skew)
	}(l, pump)

	return l, pump, fmt.Sprintf("tcp://%s", l.Addr().String()), nil
}

// offer queues b, dropping the oldest buffer when the reader cannot keep up.
func (p *audioPump) offer(b []byte) {
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

// discardBuffered drops everything captured before ffmpeg attached.
func (p *audioPump) discardBuffered() int {
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

func (p *audioPump) close() {
	if p == nil {
		return
	}
	p.closeOnce.Do(func() {
		close(p.closed)
	})
}

// relay writes the capture audio to dst anchored to the wall clock, so the
// number of PCM bytes handed to ffmpeg always matches elapsed real time. The
// video side is paced the same way (framePacer) from the same anchor, which
// keeps both ffmpeg timelines - each derived from counts, not timestamps - on
// one clock measured from one instant.
//
// skew is the one case where real time is not the target: when the pacer gives
// up on a span of video it can never emit, the audio clock has to skip the same
// span or the stream desyncs by that much forever. Nothing can unwrite the bytes
// already sent, so the relay stops writing until the shortened clock catches up
// to them - both timelines end up equally short.
func (p *audioPump) relay(dst io.Writer, skew *timelineSkew) {
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
	start := skew.startedAt(time.Now())
	var written int64

	// elapsed is time since video pts 0, minus the video the pacer will never
	// emit. Both terms are measured from the same anchor, so every span the
	// pacer abandoned counts - including one it recorded before we connected,
	// which is inside our window too. That also keeps this non-negative: the
	// pacer can only abandon time that has already elapsed since the anchor.
	elapsed := func() time.Duration {
		return time.Since(start) - skew.abandoned()
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
