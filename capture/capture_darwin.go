//go:build darwin

package capture

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -mmacosx-version-min=12.3
#cgo LDFLAGS: -mmacosx-version-min=12.3 -framework Foundation -framework ScreenCaptureKit -framework CoreMedia -framework CoreVideo
#include "capture_darwin.h"
#include <stdlib.h>

extern void macVideoCallbackGo(int id, void* data, uint32_t size, uint32_t width, uint32_t height);
extern void macAudioCallbackGo(int id, void* data, uint32_t size);
*/
import "C"
import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
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
	videoQ     *asyncPipeWriter
	audioQ     *asyncPipeWriter
	width      uint32
	height     uint32
	ready      chan struct{}
	widthOnce  sync.Once
	heightOnce sync.Once
	videoOnce  sync.Once
	audioOnce  sync.Once
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
		captureDebugf("platform=macOS stream=%d close_begin", r.id)
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
		if ok {
			if ctxInfo.videoQ != nil {
				ctxInfo.videoQ.Close()
			}
			if ctxInfo.audioQ != nil {
				ctxInfo.audioQ.Close()
			}
		}

		r.err = errors.Join(vErr, aErr)
		captureDebugf("platform=macOS stream=%d close_done err=%v", r.id, r.err)
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
		close(ctxInfo.ready)
	})
	ctxInfo.videoOnce.Do(func() {
		captureDebugf("platform=macOS stream=%d first_video_frame bytes=%d width=%d height=%d", int(id), int(size), int(width), int(height))
	})

	if ctxInfo.videoQ == nil || size == 0 {
		return
	}
	b := make([]byte, int(size))
	copy(b, unsafe.Slice((*byte)(data), int(size)))
	ctxInfo.videoQ.Enqueue(b)
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
	pcm := convertPlanarFloat32StereoToS16(byteSlice)
	if len(pcm) == 0 {
		return
	}
	ctxInfo.audioOnce.Do(func() {
		captureDebugf("platform=macOS stream=%d first_audio_chunk in_bytes=%d out_bytes=%d", int(id), int(size), len(pcm))
	})
	if ctxInfo.audioQ == nil {
		return
	}
	ctxInfo.audioQ.Enqueue(pcm)
}

// macOS implementation target: ScreenCaptureKit (SCShareableContent + SCStream).
func open(options *Options) (*Stream, error) {
	var err error
	options, err = validateOpenOptions(options)
	if err != nil {
		return nil, err
	}
	captureDebugf(
		"platform=macOS open_start stream_index=%d include_audio=%t video_queue=%d audio_queue=%d",
		options.StreamIndex,
		options.IncludeAudio,
		defaultCallbackVideoQueue,
		defaultCallbackAudioQueue,
	)

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
		vpw:   vpw,
		apw:   apw,
		ready: make(chan struct{}),
	}
	ctxInfo.videoQ = newAsyncPipeWriter("macOS", id, "video", vpw, defaultCallbackVideoQueue)
	if apw != nil {
		ctxInfo.audioQ = newAsyncPipeWriter("macOS", id, "audio", apw, defaultCallbackAudioQueue)
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
		if ctxInfo.videoQ != nil {
			ctxInfo.videoQ.Close()
		}
		if ctxInfo.audioQ != nil {
			ctxInfo.audioQ.Close()
		}
		_ = vpw.Close()
		_ = vpr.Close()
		if apw != nil {
			_ = apw.Close()
		}
		if apr != nil {
			_ = apr.Close()
		}
		captureDebugf("platform=macOS stream=%d init_failed stream_index=%d include_audio=%t", id, options.StreamIndex, options.IncludeAudio)
		return nil, fmt.Errorf("failed to initialize Mac ScreenCaptureKit session")
	}

	ctxInfo.ctx = ctx
	C.StartMacCapture(ctx)
	captureDebugf("platform=macOS stream=%d capture_started", id)

	reader := &darwinReadCloser{
		id:  id,
		vpr: vpr,
		vpw: vpw,
		apr: apr,
		apw: apw,
	}

	if err := waitForFirstFrame("macOS", ctxInfo.ready, reader.Close); err != nil {
		captureDebugf("platform=macOS stream=%d open_failed err=%v", id, err)
		return nil, err
	}
	captureDebugf(
		"platform=macOS stream=%d open_ready width=%d height=%d fps=%d include_audio=%t",
		id,
		ctxInfo.width,
		ctxInfo.height,
		60,
		apr != nil,
	)

	return &Stream{
		ReadCloser:  reader,
		Audio:       pipeReaderAsReadCloser(apr),
		Width:       ctxInfo.width, // Set asynchronously by callback
		Height:      ctxInfo.height,
		FrameRate:   60,
		PixelFormat: PixelFormatBGRA,
	}, nil
}

func convertPlanarFloat32StereoToS16(planar []byte) []byte {
	// ScreenCaptureKit audio is float32 planar; convert to packed s16le stereo.
	if len(planar) < 8 {
		return nil
	}

	frames := len(planar) / 8
	if frames == 0 {
		return nil
	}

	planeBytes := frames * 4
	if planeBytes*2 > len(planar) {
		return nil
	}

	left := planar[:planeBytes]
	right := planar[planeBytes : planeBytes*2]
	out := make([]byte, frames*4)

	for i := 0; i < frames; i++ {
		li := i * 4
		l := math.Float32frombits(binary.LittleEndian.Uint32(left[li : li+4]))
		r := math.Float32frombits(binary.LittleEndian.Uint32(right[li : li+4]))

		l16 := float32ToPCM16(l)
		r16 := float32ToPCM16(r)
		oi := i * 4
		binary.LittleEndian.PutUint16(out[oi:oi+2], uint16(l16))
		binary.LittleEndian.PutUint16(out[oi+2:oi+4], uint16(r16))
	}

	return out
}

func float32ToPCM16(v float32) int16 {
	if math.IsNaN(float64(v)) {
		return 0
	}
	if v >= 1 {
		return 32767
	}
	if v <= -1 {
		return -32768
	}

	scaled := int32(math.Round(float64(v * 32767)))
	if scaled > 32767 {
		scaled = 32767
	}
	if scaled < -32768 {
		scaled = -32768
	}
	return int16(scaled)
}
