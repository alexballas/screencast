# screencast - Unified Screen Capture (BGRA Frames)

A Go package for screen capture with a unified `io.Reader` interface that yields raw `BGRA` frames. Linux is implemented via `xdg-desktop-portal` + PipeWire, with OS-specific capture backends split by build tags.

## Features

- **Native UI Prompts:** Triggers the native OS dialog for the user to select specific screens or windows to capture.
- **Zero-Copy Performance:** Uses `cgo` and `libpipewire-0.3` to access PipeWire's shared-memory DMA-BUF/memfd buffers, avoiding expensive memory copies.
- **Unified `io.Reader` Interface:** Pipe raw `BGRA` frames directly into `ffmpeg` or any standard Go stream.
- **Graceful Fallback:** Dynamically loads the PipeWire C library at runtime (`dlopen`). The binary will not crash on systems where PipeWire is not installed; it will simply return an error.

## Requirements

- Linux with [xdg-desktop-portal](https://github.com/flatpak/xdg-desktop-portal)
- [PipeWire](https://pipewire.org/) running
- DBus session bus
- `libpipewire-0.3-dev` (Only required at **build time** for C headers)

## Installation

```bash
# Ubuntu / Debian build requirements
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
	"log"
	"os"
	"os/exec"
	"strconv"

	"go2tv.app/screencast/capture"
)

func main() {
	stream, err := capture.Open(nil)
	if err != nil {
		log.Fatalf("Failed to open capture stream: %v", err)
	}
	defer stream.Close()

	frameRate := stream.FrameRate
	if frameRate == 0 {
		frameRate = 60
	}

	// Pipe the io.Reader directly into FFmpeg
	cmd := exec.Command("ffmpeg",
		"-y", "-re",
		"-f", "rawvideo",
		"-pix_fmt", "bgra",
		"-s", fmt.Sprintf("%dx%d", stream.Width, stream.Height),
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
- **macOS:** API surface in place; backend target is ScreenCaptureKit (`SCStream`)
- **Windows:** API surface in place; backend target is Windows Graphics Capture (WGC)

## License

MIT
