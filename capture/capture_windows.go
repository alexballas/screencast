//go:build windows

package capture

/*
#cgo CXXFLAGS: -std=gnu++20
#cgo LDFLAGS: -lwindowsapp -ld3d11 -lole32 -static-libstdc++ -static-libgcc -Wl,-Bstatic -lstdc++ -lpthread -Wl,-Bdynamic
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
	id         int
	width      uint32
	height     uint32
	ready      chan struct{}
	widthOnce  sync.Once
	heightOnce sync.Once
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

	byteSlice := unsafe.Slice((*byte)(data), int(size))
	ctxInfo.vpw.Write(byteSlice)
}

//export winAudioCallbackGo
func winAudioCallbackGo(id C.int, data unsafe.Pointer, size C.uint32_t) {
	winStreamsMu.Lock()
	ctxInfo, ok := winStreams[int(id)]
	winStreamsMu.Unlock()

	if !ok || ctxInfo.apw == nil {
		return
	}

	byteSlice := unsafe.Slice((*byte)(data), int(size))
	ctxInfo.apw.Write(byteSlice)
}

// Windows implementation target: Windows Graphics Capture (WinRT GraphicsCaptureItem).
func open(options *Options) (*Stream, error) {
	if options == nil {
		options = &Options{}
	}
	if options.StreamIndex < 0 {
		return nil, fmt.Errorf("%w: StreamIndex must be >= 0", ErrInvalidOptions)
	}

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
		id:    id,
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
		return nil, fmt.Errorf("failed to initialize Windows Graphics Capture session")
	}

	ctxInfo.ctx = ctx
	C.StartWinCapture(ctx)

	// Wait for the first frame to populate real width/height
	<-ctxInfo.ready

	reader := &windowsReadCloser{
		id:  id,
		vpr: vpr,
		vpw: vpw,
		apr: apr,
		apw: apw,
	}

	var audioReader io.ReadCloser
	if apr != nil {
		audioReader = struct {
			io.Reader
			io.Closer
		}{apr, apr}
	}

	return &Stream{
		ReadCloser:  reader,
		Audio:       audioReader,
		Width:       ctxInfo.width,
		Height:      ctxInfo.height,
		FrameRate:   60,
		PixelFormat: PixelFormatBGRA,
	}, nil
}
