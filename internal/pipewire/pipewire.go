//go:build linux

package pipewire

/*
#cgo pkg-config: libpipewire-0.3
#cgo LDFLAGS: -ldl
#include <pipewire/pipewire.h>
#include <spa/param/video/format-utils.h>
#include <spa/param/audio/format-utils.h>
#include <spa/buffer/meta.h>
#include <stdlib.h>
#include <string.h>
#include <dlfcn.h>
#include <stdio.h>

// Function pointers for dynamic loading
static void (*d_pw_init)(int *argc, char **argv[]);
static struct pw_main_loop * (*d_pw_main_loop_new)(const struct spa_dict *props);
static struct pw_loop * (*d_pw_main_loop_get_loop)(struct pw_main_loop *loop);
static void (*d_pw_main_loop_quit)(struct pw_main_loop *loop);
static void (*d_pw_main_loop_run)(struct pw_main_loop *loop);
static void (*d_pw_main_loop_destroy)(struct pw_main_loop *loop);
static struct pw_context * (*d_pw_context_new)(struct pw_loop *main_loop, struct pw_properties *props, size_t user_data_size);
static void (*d_pw_context_destroy)(struct pw_context *context);
static struct pw_core * (*d_pw_context_connect_fd)(struct pw_context *context, int fd, struct pw_properties *properties, size_t user_data_size);
static struct pw_core * (*d_pw_context_connect)(struct pw_context *context, struct pw_properties *properties, size_t user_data_size);
static int (*d_pw_core_disconnect)(struct pw_core *core);
static struct pw_properties * (*d_pw_properties_new)(const char *key, ...);
static struct pw_stream * (*d_pw_stream_new)(struct pw_core *core, const char *name, struct pw_properties *props);
static void (*d_pw_stream_add_listener)(struct pw_stream *stream, struct spa_hook *listener, const struct pw_stream_events *events, void *data);
static int (*d_pw_stream_connect)(struct pw_stream *stream, enum pw_direction direction, uint32_t target_id, enum pw_stream_flags flags, const struct spa_pod **params, uint32_t n_params);
static struct pw_buffer * (*d_pw_stream_dequeue_buffer)(struct pw_stream *stream);
static int (*d_pw_stream_queue_buffer)(struct pw_stream *stream, struct pw_buffer *buffer);
static void (*d_pw_stream_destroy)(struct pw_stream *stream);

static void* pw_lib_handle = NULL;

static int load_pipewire() {
    if (pw_lib_handle != NULL) return 1;

    const char* lib_names[] = {
        "libpipewire-0.3.so.0",
        "libpipewire-0.3.so",
        NULL
    };

    for (int i = 0; lib_names[i] != NULL; i++) {
        pw_lib_handle = dlopen(lib_names[i], RTLD_NOW);
        if (pw_lib_handle) break;
    }

    if (!pw_lib_handle) return 0;

    d_pw_init = dlsym(pw_lib_handle, "pw_init");
    d_pw_main_loop_new = dlsym(pw_lib_handle, "pw_main_loop_new");
    d_pw_main_loop_get_loop = dlsym(pw_lib_handle, "pw_main_loop_get_loop");
    d_pw_main_loop_quit = dlsym(pw_lib_handle, "pw_main_loop_quit");
    d_pw_main_loop_run = dlsym(pw_lib_handle, "pw_main_loop_run");
    d_pw_main_loop_destroy = dlsym(pw_lib_handle, "pw_main_loop_destroy");
    d_pw_context_new = dlsym(pw_lib_handle, "pw_context_new");
    d_pw_context_destroy = dlsym(pw_lib_handle, "pw_context_destroy");
    d_pw_context_connect_fd = dlsym(pw_lib_handle, "pw_context_connect_fd");
    d_pw_context_connect = dlsym(pw_lib_handle, "pw_context_connect");
    d_pw_core_disconnect = dlsym(pw_lib_handle, "pw_core_disconnect");
    d_pw_properties_new = dlsym(pw_lib_handle, "pw_properties_new");
    d_pw_stream_new = dlsym(pw_lib_handle, "pw_stream_new");
    d_pw_stream_add_listener = dlsym(pw_lib_handle, "pw_stream_add_listener");
    d_pw_stream_connect = dlsym(pw_lib_handle, "pw_stream_connect");
    d_pw_stream_dequeue_buffer = dlsym(pw_lib_handle, "pw_stream_dequeue_buffer");
    d_pw_stream_queue_buffer = dlsym(pw_lib_handle, "pw_stream_queue_buffer");
    d_pw_stream_destroy = dlsym(pw_lib_handle, "pw_stream_destroy");

    if (!d_pw_init || !d_pw_main_loop_new || !d_pw_stream_new) {
        dlclose(pw_lib_handle);
        pw_lib_handle = NULL;
        return 0;
    }

    return 1;
}

extern void on_state_changed_go(int id, enum pw_stream_state old, enum pw_stream_state state, char *error);
extern void on_frame_go(int id, void *data, uint32_t size);
extern void on_param_changed_go(int id, uint32_t framerate_num, uint32_t framerate_den);

struct go_stream_data {
    int id;
    struct pw_stream *stream;
    struct spa_hook stream_listener;
};

static void on_state_changed_c(void *userdata, enum pw_stream_state old, enum pw_stream_state state, const char *error) {
    struct go_stream_data *data = userdata;
    on_state_changed_go(data->id, old, state, (char*)error);
}

static void on_param_changed_c(void *userdata, uint32_t id, const struct spa_pod *param) {
    struct go_stream_data *data = userdata;
    if (data == NULL || param == NULL || id != SPA_PARAM_Format) {
        return;
    }

    uint32_t media_type = 0;
    uint32_t media_subtype = 0;
    if (spa_format_parse(param, &media_type, &media_subtype) < 0) {
        return;
    }
    if (media_type != SPA_MEDIA_TYPE_video || media_subtype != SPA_MEDIA_SUBTYPE_raw) {
        return;
    }

    struct spa_video_info_raw raw;
    memset(&raw, 0, sizeof(raw));
    if (spa_format_video_raw_parse(param, &raw) < 0) {
        return;
    }

    on_param_changed_go(data->id, raw.framerate.num, raw.framerate.denom);
}

static void on_process_c(void *userdata) {
    struct go_stream_data *data = userdata;
    if (!data->stream) return;

    // OBS-style: dequeue all pending buffers, keep only newest.
    struct pw_buffer *latest = NULL;
    while (true) {
        struct pw_buffer *b = d_pw_stream_dequeue_buffer(data->stream);
        if (b == NULL) {
            break;
        }
        if (latest != NULL) {
            d_pw_stream_queue_buffer(data->stream, latest);
        }
        latest = b;
    }

    if (latest == NULL) {
        return;
    }

    struct spa_buffer *buf = latest->buffer;
    if (buf == NULL || buf->n_datas == 0) {
        d_pw_stream_queue_buffer(data->stream, latest);
        return;
    }

    struct spa_data *d0 = &buf->datas[0];
    if (d0->data == NULL || d0->chunk == NULL) {
        d_pw_stream_queue_buffer(data->stream, latest);
        return;
    }

    struct spa_meta_header *header = spa_buffer_find_meta_data(buf, SPA_META_Header, sizeof(*header));
    if (header != NULL && (header->flags & SPA_META_HEADER_FLAG_CORRUPTED) > 0) {
        d_pw_stream_queue_buffer(data->stream, latest);
        return;
    }
    if ((d0->chunk->flags & SPA_CHUNK_FLAG_CORRUPTED) > 0) {
        d_pw_stream_queue_buffer(data->stream, latest);
        return;
    }

    uint32_t size = d0->chunk->size;
    uint32_t offset = d0->chunk->offset;
    if (size == 0 || offset >= d0->maxsize) {
        d_pw_stream_queue_buffer(data->stream, latest);
        return;
    }
    if (size > d0->maxsize - offset) {
        d_pw_stream_queue_buffer(data->stream, latest);
        return;
    }

    uint8_t *frame = SPA_PTROFF(d0->data, offset, uint8_t);
    on_frame_go(data->id, frame, size);

    d_pw_stream_queue_buffer(data->stream, latest);
}

static const struct pw_stream_events stream_events = {
    PW_VERSION_STREAM_EVENTS,
    .state_changed = on_state_changed_c,
    .param_changed = on_param_changed_c,
    .process = on_process_c,
};

static inline struct pw_stream * create_stream(struct pw_core *core, const char *name, struct go_stream_data *data) {
    struct pw_properties *props = d_pw_properties_new(
                PW_KEY_MEDIA_TYPE, "Video",
                PW_KEY_MEDIA_CATEGORY, "Capture",
                PW_KEY_MEDIA_ROLE, "Screen",
                NULL);

    struct pw_stream *stream = d_pw_stream_new(core, name, props);
    if (stream != NULL) {
        data->stream = stream;
        d_pw_stream_add_listener(stream, &data->stream_listener, &stream_events, data);
    }
    return stream;
}

static inline int connect_stream(struct pw_stream *stream, uint32_t target_id, uint32_t width, uint32_t height, uint32_t framerate_num, uint32_t framerate_den) {
    uint8_t buffer[1024];
    struct spa_pod_builder b = SPA_POD_BUILDER_INIT(buffer, sizeof(buffer));

    const struct spa_pod *params[1];
    params[0] = spa_pod_builder_add_object(&b,
        SPA_TYPE_OBJECT_Format, SPA_PARAM_EnumFormat,
        SPA_FORMAT_mediaType, SPA_POD_Id(SPA_MEDIA_TYPE_video),
        SPA_FORMAT_mediaSubtype, SPA_POD_Id(SPA_MEDIA_SUBTYPE_raw),
        SPA_FORMAT_VIDEO_format, SPA_POD_CHOICE_ENUM_Id(7,
            SPA_VIDEO_FORMAT_BGRx,
            SPA_VIDEO_FORMAT_BGRx,
            SPA_VIDEO_FORMAT_RGBx,
            SPA_VIDEO_FORMAT_RGBA,
            SPA_VIDEO_FORMAT_BGRA,
            SPA_VIDEO_FORMAT_RGB,
            SPA_VIDEO_FORMAT_BGR),
        SPA_FORMAT_VIDEO_size, SPA_POD_CHOICE_RANGE_Rectangle(
            &SPA_RECTANGLE(width, height),
            &SPA_RECTANGLE(1, 1),
            &SPA_RECTANGLE(8192, 8192)),
        SPA_FORMAT_VIDEO_framerate, SPA_POD_CHOICE_RANGE_Fraction(
            &SPA_FRACTION(framerate_num, framerate_den),
            &SPA_FRACTION(0, 1),
            &SPA_FRACTION(1000, 1)));

    return d_pw_stream_connect(stream,
        PW_DIRECTION_INPUT,
        target_id,
        PW_STREAM_FLAG_AUTOCONNECT |
        PW_STREAM_FLAG_MAP_BUFFERS,
        params, 1);
}

// Accessors for Go
static inline void wrap_pw_init() { d_pw_init(NULL, NULL); }
static inline struct pw_main_loop * wrap_pw_main_loop_new() { return d_pw_main_loop_new(NULL); }
static inline struct pw_context * wrap_pw_context_new(struct pw_main_loop *loop) { return d_pw_context_new(d_pw_main_loop_get_loop(loop), NULL, 0); }
static inline struct pw_core * wrap_pw_context_connect_fd(struct pw_context *context, int fd) { return d_pw_context_connect_fd(context, fd, NULL, 0); }
static inline struct pw_core * wrap_pw_context_connect(struct pw_context *context) { return d_pw_context_connect(context, NULL, 0); }

static inline struct pw_stream * create_audio_stream(struct pw_core *core, const char *name, struct go_stream_data *data) {
    struct pw_properties *props = d_pw_properties_new(
                PW_KEY_MEDIA_TYPE, "Audio",
                PW_KEY_MEDIA_CATEGORY, "Capture",
                PW_KEY_STREAM_CAPTURE_SINK, "true",
                NULL);

    struct pw_stream *stream = d_pw_stream_new(core, name, props);
    if (stream != NULL) {
        data->stream = stream;
        d_pw_stream_add_listener(stream, &data->stream_listener, &stream_events, data);
    }
    return stream;
}

static inline int connect_audio_stream(struct pw_stream *stream) {
    uint8_t buffer[1024];
    struct spa_pod_builder b = SPA_POD_BUILDER_INIT(buffer, sizeof(buffer));

    const struct spa_pod *params[1];
    params[0] = spa_pod_builder_add_object(&b,
        SPA_TYPE_OBJECT_Format, SPA_PARAM_EnumFormat,
        SPA_FORMAT_mediaType, SPA_POD_Id(SPA_MEDIA_TYPE_audio),
        SPA_FORMAT_mediaSubtype, SPA_POD_Id(SPA_MEDIA_SUBTYPE_raw),
        SPA_FORMAT_AUDIO_format, SPA_POD_Id(SPA_AUDIO_FORMAT_S16),
        SPA_FORMAT_AUDIO_rate, SPA_POD_Int(48000),
        SPA_FORMAT_AUDIO_channels, SPA_POD_Int(2));

    return d_pw_stream_connect(stream,
        PW_DIRECTION_INPUT,
        (uint32_t)-1,
        PW_STREAM_FLAG_AUTOCONNECT |
        PW_STREAM_FLAG_MAP_BUFFERS,
        params, 1);
}

static inline void wrap_pw_main_loop_run(struct pw_main_loop *loop) { d_pw_main_loop_run(loop); }
static inline void wrap_pw_main_loop_quit(struct pw_main_loop *loop) { d_pw_main_loop_quit(loop); }
static inline void wrap_pw_stream_destroy(struct pw_stream *stream) { d_pw_stream_destroy(stream); }
static inline void wrap_pw_core_disconnect(struct pw_core *core) { d_pw_core_disconnect(core); }
static inline void wrap_pw_context_destroy(struct pw_context *context) { d_pw_context_destroy(context); }
static inline void wrap_pw_main_loop_destroy(struct pw_main_loop *loop) { d_pw_main_loop_destroy(loop); }

*/
import "C"
import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

var ErrLibraryNotLoaded = errors.New("libpipewire-0.3.so.0 could not be loaded")

const (
	videoQueueSize = 8
	audioQueueSize = 384
)

type Stream struct {
	loop    *C.struct_pw_main_loop
	context *C.struct_pw_context
	core    *C.struct_pw_core
	cData   *C.struct_go_stream_data

	id int
	pr *io.PipeReader
	pw *io.PipeWriter

	kind    string
	frameCh chan []byte
	frameMu sync.RWMutex

	wg        sync.WaitGroup
	writeWG   sync.WaitGroup
	statsWG   sync.WaitGroup
	startOnce sync.Once
	closeOnce sync.Once
	closeErr  error
	closed    atomic.Bool
	statsStop chan struct{}

	framesIn       atomic.Uint64
	framesDropped  atomic.Uint64
	framesWritten  atomic.Uint64
	frameBytesIn   atomic.Uint64
	frameBytesOut  atomic.Uint64
	writeErrs      atomic.Uint64
	lastFrameWrite atomic.Int64
	framerateNum   atomic.Uint32
	framerateDen   atomic.Uint32
	formatReady    chan struct{}
	formatReadySet sync.Once
}

var (
	streamsMu sync.Mutex
	streams   = make(map[int]*Stream)
	nextID    = 1
	libLoaded bool
	libMu     sync.Mutex

	debugInitOnce sync.Once
	debugEnabled  bool
	debugLogger   *log.Logger
)

func initDebug() {
	debugInitOnce.Do(func() {
		debugEnabled = os.Getenv("SCREENCAST_PIPEWIRE_DEBUG") == "1"
		if !debugEnabled {
			return
		}

		out := io.Writer(os.Stderr)
		if p := os.Getenv("SCREENCAST_PIPEWIRE_DEBUG_FILE"); p != "" {
			f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err == nil {
				out = f
			}
		}

		debugLogger = log.New(out, "screencast/pipewire ", log.LstdFlags|log.Lmicroseconds)
	})
}

func debugf(format string, args ...any) {
	initDebug()
	if !debugEnabled || debugLogger == nil {
		return
	}
	debugLogger.Printf(format, args...)
}

// IsAvailable checks if the PipeWire C library can be loaded.
func IsAvailable() bool {
	libMu.Lock()
	defer libMu.Unlock()
	if libLoaded {
		return true
	}
	if C.load_pipewire() == 1 {
		libLoaded = true
		C.wrap_pw_init()
		return true
	}
	return false
}

func NewStream(fd int, nodeID uint32, width, height uint32) (*Stream, error) {
	if !IsAvailable() {
		return nil, ErrLibraryNotLoaded
	}

	pr, pw := io.Pipe()
	s := &Stream{
		pr:          pr,
		pw:          pw,
		kind:        "video",
		frameCh:     make(chan []byte, videoQueueSize),
		statsStop:   make(chan struct{}),
		formatReady: make(chan struct{}),
	}
	s.startPipeWriter()
	s.startDebugStats()

	streamsMu.Lock()
	s.id = nextID
	nextID++
	streamsMu.Unlock()

	// dup fd because pw_context_connect_fd takes ownership
	dupFd, err := syscall.Dup(fd)
	if err != nil {
		return nil, fmt.Errorf("dup fd: %v", err)
	}
	defer func() {
		if dupFd >= 0 {
			_ = syscall.Close(dupFd)
		}
	}()

	cleanupOnError := func(err error) (*Stream, error) {
		_ = s.Close()
		return nil, err
	}

	s.loop = C.wrap_pw_main_loop_new()
	if s.loop == nil {
		return cleanupOnError(fmt.Errorf("failed to create main loop"))
	}

	s.context = C.wrap_pw_context_new(s.loop)
	if s.context == nil {
		return cleanupOnError(fmt.Errorf("failed to create context"))
	}

	s.core = C.wrap_pw_context_connect_fd(s.context, C.int(dupFd))
	if s.core == nil {
		return cleanupOnError(fmt.Errorf("failed to connect fd"))
	}
	dupFd = -1 // ownership was transferred to PipeWire

	name := C.CString("screencast-capture")
	defer C.free(unsafe.Pointer(name))

	s.cData = (*C.struct_go_stream_data)(C.malloc(C.sizeof_struct_go_stream_data))
	s.cData.id = C.int(s.id)
	s.cData.stream = nil

	stream := C.create_stream(s.core, name, s.cData)
	if stream == nil {
		return cleanupOnError(fmt.Errorf("failed to create stream"))
	}
	s.cData.stream = stream

	res := C.connect_stream(stream, C.uint32_t(nodeID), C.uint32_t(width), C.uint32_t(height), 60, 1)
	if res < 0 {
		return cleanupOnError(fmt.Errorf("failed to connect stream: %d", int(res)))
	}

	streamsMu.Lock()
	streams[s.id] = s
	streamsMu.Unlock()

	return s, nil
}

func (s *Stream) Start() {
	s.startOnce.Do(func() {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			C.wrap_pw_main_loop_run(s.loop)
		}()
	})
}

func (s *Stream) Stop() {
	if s.loop != nil {
		C.wrap_pw_main_loop_quit(s.loop)
	}
}

func (s *Stream) Read(p []byte) (int, error) {
	return s.pr.Read(p)
}

func (s *Stream) startPipeWriter() {
	s.writeWG.Add(1)
	go func() {
		defer s.writeWG.Done()
		for frame := range s.frameCh {
			if len(frame) == 0 {
				continue
			}
			if _, err := s.pw.Write(frame); err != nil {
				s.writeErrs.Add(1)
				debugf("stream=%d kind=%s writer error=%v", s.id, s.kind, err)
				return
			}
			s.framesWritten.Add(1)
			s.frameBytesOut.Add(uint64(len(frame)))
			s.lastFrameWrite.Store(time.Now().UnixNano())
		}
	}()
}

func (s *Stream) startDebugStats() {
	initDebug()
	if !debugEnabled {
		return
	}

	s.statsWG.Add(1)
	go func() {
		defer s.statsWG.Done()
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()

		for {
			select {
			case <-s.statsStop:
				return
			case <-t.C:
				lastWriteNs := s.lastFrameWrite.Load()
				lastWrite := "never"
				if lastWriteNs > 0 {
					lastWrite = time.Unix(0, lastWriteNs).Format(time.RFC3339Nano)
				}

				debugf(
					"stream=%d kind=%s qlen=%d in=%d out=%d drop=%d in_bytes=%d out_bytes=%d write_errs=%d last_write=%s",
					s.id,
					s.kind,
					len(s.frameCh),
					s.framesIn.Load(),
					s.framesWritten.Load(),
					s.framesDropped.Load(),
					s.frameBytesIn.Load(),
					s.frameBytesOut.Load(),
					s.writeErrs.Load(),
					lastWrite,
				)
			}
		}
	}()
}

func (s *Stream) enqueueFrame(data unsafe.Pointer, size C.uint32_t) {
	if s == nil || data == nil || size == 0 || s.closed.Load() {
		return
	}

	s.framesIn.Add(1)
	s.frameBytesIn.Add(uint64(size))

	buf := make([]byte, int(size))
	copy(buf, unsafe.Slice((*byte)(data), int(size)))

	s.frameMu.RLock()
	defer s.frameMu.RUnlock()
	if s.closed.Load() {
		return
	}

	select {
	case s.frameCh <- buf:
	default:
		// Keep stream near-live under backpressure: drop oldest frame.
		select {
		case <-s.frameCh:
			s.framesDropped.Add(1)
		default:
		}
		select {
		case s.frameCh <- buf:
		default:
			s.framesDropped.Add(1)
		}
	}
}

func (s *Stream) Close() error {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		close(s.statsStop)
		s.statsWG.Wait()
		s.Stop()
		s.wg.Wait() // wait for main loop to exit fully

		debugf("stream=%d kind=%s closing", s.id, s.kind)

		streamsMu.Lock()
		delete(streams, s.id)
		streamsMu.Unlock()

		s.frameMu.Lock()
		close(s.frameCh)
		s.frameMu.Unlock()
		s.formatReadySet.Do(func() {
			close(s.formatReady)
		})

		// Close reader first to unblock writer goroutine if it is stuck in Pipe write.
		prErr := s.pr.Close()
		s.writeWG.Wait()
		pwErr := s.pw.Close()
		err := errors.Join(prErr, pwErr)

		if s.cData != nil {
			if s.cData.stream != nil {
				C.wrap_pw_stream_destroy(s.cData.stream)
			}
			C.free(unsafe.Pointer(s.cData))
			s.cData = nil
		}
		if s.core != nil {
			C.wrap_pw_core_disconnect(s.core)
			s.core = nil
		}
		if s.context != nil {
			C.wrap_pw_context_destroy(s.context)
			s.context = nil
		}
		if s.loop != nil {
			C.wrap_pw_main_loop_destroy(s.loop)
			s.loop = nil
		}

		s.closeErr = err
		debugf("stream=%d kind=%s closed err=%v", s.id, s.kind, err)
	})

	return s.closeErr
}

func (s *Stream) setFrameRate(num, den uint32) {
	if s == nil || den == 0 {
		return
	}

	s.framerateNum.Store(num)
	s.framerateDen.Store(den)
	s.formatReadySet.Do(func() {
		close(s.formatReady)
	})
}

func (s *Stream) FrameRate() uint32 {
	if s == nil {
		return 0
	}

	num := s.framerateNum.Load()
	den := s.framerateDen.Load()
	if den == 0 {
		return 0
	}

	fps := num / den
	if num%den != 0 {
		fps++
	}

	return fps
}

func (s *Stream) WaitFrameRate(timeout time.Duration) uint32 {
	if s == nil {
		return 0
	}
	if timeout <= 0 {
		return s.FrameRate()
	}

	select {
	case <-s.formatReady:
	case <-time.After(timeout):
	}

	return s.FrameRate()
}

//export on_state_changed_go
func on_state_changed_go(id C.int, old C.enum_pw_stream_state, state C.enum_pw_stream_state, cErr *C.char) {
	errText := ""
	if cErr != nil {
		errText = C.GoString(cErr)
	}
	debugf("stream=%d state old=%d new=%d err=%q", int(id), int(old), int(state), errText)
}

//export on_param_changed_go
func on_param_changed_go(id C.int, framerateNum C.uint32_t, framerateDen C.uint32_t) {
	streamsMu.Lock()
	s, ok := streams[int(id)]
	streamsMu.Unlock()
	if !ok {
		return
	}

	s.setFrameRate(uint32(framerateNum), uint32(framerateDen))
	debugf("stream=%d kind=%s format framerate=%d/%d", int(id), s.kind, uint32(framerateNum), uint32(framerateDen))
}

//export on_frame_go
func on_frame_go(id C.int, data unsafe.Pointer, size C.uint32_t) {
	streamsMu.Lock()
	s, ok := streams[int(id)]
	streamsMu.Unlock()

	if !ok {
		return
	}

	s.enqueueFrame(data, size)
}

func NewAudioStream() (*Stream, error) {
	if !IsAvailable() {
		return nil, ErrLibraryNotLoaded
	}

	pr, pw := io.Pipe()
	s := &Stream{
		pr:          pr,
		pw:          pw,
		kind:        "audio",
		frameCh:     make(chan []byte, audioQueueSize),
		statsStop:   make(chan struct{}),
		formatReady: make(chan struct{}),
	}
	s.startPipeWriter()
	s.startDebugStats()

	streamsMu.Lock()
	s.id = nextID
	nextID++
	streamsMu.Unlock()

	cleanupOnError := func(err error) (*Stream, error) {
		_ = s.Close()
		return nil, err
	}

	s.loop = C.wrap_pw_main_loop_new()
	if s.loop == nil {
		return cleanupOnError(fmt.Errorf("failed to create main loop"))
	}

	s.context = C.wrap_pw_context_new(s.loop)
	if s.context == nil {
		return cleanupOnError(fmt.Errorf("failed to create context"))
	}

	s.core = C.wrap_pw_context_connect(s.context)
	if s.core == nil {
		return cleanupOnError(fmt.Errorf("failed to connect to pipewire daemon"))
	}

	name := C.CString("screencast-audio-capture")
	defer C.free(unsafe.Pointer(name))

	s.cData = (*C.struct_go_stream_data)(C.malloc(C.sizeof_struct_go_stream_data))
	s.cData.id = C.int(s.id)
	s.cData.stream = nil

	stream := C.create_audio_stream(s.core, name, s.cData)
	if stream == nil {
		return cleanupOnError(fmt.Errorf("failed to create audio stream"))
	}
	s.cData.stream = stream

	res := C.connect_audio_stream(stream)
	if res < 0 {
		return cleanupOnError(fmt.Errorf("failed to connect audio stream: %d", int(res)))
	}

	streamsMu.Lock()
	streams[s.id] = s
	streamsMu.Unlock()

	return s, nil
}
