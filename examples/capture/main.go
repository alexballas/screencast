package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"go2tv.app/screencast/capture"
)

func main() {
	fmt.Println("Screen capture with live transcoding to H.264...")
	fmt.Println("Press Ctrl+C to stop")

	stream, err := capture.Open(&capture.Options{
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
	fmt.Printf("Capturing: %dx%d %s @ %d FPS\n", width, height, stream.PixelFormat, frameRate)

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
