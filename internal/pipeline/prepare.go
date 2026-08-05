package pipeline

import (
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strconv"
	"sync"
	"time"

	"go2tv.app/screencast/capture"
)

// videoCloseTimeout bounds the wait on the video input. FramePacer.Close joins
// the capture backend's Close, and a backend can block there for good; the
// alternative to giving up on it is a Close that never returns.
const videoCloseTimeout = 1500 * time.Millisecond

// Config is everything a caller decides before anything is opened.
type Config struct {
	FFmpegPath   string
	IncludeAudio bool
	// StreamIndex selects the display/stream index passed to capture.Open (default 0).
	StreamIndex int
	// GOPSeconds is the keyframe interval the encoder plan is built around.
	GOPSeconds      int
	VideoQueueSize  int
	AudioQueueSize  int
	AudioChunkSize  int
	AudioRelayQueue int
	LogOutput       io.Writer
	DebugCommand    bool
	// OpenCapture replaces capture.Open, so a caller's tests can drive the
	// pipeline from a synthetic backend instead of the real screen.
	OpenCapture func(*capture.Options) (*capture.Stream, error)
}

// Prepared is a pipeline built but not started: capture is open, the encoder is
// chosen, the pacer and the audio relay are already draining the backend, and
// every ffmpeg argument but the muxer's is decided.
//
// Draining before ffmpeg exists is deliberate - see AudioPump - so a Prepared
// that is neither launched nor aborted leaks a capture session and its
// goroutines. Exactly one of Launch and Abort has to be called on it.
type Prepared struct {
	cfg   Config
	args  FFmpegArgsParams
	skew  *TimelineSkew
	debug bool

	// videoInput owns the capture backend: the stream itself until
	// NewFramePacer takes it, the pacer afterwards. Unwinding closes this and
	// nothing else, so there is exactly one owner at every point in Prepare.
	videoInput io.Closer
	// video is that same pacer as a reader, for ffmpeg's stdin. It is nil until
	// the pacer exists.
	video *FramePacer

	audioSrc io.ReadCloser
	ownAudio bool
	pump     *AudioPump
	listener net.Listener

	abortOnce sync.Once
	abortErr  error
}

// Prepare opens the capture backend and builds everything upstream of the
// muxer. On failure nothing is left running: it unwinds through the same path
// Abort uses, which is written for any prefix of what follows.
func Prepare(cfg *Config) (*Prepared, error) {
	if cfg == nil {
		return nil, errors.New("nil config")
	}

	p := &Prepared{
		cfg:   *cfg,
		skew:  &TimelineSkew{},
		debug: DebugEnabled(),
	}
	if p.debug {
		// Umbrella debug mode: emit ffmpeg stderr and print the full command.
		p.cfg.LogOutput = MergeDebugWriter(p.cfg.LogOutput)
		p.cfg.DebugCommand = true
	}

	open := p.cfg.OpenCapture
	if open == nil {
		open = capture.Open
	}
	stream, err := open(&capture.Options{
		StreamIndex:  p.cfg.StreamIndex,
		IncludeAudio: p.cfg.IncludeAudio,
	})
	if err != nil {
		return nil, fmt.Errorf("screencast open: %w", err)
	}
	p.videoInput = stream

	fps := TargetFPS(stream)
	if p.debug {
		DebugPrintf(
			"screencast/hls fps_target platform=%s width=%d height=%d source=%d target=%d",
			runtime.GOOS,
			stream.Width,
			stream.Height,
			stream.FrameRate,
			fps,
		)
	}
	fpsArg := strconv.FormatUint(uint64(fps), 10)
	gopFrames := uint64(fps) * uint64(p.cfg.GOPSeconds)
	if gopFrames == 0 {
		gopFrames = uint64(fps)
	}
	gopArg := strconv.FormatUint(gopFrames, 10)

	encoderPlan := SelectVideoEncoder(p.cfg.FFmpegPath, BaseVideoFilter(fpsArg), gopArg, p.cfg.GOPSeconds, p.cfg.LogOutput, p.debug)

	// ffmpeg derives rawvideo timestamps from the frame count and -r, so the
	// video timeline only tracks real time if we actually feed it fps frames per
	// second. Every backend is damage-driven and delivers fewer, so pace on all
	// platforms - otherwise video drifts behind the audio without bound.
	video, err := NewFramePacer(stream, fps, p.skew)
	if err != nil {
		_ = p.Abort()
		return nil, fmt.Errorf("screencast pacer: %w", err)
	}
	p.video, p.videoInput = video, video

	p.audioSrc = stream.Audio
	if p.cfg.IncludeAudio && p.audioSrc == nil {
		p.audioSrc = NewSilencePCMReader(48000, 2, 16, 20*time.Millisecond)
		p.ownAudio = true
		if p.cfg.LogOutput != nil {
			_, _ = fmt.Fprintln(p.cfg.LogOutput, "screencast audio source: synthetic_silence")
		}
		if p.debug {
			DebugPrintf("screencast/hls audio_source=synthetic_silence")
		}
	}

	audioEnabled := p.cfg.IncludeAudio && p.audioSrc != nil
	audioURL := ""
	if audioEnabled {
		p.listener, p.pump, audioURL, err = StartAudioRelay(p.audioSrc, p.cfg.AudioChunkSize, p.cfg.AudioRelayQueue, p.skew, p.debug)
		if err != nil {
			_ = p.Abort()
			return nil, fmt.Errorf("screencast audio listener: %w", err)
		}

		if p.cfg.LogOutput != nil {
			_, _ = fmt.Fprintf(p.cfg.LogOutput, "screencast audio relay: %s\n", audioURL)
		}
	}

	p.args = FFmpegArgsParams{
		Debug:          p.debug,
		EncoderPlan:    encoderPlan,
		VideoQueueSize: p.cfg.VideoQueueSize,
		AudioQueueSize: p.cfg.AudioQueueSize,
		PixelFormat:    stream.PixelFormat,
		Width:          stream.Width,
		Height:         stream.Height,
		FpsArg:         fpsArg,
		AudioEnabled:   audioEnabled,
		AudioURL:       audioURL,
	}

	return p, nil
}

// Abort releases a prepared pipeline that will not be launched. It is the same
// unwind Close performs once ffmpeg is dead, and it is the only one: every
// handle is optional, so it is correct for any prefix of Prepare as well as for
// a complete one.
//
// The order is not arbitrary. The pump closes before the capture audio source
// because closing the source is what releases a producer parked in src.Read -
// see AudioPump - and the video input's Close is bounded because it joins the
// capture backend's.
func (p *Prepared) Abort() error {
	if p == nil {
		return nil
	}

	p.abortOnce.Do(func() {
		if p.listener != nil {
			p.abortErr = errors.Join(p.abortErr, p.listener.Close())
		}

		p.pump.Close()

		if p.videoInput != nil {
			p.abortErr = errors.Join(p.abortErr, closeBounded(p.videoInput))
		}
		if p.ownAudio && p.audioSrc != nil {
			p.abortErr = errors.Join(p.abortErr, p.audioSrc.Close())
		}
	})

	return p.abortErr
}

// DroppedFrames reports how many video frames the pacer gave up on.
func (p *Prepared) DroppedFrames() int64 {
	if p == nil {
		return 0
	}
	return p.skew.DroppedFrames()
}

// closeBounded closes c, giving up on the wait - not on the close - after
// videoCloseTimeout. The Close goes on running in its goroutine; what is
// bounded is how long the caller is held by it.
func closeBounded(c io.Closer) error {
	done := make(chan error, 1)
	go func() {
		done <- c.Close()
	}()

	select {
	case err := <-done:
		return err
	case <-time.After(videoCloseTimeout):
		return nil
	}
}
