//go:build linux

package capture

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"syscall"
	"time"

	"go2tv.app/screencast/internal/pipewire"
	"go2tv.app/screencast/screencast"
)

const defaultLinuxFrameRate = 60

type linuxReadCloser struct {
	stream *pipewire.Stream
	audio  *pipewire.Stream
	sess   *screencast.Session

	once sync.Once
	err  error
}

func (r *linuxReadCloser) Read(p []byte) (int, error) {
	return r.stream.Read(p)
}

func (r *linuxReadCloser) Close() error {
	r.once.Do(func() {
		streamErr := r.stream.Close()
		var audioErr error
		if r.audio != nil {
			audioErr = r.audio.Close()
		}
		sessErr := r.sess.Close()
		r.err = errors.Join(streamErr, audioErr, sessErr)
	})

	return r.err
}

func open(options *Options) (*Stream, error) {
	if options == nil {
		options = &Options{}
	}
	if options.StreamIndex < 0 {
		return nil, fmt.Errorf("%w: StreamIndex must be >= 0", ErrInvalidOptions)
	}

	if !pipewire.IsAvailable() {
		return nil, pipewire.ErrLibraryNotLoaded
	}

	sess, err := screencast.CreateSession(nil)
	if err != nil {
		return nil, err
	}
	if sess == nil {
		return nil, ErrCancelled
	}

	// Close session on setup failure.
	cleanupSession := true
	defer func() {
		if cleanupSession {
			_ = sess.Close()
		}
	}()

	err = sess.SelectSources(&screencast.SelectSourcesOptions{
		Types:      screencast.SourceTypeMonitor | screencast.SourceTypeWindow,
		CursorMode: screencast.CursorModeEmbedded,
		Multiple:   true,
	})
	if err != nil {
		return nil, err
	}

	streams, err := sess.Start("", nil)
	if err != nil {
		return nil, err
	}
	if streams == nil {
		return nil, ErrCancelled
	}
	if len(streams) == 0 {
		return nil, ErrNoStreams
	}
	if options.StreamIndex >= len(streams) {
		return nil, fmt.Errorf("%w: StreamIndex %d out of range (streams=%d)", ErrInvalidOptions, options.StreamIndex, len(streams))
	}

	selected := streams[options.StreamIndex]
	if selected.Size[0] <= 0 || selected.Size[1] <= 0 {
		return nil, fmt.Errorf("invalid stream size %dx%d", selected.Size[0], selected.Size[1])
	}

	fd, err := sess.OpenPipeWireRemote(nil)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(fd)

	pwStream, err := pipewire.NewStream(fd, selected.NodeID, uint32(selected.Size[0]), uint32(selected.Size[1]))
	if err != nil {
		return nil, err
	}
	pwStream.Start()
	frameRate := pwStream.WaitFrameRate(1200 * time.Millisecond)
	if frameRate == 0 {
		frameRate = defaultLinuxFrameRate
	}

	var (
		audioReader io.ReadCloser
		audioStream *pipewire.Stream
	)
	if options.IncludeAudio {
		audioStream, err = pipewire.NewAudioStream()
		if err != nil {
			pwStream.Close()
			return nil, err
		}
		audioStream.Start()
		audioReader = audioStream
	}

	reader := &linuxReadCloser{
		stream: pwStream,
		audio:  audioStream,
		sess:   sess,
	}

	cleanupSession = false
	return &Stream{
		ReadCloser:  reader,
		Audio:       audioReader,
		Width:       uint32(selected.Size[0]),
		Height:      uint32(selected.Size[1]),
		FrameRate:   frameRate,
		PixelFormat: PixelFormatBGRA,
	}, nil
}
