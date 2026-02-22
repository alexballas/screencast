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
		captureDebugf("platform=linux close_begin")
		streamErr := r.stream.Close()
		var audioErr error
		if r.audio != nil {
			audioErr = r.audio.Close()
		}
		sessErr := r.sess.Close()
		r.err = errors.Join(streamErr, audioErr, sessErr)
		captureDebugf("platform=linux close_done err=%v", r.err)
	})

	return r.err
}

func open(options *Options) (*Stream, error) {
	var err error
	options, err = validateOpenOptions(options)
	if err != nil {
		return nil, err
	}
	captureDebugf("platform=linux open_start stream_index=%d include_audio=%t", options.StreamIndex, options.IncludeAudio)

	if !pipewire.IsAvailable() {
		captureDebugf("platform=linux open_failed reason=pipewire_not_loaded")
		return nil, pipewire.ErrLibraryNotLoaded
	}

	sess, err := screencast.CreateSession(nil)
	if err != nil {
		captureDebugf("platform=linux create_session_failed err=%v", err)
		return nil, err
	}
	if sess == nil {
		captureDebugf("platform=linux open_cancelled stage=create_session")
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
		captureDebugf("platform=linux select_sources_failed err=%v", err)
		return nil, err
	}

	streams, err := sess.Start("", nil)
	if err != nil {
		captureDebugf("platform=linux start_failed err=%v", err)
		return nil, err
	}
	if streams == nil {
		captureDebugf("platform=linux open_cancelled stage=start")
		return nil, ErrCancelled
	}
	if len(streams) == 0 {
		captureDebugf("platform=linux open_failed reason=no_streams")
		return nil, ErrNoStreams
	}
	if options.StreamIndex >= len(streams) {
		captureDebugf("platform=linux open_failed reason=stream_index_out_of_range stream_index=%d streams=%d", options.StreamIndex, len(streams))
		return nil, fmt.Errorf("%w: StreamIndex %d out of range (streams=%d)", ErrInvalidOptions, options.StreamIndex, len(streams))
	}

	selected := streams[options.StreamIndex]
	if selected.Size[0] <= 0 || selected.Size[1] <= 0 {
		captureDebugf("platform=linux open_failed reason=invalid_stream_size width=%d height=%d", selected.Size[0], selected.Size[1])
		return nil, fmt.Errorf("invalid stream size %dx%d", selected.Size[0], selected.Size[1])
	}

	fd, err := sess.OpenPipeWireRemote(nil)
	if err != nil {
		captureDebugf("platform=linux open_pipewire_remote_failed err=%v", err)
		return nil, err
	}
	defer syscall.Close(fd)

	pwStream, err := pipewire.NewStream(fd, selected.NodeID, uint32(selected.Size[0]), uint32(selected.Size[1]))
	if err != nil {
		captureDebugf("platform=linux pipewire_new_video_stream_failed err=%v", err)
		return nil, err
	}
	pwStream.Start()
	frameRate := pwStream.WaitFrameRate(1200 * time.Millisecond)
	if frameRate == 0 {
		frameRate = defaultLinuxFrameRate
	}
	captureDebugf(
		"platform=linux video_ready node_id=%d width=%d height=%d fps=%d",
		selected.NodeID,
		selected.Size[0],
		selected.Size[1],
		frameRate,
	)

	var (
		audioReader io.ReadCloser
		audioStream *pipewire.Stream
	)
	if options.IncludeAudio {
		audioStream, err = pipewire.NewAudioStream()
		if err != nil {
			captureDebugf("platform=linux pipewire_new_audio_stream_failed err=%v", err)
			pwStream.Close()
			return nil, err
		}
		audioStream.Start()
		audioReader = audioStream
		captureDebugf("platform=linux audio_ready")
	}

	reader := &linuxReadCloser{
		stream: pwStream,
		audio:  audioStream,
		sess:   sess,
	}

	cleanupSession = false
	captureDebugf(
		"platform=linux open_ready width=%d height=%d fps=%d include_audio=%t",
		selected.Size[0],
		selected.Size[1],
		frameRate,
		options.IncludeAudio,
	)
	return &Stream{
		ReadCloser:  reader,
		Audio:       audioReader,
		Width:       uint32(selected.Size[0]),
		Height:      uint32(selected.Size[1]),
		FrameRate:   frameRate,
		PixelFormat: PixelFormatBGRA,
	}, nil
}
