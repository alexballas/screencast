package avsync

import (
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

	// DefaultChunkSize is the default read chunk the audio pump asks of the
	// capture source.
	DefaultChunkSize = 4096
	// DefaultRelayQueue is the default number of chunks the audio pump buffers
	// (~0.7s of jitter slack. Anything deeper just delays the moment the relay
	// notices it is behind the wall clock.)
	DefaultRelayQueue = 32

	BytesPerPixelBGRA = 4
	// MaxFrameBurst is the most frames the pacer may emit in one tick while
	// catching up after a stall.
	MaxFrameBurst = 8
	// MaxFrameDebtSeconds is the backlog past which catching up is hopeless
	// rather than merely late; the span is abandoned instead.
	MaxFrameDebtSeconds = 2
)
