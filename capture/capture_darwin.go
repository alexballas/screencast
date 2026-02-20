//go:build darwin

package capture

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Foundation -framework ScreenCaptureKit -framework CoreMedia -framework CoreVideo
#include "capture_darwin.h"
#include <stdlib.h>

extern void macVideoCallbackGo(int id, void* data, uint32_t size, uint32_t width, uint32_t height);
extern void macAudioCallbackGo(int id, void* data, uint32_t size);
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
	macStreamsMu sync.Mutex
	macStreams   = make(map[int]*macStreamContext)
	macNextID    = 1
)

type macStreamContext struct {
	ctx        unsafe.Pointer
	vpw        *io.PipeWriter
	apw        *io.PipeWriter
	id         int
	width      uint32
	height     uint32
	widthOnce  sync.Once
	heightOnce sync.Once
}

type darwinReadCloser struct {
	id  int
	vpr *io.PipeReader
	vpw *io.PipeWriter
	apr *io.PipeReader
	apw *io.PipeWriter

	closeOnce sync.Once
	err       error
}

func (r *darwinReadCloser) Read(p []byte) (int, error) {
	return r.vpr.Read(p)
}

func (r *darwinReadCloser) Close() error {
	r.closeOnce.Do(func() {
		macStreamsMu.Lock()
		ctxInfo, ok := macStreams[r.id]
		if ok {
			delete(macStreams, r.id)
		}
		macStreamsMu.Unlock()

		if ok && ctxInfo.ctx != nil {
			C.StopMacCapture(ctxInfo.ctx)
			C.FreeMacCapture(ctxInfo.ctx)
		}

		vErr := r.vpw.Close()
		r.vpr.Close()

		var aErr error
		if r.apw != nil {
			aErr = r.apw.Close()
			r.apr.Close()
		}

		r.err = errors.Join(vErr, aErr)
	})

	return r.err
}

//export macVideoCallbackGo
func macVideoCallbackGo(id C.int, data unsafe.Pointer, size C.uint32_t, width C.uint32_t, height C.uint32_t) {
	macStreamsMu.Lock()
	ctxInfo, ok := macStreams[int(id)]
	macStreamsMu.Unlock()

	if !ok {
		return
	}

	ctxInfo.widthOnce.Do(func() {
		ctxInfo.width = uint32(width)
	})
	ctxInfo.heightOnce.Do(func() {
		ctxInfo.height = uint32(height)
	})

	byteSlice := unsafe.Slice((*byte)(data), int(size))
	ctxInfo.vpw.Write(byteSlice)
}

//export macAudioCallbackGo
func macAudioCallbackGo(id C.int, data unsafe.Pointer, size C.uint32_t) {
	macStreamsMu.Lock()
	ctxInfo, ok := macStreams[int(id)]
	macStreamsMu.Unlock()

	if !ok || ctxInfo.apw == nil {
		return
	}

	byteSlice := unsafe.Slice((*byte)(data), int(size))
	ctxInfo.apw.Write(byteSlice)
}

// macOS implementation target: ScreenCaptureKit (SCShareableContent + SCStream).
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

	macStreamsMu.Lock()
	id := macNextID
	macNextID++

	ctxInfo := &macStreamContext{
		id:  id,
		vpw: vpw,
		apw: apw,
	}
	macStreams[id] = ctxInfo
	macStreamsMu.Unlock()

	vcb := (C.VideoFrameCallback)(C.macVideoCallbackGo)
	var acb C.AudioFrameCallback
	if options.IncludeAudio {
		acb = (C.AudioFrameCallback)(C.macAudioCallbackGo)
	}

	ctx := C.InitMacCapture(C.int(id), C.int(options.StreamIndex), C.bool(options.IncludeAudio), vcb, acb)
	if ctx == nil {
		macStreamsMu.Lock()
		delete(macStreams, id)
		macStreamsMu.Unlock()
		return nil, fmt.Errorf("failed to initialize Mac ScreenCaptureKit session")
	}

	ctxInfo.ctx = ctx
	C.StartMacCapture(ctx)

	reader := &darwinReadCloser{
		id:  id,
		vpr: vpr,
		vpw: vpw,
		apr: apr,
		apw: apw,
	}

	var audioReader io.ReadCloser
	if options.IncludeAudio {
		audioReader = struct {
			io.Reader
			io.Closer
		}{apr, apr}
	}

	return &Stream{
		ReadCloser:  reader,
		Audio:       audioReader,
		Width:       0, // Width/height available after first frame in ctxInfo
		Height:      0,
		FrameRate:   60,
		PixelFormat: PixelFormatBGRA,
	}, nil
}
