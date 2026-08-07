package pipeline

// Default queue sizes for the two ffmpeg inputs. They sit here beside
// DefaultAudioChunkSize and DefaultAudioRelayQueue because all four are
// pipeline knobs: every output format exposes the same set, and none of them
// mean anything different for one muxer than for another.
const (
	DefaultVideoQueueSize = 2048
	DefaultAudioQueueSize = 8192
)

// QueueSizes are the pipeline's buffering knobs as a caller supplies them: the
// two ffmpeg input queues, the size of one audio relay write, and how many of
// those writes may be in flight.
type QueueSizes struct {
	Video      int
	Audio      int
	AudioChunk int
	AudioRelay int
}

// Normalize resolves each knob against its default and bounds. The bounds are
// what the pipeline can work with rather than a matter of taste - too small a
// queue stalls ffmpeg, too large a one hides backpressure behind megabytes of
// buffer - so an out-of-range value is clamped rather than rejected: a caller
// asking for the extreme wants the extreme this pipeline has.
func (q QueueSizes) Normalize() QueueSizes {
	return QueueSizes{
		Video:      resolve(q.Video, DefaultVideoQueueSize, 128, 16384),
		Audio:      resolve(q.Audio, DefaultAudioQueueSize, 256, 32768),
		AudioChunk: resolve(q.AudioChunk, DefaultAudioChunkSize, 512, 32768),
		AudioRelay: resolve(q.AudioRelay, DefaultAudioRelayQueue, 8, 4096),
	}
}

// resolve settles one knob: zero means unset and takes the default, anything
// else is held inside [lo, hi].
func resolve(v, def, lo, hi int) int {
	switch {
	case v == 0:
		return def
	case v < lo:
		return lo
	case v > hi:
		return hi
	default:
		return v
	}
}
