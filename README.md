# screencast - Screen Casting via xdg-desktop-portal

A Go package for screen casting using the Linux `xdg-desktop-portal` ScreenCast API. It automatically requests permissions, allows the user to select monitors or windows, and yields a native `io.Reader` of zero-copy raw video frames via PipeWire.

## Features

- **Native UI Prompts:** Triggers the native OS dialog for the user to select specific screens or windows to capture.
- **Zero-Copy Performance:** Uses `cgo` and `libpipewire-0.3` to access PipeWire's shared-memory DMA-BUF/memfd buffers, avoiding expensive memory copies.
- **`io.Reader` Interface:** Abstracted perfectly so you can pipe the raw `BGRA` frames directly into `ffmpeg` or any standard Go I/O stream.
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

This minimal example shows how to request a screen, open the PipeWire stream, and pipe the raw frames directly to FFmpeg to encode an MP4 file.

```go
package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"

	"go2tv.app/screencast/internal/pipewire"
	"go2tv.app/screencast/screencast"
)

func main() {
	// 1. Ensure PipeWire is installed and available
	if !pipewire.IsAvailable() {
		log.Fatalf("PipeWire is not available on this system.")
	}

	// 2. Create a desktop portal session
	sess, err := screencast.CreateSession(nil)
	if err != nil {
		log.Fatalf("Failed to create session: %v", err)
	}
	defer sess.Close()

	// 3. Prompt the user to select a monitor or window
	err = sess.SelectSources(&screencast.SelectSourcesOptions{
		Types:      screencast.SourceTypeMonitor | screencast.SourceTypeWindow,
		CursorMode: screencast.CursorModeEmbedded,
	})
	if err != nil {
		log.Fatalf("Failed to select sources: %v", err)
	}

	// 4. Start the portal session
	streams, err := sess.Start("", nil)
	if err != nil || len(streams) == 0 {
		log.Fatalf("Failed to start or no streams available")
	}
	
	streamInfo := streams[0]
	width, height := uint32(streamInfo.Size[0]), uint32(streamInfo.Size[1])
	fmt.Printf("Capturing: %dx%d\n", width, height)

	// 5. Get the PipeWire file descriptor from the portal
	fd, err := sess.OpenPipeWireRemote(nil)
	if err != nil {
		log.Fatalf("Failed to open PipeWire remote: %v", err)
	}

	// 6. Connect the PipeWire Stream (Implements io.Reader)
	pwStream, err := pipewire.NewStream(fd, uint32(streamInfo.NodeID), width, height)
	if err != nil {
		log.Fatalf("Failed to create PipeWire stream: %v", err)
	}
	pwStream.Start()
	defer pwStream.Close()

	// 7. Pipe the io.Reader directly into FFmpeg
	cmd := exec.Command("ffmpeg",
		"-y", "-re",
		"-f", "rawvideo",
		"-pix_fmt", "bgra", // PipeWire outputs raw BGRA
		"-s", fmt.Sprintf("%dx%d", width, height),
		"-r", "60", // 60 FPS
		"-i", "pipe:0", // Read from stdin
		"-c:v", "libx264",
		"-preset", "ultrafast",
		"-pix_fmt", "yuv420p",
		"output.mp4",
	)
	
	cmd.Stdin = pwStream
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

## Cross-Platform Roadmap

Currently, this library supports Linux (Wayland/X11) via `xdg-desktop-portal` and PipeWire. 
Future updates aim to provide the exact same `io.Reader` interface for macOS and Windows using their respective modern capture APIs:

- **macOS:** ScreenCaptureKit (`SCStream`) via Cgo/Objective-C.
- **Windows:** Windows Graphics Capture (WGC) via WinRT.

## License

MIT
