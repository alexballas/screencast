//go:build windows

package capture

/*
#cgo CXXFLAGS: -std=gnu++20
#cgo LDFLAGS: -lwindowsapp -ld3d11 -lole32 -static-libstdc++ -static-libgcc -Wl,-Bstatic -l:libstdc++.a -l:libwinpthread.a
#include "capture_windows.h"
#include <stdlib.h>

extern void winVideoCallbackGo(int id, void* data, uint32_t size, uint32_t width, uint32_t height);
extern void winAudioCallbackGo(int id, void* data, uint32_t size);
*/
import "C"
import (
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

var (
	winStreamsMu sync.Mutex
	winStreams   = make(map[int]*winStreamContext)
	winNextID    = 1
)

type winStreamContext struct {
	ctx        unsafe.Pointer
	vpw        *io.PipeWriter
	apw        *io.PipeWriter
	width      uint32
	height     uint32
	ready      chan struct{}
	widthOnce  sync.Once
	heightOnce sync.Once
	videoOnce  sync.Once
	audioOnce  sync.Once
	lastVSlow  atomic.Int64
	lastASlow  atomic.Int64
}

type windowsReadCloser struct {
	id  int
	vpr *io.PipeReader
	vpw *io.PipeWriter
	apr *io.PipeReader
	apw *io.PipeWriter

	closeOnce sync.Once
	err       error
}

func (r *windowsReadCloser) Read(p []byte) (int, error) {
	return r.vpr.Read(p)
}

func (r *windowsReadCloser) Close() error {
	r.closeOnce.Do(func() {
		captureDebugf("platform=windows stream=%d close_begin", r.id)
		winStreamsMu.Lock()
		ctxInfo, ok := winStreams[r.id]
		if ok {
			delete(winStreams, r.id)
		}
		winStreamsMu.Unlock()

		vErr := r.vpw.Close()
		var aErr error
		if r.apw != nil {
			aErr = r.apw.Close()
		}

		if ok && ctxInfo.ctx != nil {
			C.StopWinCapture(ctxInfo.ctx)
			C.FreeWinCapture(ctxInfo.ctx)
		}

		r.vpr.Close()
		if r.apr != nil {
			r.apr.Close()
		}

		r.err = errors.Join(vErr, aErr)
		captureDebugf("platform=windows stream=%d close_done err=%v", r.id, r.err)
	})

	return r.err
}

//export winVideoCallbackGo
func winVideoCallbackGo(id C.int, data unsafe.Pointer, size C.uint32_t, width C.uint32_t, height C.uint32_t) {
	winStreamsMu.Lock()
	ctxInfo, ok := winStreams[int(id)]
	winStreamsMu.Unlock()

	if !ok {
		return
	}

	ctxInfo.widthOnce.Do(func() {
		ctxInfo.width = uint32(width)
	})
	ctxInfo.heightOnce.Do(func() {
		ctxInfo.height = uint32(height)
		close(ctxInfo.ready)
	})
	ctxInfo.videoOnce.Do(func() {
		captureDebugf("platform=windows stream=%d first_video_frame bytes=%d width=%d height=%d", int(id), int(size), int(width), int(height))
	})

	byteSlice := unsafe.Slice((*byte)(data), int(size))
	start := time.Now()
	_, err := ctxInfo.vpw.Write(byteSlice)
	if err != nil {
		captureDebugf("platform=windows stream=%d video_write_err=%v", int(id), err)
		return
	}
	d := time.Since(start)
	if d > 50*time.Millisecond && captureShouldLogSlowWrite(&ctxInfo.lastVSlow, time.Second) {
		captureDebugf("platform=windows stream=%d slow_video_write duration=%s bytes=%d", int(id), d, int(size))
	}
}

//export winAudioCallbackGo
func winAudioCallbackGo(id C.int, data unsafe.Pointer, size C.uint32_t) {
	winStreamsMu.Lock()
	ctxInfo, ok := winStreams[int(id)]
	winStreamsMu.Unlock()

	if !ok || ctxInfo.apw == nil {
		return
	}
	ctxInfo.audioOnce.Do(func() {
		captureDebugf("platform=windows stream=%d first_audio_chunk bytes=%d", int(id), int(size))
	})

	byteSlice := unsafe.Slice((*byte)(data), int(size))
	start := time.Now()
	_, err := ctxInfo.apw.Write(byteSlice)
	if err != nil {
		captureDebugf("platform=windows stream=%d audio_write_err=%v", int(id), err)
		return
	}
	d := time.Since(start)
	if d > 50*time.Millisecond && captureShouldLogSlowWrite(&ctxInfo.lastASlow, time.Second) {
		captureDebugf("platform=windows stream=%d slow_audio_write duration=%s bytes=%d", int(id), d, int(size))
	}
}

// Windows implementation target: Windows Graphics Capture (WinRT GraphicsCaptureItem).
func open(options *Options) (*Stream, error) {
	var err error
	options, err = validateOpenOptions(options)
	if err != nil {
		return nil, err
	}
	captureDebugf("platform=windows open_start stream_index=%d include_audio=%t", options.StreamIndex, options.IncludeAudio)

	vpr, vpw := io.Pipe()
	var apr *io.PipeReader
	var apw *io.PipeWriter

	if options.IncludeAudio {
		apr, apw = io.Pipe()
	}

	winStreamsMu.Lock()
	id := winNextID
	winNextID++

	ctxInfo := &winStreamContext{
		vpw:   vpw,
		apw:   apw,
		ready: make(chan struct{}),
	}
	winStreams[id] = ctxInfo
	winStreamsMu.Unlock()

	vcb := (C.WinVideoFrameCallback)(C.winVideoCallbackGo)
	var acb C.WinAudioFrameCallback
	if options.IncludeAudio {
		acb = (C.WinAudioFrameCallback)(C.winAudioCallbackGo)
	}

	ctx := C.InitWinCapture(C.int(id), C.int(options.StreamIndex), C.bool(options.IncludeAudio), vcb, acb)
	if ctx == nil && options.IncludeAudio {
		captureDebugf("platform=windows stream=%d init_with_audio_failed retrying_video_only", id)
		// If audio initialization fails, retry video-only to keep capture functional.
		if apw != nil {
			_ = apw.Close()
			apw = nil
			ctxInfo.apw = nil
		}
		if apr != nil {
			_ = apr.Close()
			apr = nil
		}
		ctx = C.InitWinCapture(C.int(id), C.int(options.StreamIndex), C.bool(false), vcb, nil)
	}
	if ctx == nil {
		winStreamsMu.Lock()
		delete(winStreams, id)
		winStreamsMu.Unlock()
		captureDebugf("platform=windows stream=%d init_failed stream_index=%d include_audio=%t", id, options.StreamIndex, options.IncludeAudio)
		return nil, fmt.Errorf("failed to initialize Windows Graphics Capture session")
	}

	ctxInfo.ctx = ctx
	C.StartWinCapture(ctx)
	captureDebugf("platform=windows stream=%d capture_started", id)

	reader := &windowsReadCloser{
		id:  id,
		vpr: vpr,
		vpw: vpw,
		apr: apr,
		apw: apw,
	}

	if err := waitForFirstFrame("windows", ctxInfo.ready, reader.Close); err != nil {
		captureDebugf("platform=windows stream=%d open_failed err=%v", id, err)
		return nil, err
	}
	captureDebugf(
		"platform=windows stream=%d open_ready width=%d height=%d fps=%d include_audio=%t",
		id,
		ctxInfo.width,
		ctxInfo.height,
		60,
		apr != nil,
	)

	return &Stream{
		ReadCloser:  reader,
		Audio:       pipeReaderAsReadCloser(apr),
		Width:       ctxInfo.width,
		Height:      ctxInfo.height,
		FrameRate:   60,
		PixelFormat: PixelFormatBGRA,
	}, nil
}
