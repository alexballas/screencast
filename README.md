# screencast - Unified Screen Capture (BGRA Frames)

A Go package for screen capture with a unified `io.Reader` interface that yields raw `BGRA` frames.
Supports Linux (`xdg-desktop-portal` + PipeWire), macOS (ScreenCaptureKit), and Windows (Windows Graphics Capture).

## Features

- **Cross-Platform:** Native OS dialogs or direct capture APIs on Linux, macOS 13+, and Windows 10+.
- **Zero-Copy Performance (Linux):** Uses `cgo` and `libpipewire-0.3` to access PipeWire's shared-memory DMA-BUF/memfd buffers, avoiding expensive memory copies.
- **Unified `io.Reader` Interface:** Pipe raw `BGRA` frames directly into `ffmpeg` or any standard Go stream.
- **Audio Capture:** Optional system audio capture (48kHz, 16-bit, stereo) on supported platforms.
- **Graceful Fallback:** Dynamically loads the PipeWire C library at runtime (`dlopen`).
- **Robust Lifecycle Handling:** Idempotent close paths, auto-cleanup when ffmpeg exits, and first-frame startup timeouts on desktop backends.

## Requirements

### Linux
- [xdg-desktop-portal](https://github.com/flatpak/xdg-desktop-portal)
- [PipeWire](https://pipewire.org/) running
- DBus session bus
- `libpipewire-0.3-dev` (Only required at **build time** for C headers)

### macOS
- macOS 13.0 or later
- CGO enabled

### Windows
- Windows 10 (1809) or later
- CGO enabled with a C/C++ compiler (e.g., MSYS2/MinGW-w64)
- Uses Windows Graphics Capture (WinRT + D3D11). Windows 8.1 and older are not supported and may fail at runtime even if the application starts.

## Installation

```bash
# Ubuntu / Debian build requirements (Linux only)
sudo apt install libpipewire-0.3-dev pkg-config

# Get the Go package
go get go2tv.app/screencast
```

## Quick Start (Capture to FFmpeg)

This minimal example shows how to open a capture stream and pipe raw `BGRA` frames directly to FFmpeg.

```go
package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"

	"go2tv.app/screencast/capture"
)

func main() {
	// Audio is enabled by default. Use &capture.Options{IncludeAudio: false} to disable.
	stream, err := capture.Open(nil)
	if err != nil {
		log.Fatalf("Failed to open capture stream: %v", err)
	}
	defer stream.Close()

	if stream.Audio != nil {
		fmt.Println("Audio capture enabled (draining in background)")
		go func() {
			_, _ = io.Copy(io.Discard, stream.Audio)
		}()
	}

	frameRate := stream.FrameRate
	if frameRate == 0 {
		frameRate = 60
	}

	width, height := stream.Width, stream.Height

	// Pipe the io.Reader directly into FFmpeg
	cmd := exec.Command("ffmpeg",
		"-y", "-re",
		"-f", "rawvideo",
		"-pix_fmt", "bgra",
		"-s", fmt.Sprintf("%dx%d", width, height),
		"-r", strconv.FormatUint(uint64(frameRate), 10),
		"-i", "pipe:0", // Read from stdin
		"-c:v", "libx264",
		"-preset", "ultrafast",
		"-pix_fmt", "yuv420p",
		"output.mp4",
	)
	
	cmd.Stdin = stream
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	fmt.Println("Recording... Press Ctrl+C to stop.")
	if err := cmd.Run(); err != nil {
		log.Fatalf("FFmpeg exited with error: %v", err)
	}
}
```

## Running the Example

```bash
go run examples/capture/main.go
```

## Cross-Platform Status

- **Linux:** Implemented (`xdg-desktop-portal` + PipeWire)
- **macOS:** Implemented (ScreenCaptureKit)
- **Windows:** Implemented (Windows Graphics Capture)
  - Limitation: requires Windows 10 version 1809 or newer.

## End-to-End Debugging

Set one environment variable to enable debug logging across capture, HLS, and ffmpeg paths:

```bash
SCREENCAST_DEBUG=1
```

Optional: write debug logs to a file (recommended for GUI apps on Windows/macOS):

```bash
SCREENCAST_DEBUG=1 SCREENCAST_DEBUG_FILE=/tmp/screencast-debug.log
```

What `SCREENCAST_DEBUG=1` enables:

- Capture lifecycle logs on all platforms (Linux/macOS/Windows), including first-frame timing.
- Slow write diagnostics in Windows/macOS capture callbacks (helps identify buffering/backpressure).
- PipeWire internal stream debug logs (Linux backend).
- ffmpeg command printing and ffmpeg stderr capture in `hls.Start`.
- ffmpeg `-loglevel debug` in `hls.Start`.
- HLS HTTP directory handler debug logs in `hls.NewDirectoryHandler`.

What `SCREENCAST_DEBUG_FILE=/path/to/log` does:

- Routes capture debug logs to the file on all platforms (Linux/macOS/Windows).
- Routes HLS debug logs to the file on all platforms (Linux/macOS/Windows).
- Routes PipeWire debug logs to the same file on Linux.

Legacy variables still supported:

- `SCREENCAST_CAPTURE_DEBUG=1`
- `SCREENCAST_CAPTURE_DEBUG_FILE=/path/to/file`
- `SCREENCAST_PIPEWIRE_DEBUG=1`
- `SCREENCAST_PIPEWIRE_DEBUG_FILE=/path/to/file`

## HLS Session Notes

- `hls.Session` cleanup is idempotent (`Close()` can be called multiple times safely).
- If ffmpeg exits unexpectedly, session resources are now auto-cleaned.
- Desktop backends use a first-frame timeout to avoid indefinite startup hangs.
- For diagnostics after failure, use `Session.StderrTail(n)`.

## License

MIT
