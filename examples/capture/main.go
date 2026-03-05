package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"go2tv.app/screencast/capture"
	"go2tv.app/screencast/internal/processutil"
)

func main() {
	fmt.Println("Screen capture with live transcoding to H.264...")
	fmt.Println("Press Ctrl+C to stop")

	streamIndex := 0
	if raw := strings.TrimSpace(os.Getenv("SCREENCAST_STREAM_INDEX")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			log.Fatalf("Invalid SCREENCAST_STREAM_INDEX %q: %v", raw, err)
		}
		if parsed < 0 {
			log.Fatalf("Invalid SCREENCAST_STREAM_INDEX %q: must be >= 0", raw)
		}
		streamIndex = parsed
	}

	displays, err := capture.ListDisplays()
	if err == nil {
		fmt.Println("Available displays:")
		for _, d := range displays {
			fmt.Printf("[%d] %s (%dx%d) primary=%t\n", d.Index, d.Name, d.Width, d.Height, d.Primary)
		}
	} else if !errors.Is(err, capture.ErrNotImplemented) {
		fmt.Printf("Warning: failed to list displays: %v\n", err)
	}

	stream, err := capture.Open(&capture.Options{
		StreamIndex:  streamIndex,
		IncludeAudio: true,
	})
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

	width, height := stream.Width, stream.Height

	frameRate := stream.FrameRate
	if frameRate == 0 {
		frameRate = 60
	}
	fmt.Printf("Capturing stream index %d: %dx%d %s @ %d FPS\n", streamIndex, width, height, stream.PixelFormat, frameRate)

	outputPath := "output.mp4"
	absoluteOutputPath, err := filepath.Abs(outputPath)
	if err != nil {
		log.Fatalf("Failed to resolve output path: %v", err)
	}

	cmd := exec.Command("ffmpeg",
		"-y",
		"-re",
		"-f", "rawvideo",
		"-pix_fmt", "bgra",
		"-s", fmt.Sprintf("%dx%d", width, height),
		"-r", strconv.FormatUint(uint64(frameRate), 10),
		"-i", "pipe:0",
		"-c:v", "libx264",
		"-preset", "ultrafast",
		"-tune", "zerolatency",
		"-pix_fmt", "yuv420p",
		outputPath,
	)
	cmd.Stdin = stream
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	processutil.HideConsoleWindow(cmd)

	if err := cmd.Start(); err != nil {
		log.Fatalf("Failed to start ffmpeg: %v", err)
	}

	fmt.Println("\nRecording... Press Ctrl+C to stop.")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigChan)

	go func() {
		<-sigChan
		fmt.Println("\nStopping...")
		_ = stream.Close() // Closing the stream sends EOF to ffmpeg stdin.
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
	}()

	err = cmd.Wait()
	if err != nil {
		fmt.Printf("FFmpeg exited: %v\n", err)
	}

	fmt.Printf("Output saved to %s\n", absoluteOutputPath)
}
