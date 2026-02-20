//go:build linux

package pipewire

/*
#cgo pkg-config: libpipewire-0.3
#cgo LDFLAGS: -ldl
#include <pipewire/pipewire.h>
#include <spa/param/video/format-utils.h>
#include <spa/param/audio/format-utils.h>
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

struct go_stream_data {
    int id;
    struct pw_stream *stream;
    struct spa_hook stream_listener;
};

static void on_state_changed_c(void *userdata, enum pw_stream_state old, enum pw_stream_state state, const char *error) {
    struct go_stream_data *data = userdata;
    on_state_changed_go(data->id, old, state, (char*)error);
}

static void on_process_c(void *userdata) {
    struct go_stream_data *data = userdata;
    if (!data->stream) return;

    struct pw_buffer *b = d_pw_stream_dequeue_buffer(data->stream);
    if (b == NULL) {
        return;
    }

    struct spa_buffer *buf = b->buffer;
    if (buf->datas[0].data != NULL && buf->datas[0].chunk != NULL) {
        uint32_t size = buf->datas[0].chunk->size;
        if (size > 0) {
            on_frame_go(data->id, buf->datas[0].data, size);
        }
    }

    d_pw_stream_queue_buffer(data->stream, b);
}

static const struct pw_stream_events stream_events = {
    PW_VERSION_STREAM_EVENTS,
    .state_changed = on_state_changed_c,
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
	"sync"
	"syscall"
	"unsafe"
)

var ErrLibraryNotLoaded = errors.New("libpipewire-0.3.so.0 could not be loaded")

type Stream struct {
	loop    *C.struct_pw_main_loop
	context *C.struct_pw_context
	core    *C.struct_pw_core
	cData   *C.struct_go_stream_data

	id int
	pr *io.PipeReader
	pw *io.PipeWriter

	wg        sync.WaitGroup
	startOnce sync.Once
	closeOnce sync.Once
	closeErr  error
}

var (
	streamsMu sync.Mutex
	streams   = make(map[int]*Stream)
	nextID    = 1
	libLoaded bool
	libMu     sync.Mutex
)

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
		pr: pr,
		pw: pw,
	}

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

func (s *Stream) Close() error {
	s.closeOnce.Do(func() {
		s.Stop()
		s.wg.Wait() // wait for main loop to exit fully

		err := errors.Join(s.pw.Close(), s.pr.Close())

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

		streamsMu.Lock()
		delete(streams, s.id)
		streamsMu.Unlock()

		s.closeErr = err
	})

	return s.closeErr
}

//export on_state_changed_go
func on_state_changed_go(id C.int, old C.enum_pw_stream_state, state C.enum_pw_stream_state, error *C.char) {
	// Intentionally suppressed for clean output
}

//export on_frame_go
func on_frame_go(id C.int, data unsafe.Pointer, size C.uint32_t) {
	streamsMu.Lock()
	s, ok := streams[int(id)]
	streamsMu.Unlock()

	if !ok {
		return
	}

	byteSlice := unsafe.Slice((*byte)(data), int(size))
	s.pw.Write(byteSlice)
}

func NewAudioStream() (*Stream, error) {
	if !IsAvailable() {
		return nil, ErrLibraryNotLoaded
	}

	pr, pw := io.Pipe()
	s := &Stream{
		pr: pr,
		pw: pw,
	}

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
