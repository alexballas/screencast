package avsync

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"
)

// countingWriter stands in for the ffmpeg audio socket. It counts bytes, but a
// byte count alone cannot tell relayed capture audio from relay-injected
// silence - a relay that dropped every buffer and padded the hole would produce
// the same total - so it also classifies what it is handed. See floodPCM for
// the pattern it decodes.
type countingWriter struct {
	mu     sync.Mutex
	n      int
	silent int
	lastPC uint32
	seqErr error
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.n += len(p)
	if isSilence(p) {
		w.silent += len(p)
		return len(p), nil
	}

	// Capture audio carries a per-PCM-frame counter, so it must arrive strictly
	// increasing: a repeat means the relay re-sent audio ffmpeg already has, and
	// a counter that does not advance by whole frames means a trim landed
	// mid-frame and shifted the stereo channels.
	for i := 0; i+AudioFrameBytes <= len(p); i += AudioFrameBytes {
		seq := binary.LittleEndian.Uint32(p[i:])
		if w.seqErr == nil && seq <= w.lastPC {
			w.seqErr = fmt.Errorf("relayed PCM frame %d after %d: audio replayed or misaligned", seq, w.lastPC)
		}
		w.lastPC = seq
	}
	return len(p), nil
}

func (w *countingWriter) total() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.n
}

// captured reports the bytes that were real capture audio rather than silence
// the relay generated itself.
func (w *countingWriter) captured() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.n - w.silent
}

func (w *countingWriter) sequenceErr() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seqErr
}

// isSilence reports whether p is a buffer the relay synthesized. floodPCM numbers
// its PCM frames from 1, so an all-zero buffer can only have come from the relay.
func isSilence(p []byte) bool {
	for _, b := range p {
		if b != 0 {
			return false
		}
	}
	return true
}

// Audio captured before ffmpeg connects must never reach the mux: ffmpeg
// timestamps raw PCM by byte count, so pre-roll shifts the whole audio track
// behind the video by its own duration.
func TestAudioPumpDiscardsPreroll(t *testing.T) {
	pr, pw := io.Pipe()

	pump := StartPump(pr, DefaultChunkSize, DefaultRelayQueue)
	defer pump.Close()

	preroll := make([]byte, audioBytesFor(500*time.Millisecond))
	go func() {
		_, _ = pw.Write(preroll)
		_ = pw.Close()
	}()

	// Wait for the producer to reach EOF and exit. Draining while it is still
	// offering races it into refilling the queue between the drain and the
	// emptiness check below.
	select {
	case <-pump.done:
	case <-time.After(2 * time.Second):
		t.Fatal("audio pump did not drain the source")
	}

	if dropped := pump.DiscardBuffered(); dropped == 0 {
		t.Fatalf("discardBuffered() = 0, want the buffered pre-roll")
	}
	if buffered := len(pump.ch); buffered != 0 {
		t.Fatalf("pump still holds %d buffers after discard", buffered)
	}
}

// ffmpeg reads the relay as one contiguous s16le stream, so a buffer carrying a
// partial PCM frame shifts every later sample for the rest of the session. The
// pump must realign whatever the source hands it.
func TestAudioPumpEmitsWholePCMFrames(t *testing.T) {
	// Deliberately odd read sizes, none a multiple of AudioFrameBytes.
	src := &oddSizedReader{sizes: []int{1, 3, 7, 13, 5, 2, 9}, total: 4000}

	pump := StartPump(src, DefaultChunkSize, DefaultRelayQueue)
	defer pump.Close()

	got := 0
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case b, ok := <-pump.ch:
			if !ok {
				if got == 0 {
					t.Fatal("pump emitted nothing")
				}
				return
			}
			if len(b)%AudioFrameBytes != 0 {
				t.Fatalf("pump emitted %d bytes, not a whole %d-byte PCM frame", len(b), AudioFrameBytes)
			}
			got += len(b)
		case <-time.After(200 * time.Millisecond):
			if got == 0 {
				t.Fatal("pump emitted nothing")
			}
			return
		}
	}
}

// oddSizedReader returns short, deliberately unaligned reads, then EOF.
type oddSizedReader struct {
	sizes []int
	next  int
	total int
	sent  int
}

func (r *oddSizedReader) Read(p []byte) (int, error) {
	if r.sent >= r.total {
		return 0, io.EOF
	}
	n := min(min(r.sizes[r.next%len(r.sizes)], len(p)), r.total-r.sent)
	r.next++
	r.sent += n
	for i := range p[:n] {
		p[i] = byte(i)
	}
	return n, nil
}

// Video the pacer gives up on is invisible to ffmpeg, which counts frames and
// bytes rather than reading timestamps. The relay has to drop the same span of
// audio, or the stream is desynced by the abandoned duration permanently and
// every later drop stacks on top.
func TestAudioRelayShortensClockByAbandonedVideo(t *testing.T) {
	const (
		run       = 400 * time.Millisecond
		abandoned = 200 * time.Millisecond
		tolerance = 60 * time.Millisecond
	)

	pr, pw := io.Pipe()
	defer pw.Close()
	floodPCM(t, pw)

	pump := StartPump(pr, DefaultChunkSize, DefaultRelayQueue)
	skew := NewSkew()
	dst := &countingWriter{}

	done := make(chan struct{})
	go func() {
		defer close(done)
		pump.Relay(dst, skew)
	}()

	time.Sleep(run / 2)
	skew.Drop(12, abandoned) // 12 frames at 60fps
	time.Sleep(run / 2)
	pump.Close()
	<-done

	got := int64(dst.total())
	// Wall clock minus the abandoned span. Nothing can unwrite bytes already
	// sent, so the trim dead band is the residual.
	lower := audioBytesFor(run - abandoned - tolerance)
	upper := audioBytesFor(run - abandoned + audioTrimThreshold + tolerance)
	if got < lower || got > upper {
		t.Fatalf("relayed %v of audio over %v of wall clock with %v abandoned, want within [%v, %v]",
			audioDuration(got), run, abandoned, audioDuration(lower), audioDuration(upper))
	}
	// The shortened clock must be paid for by relaying less capture audio, not
	// by swapping it for silence.
	if captured := int64(dst.captured()); captured < lower {
		t.Fatalf("relayed %v of capture audio (%v of it injected silence), want at least %v",
			audioDuration(captured), audioDuration(got-captured), audioDuration(lower))
	}
	if err := dst.sequenceErr(); err != nil {
		t.Fatal(err)
	}
}

// ffmpeg drains a video frame from pipe:0 before it opens the audio socket, and
// stamps both inputs from pts 0 regardless. Measuring from the connect would put
// audio byte 0 later in real time than video frame 0, so audio would lead the
// picture by that gap for the whole session; the relay has to owe the gap from
// its first tick and pay it out.
func TestAudioRelayAnchorsToVideoStart(t *testing.T) {
	const (
		// Comfortably past audioTrimThreshold: an unanchored relay overshoots the
		// run by that much on its own as the queued audio drains, so a smaller gap
		// would not separate the two.
		anchorLag = 500 * time.Millisecond
		run       = 400 * time.Millisecond
		tolerance = 120 * time.Millisecond
	)

	pr, pw := io.Pipe()
	defer pw.Close()
	floodPCM(t, pw)

	pump := StartPump(pr, DefaultChunkSize, DefaultRelayQueue)
	skew := NewSkew()
	// ffmpeg took the first video frame anchorLag ago; the audio input is only
	// being connected now.
	skew.MarkStart(time.Now().Add(-anchorLag))
	dst := &countingWriter{}

	done := make(chan struct{})
	go func() {
		defer close(done)
		pump.Relay(dst, skew)
	}()

	time.Sleep(run)
	pump.Close()
	<-done

	got := int64(dst.total())
	// Anchored at video pts 0, so the run plus the gap that preceded it. Relaying
	// only the run means audio byte 0 is anchorLag late against video frame 0.
	lower := audioBytesFor(run + anchorLag - tolerance)
	upper := audioBytesFor(run + anchorLag + audioTrimThreshold + tolerance)
	if got < lower || got > upper {
		t.Fatalf("relayed %v of audio %v after video pts 0, want within [%v, %v]",
			audioDuration(got), run+anchorLag, audioDuration(lower), audioDuration(upper))
	}
	if err := dst.sequenceErr(); err != nil {
		t.Fatal(err)
	}
}

func audioDuration(bytes int64) time.Duration {
	return (time.Duration(bytes) * time.Second / AudioBytesPerSecond).Round(time.Millisecond)
}

// floodPCM keeps w oversupplied with capture audio until the test ends, so the
// relay's byte count is decided by its clock rather than by running dry. A
// one-shot dump drains within milliseconds and then silently measures the
// silence-fill path instead of the trim path.
//
// Every PCM frame carries its own sequence number, starting at 1. Zeros would
// make capture audio and relay-injected silence indistinguishable downstream: a
// relay that dropped all of it and padded the gap would relay the same number of
// identical bytes and pass on the count alone.
func floodPCM(t *testing.T, w *io.PipeWriter) {
	t.Helper()

	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })

	go func() {
		chunk := make([]byte, DefaultChunkSize)
		seq := uint32(1)
		for {
			select {
			case <-stop:
				return
			default:
			}
			for i := 0; i+AudioFrameBytes <= len(chunk); i += AudioFrameBytes {
				binary.LittleEndian.PutUint32(chunk[i:], seq)
				seq++
			}
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}()
}

// The relay must hand ffmpeg one second of PCM per second of wall clock,
// whatever the source does, because the video side is paced on the same clock.
func TestAudioRelayTracksWallClock(t *testing.T) {
	const (
		run       = 400 * time.Millisecond
		tolerance = 150 * time.Millisecond
	)

	tests := []struct {
		name  string
		flood bool
		lower int64
		upper int64
		// wantCapture: the budget has to be met with real capture audio. Without
		// this, discarding every buffer and padding with silence passes.
		wantCapture bool
	}{
		{
			// Source offers far more audio than real time: excess must be
			// trimmed, not stretched past the video.
			name:        "burst is trimmed",
			flood:       true,
			lower:       audioBytesFor(run - tolerance),
			upper:       audioBytesFor(run + audioTrimThreshold + tolerance),
			wantCapture: true,
		},
		{
			// Source produces nothing: silence must be injected to hold the gap
			// open, otherwise later audio lands early against the video.
			name:  "stall is filled with silence",
			lower: audioBytesFor(run - audioFillThreshold - tolerance),
			upper: audioBytesFor(run + tolerance),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pr, pw := io.Pipe()
			defer pw.Close()
			if tc.flood {
				floodPCM(t, pw)
			}

			pump := StartPump(pr, DefaultChunkSize, DefaultRelayQueue)
			dst := &countingWriter{}

			done := make(chan struct{})
			go func() {
				defer close(done)
				pump.Relay(dst, nil)
			}()

			time.Sleep(run)
			pump.Close()
			<-done

			got := int64(dst.total())
			if got < tc.lower || got > tc.upper {
				t.Fatalf("relayed %v of audio over %v of wall clock, want within [%v, %v]",
					audioDuration(got), run, audioDuration(tc.lower), audioDuration(tc.upper))
			}
			if got%AudioFrameBytes != 0 {
				t.Fatalf("relayed %d bytes, not aligned to a %d-byte PCM frame", got, AudioFrameBytes)
			}

			captured := int64(dst.captured())
			if tc.wantCapture {
				if captured < tc.lower {
					t.Fatalf("relayed %v of capture audio (%v of it injected silence) over %v, want at least %v of capture audio",
						audioDuration(captured), audioDuration(got-captured), run, audioDuration(tc.lower))
				}
			} else if captured != 0 {
				t.Fatalf("relayed %v of capture audio from a source that produced none", audioDuration(captured))
			}
			if err := dst.sequenceErr(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
