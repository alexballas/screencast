package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"go2tv.app/screencast/internal/pipewire"
	"go2tv.app/screencast/screencast"
)

func main() {
	fmt.Println("Screen capture with live transcoding to H.264...")
	fmt.Println("Press Ctrl+C to stop")

	sess, err := screencast.CreateSession(nil)
	if err != nil {
		log.Fatalf("Failed to create session: %v", err)
	}
	if sess == nil {
		log.Fatal("Session creation was cancelled")
	}

	err = sess.SelectSources(&screencast.SelectSourcesOptions{
		Types:      screencast.SourceTypeMonitor | screencast.SourceTypeWindow,
		CursorMode: screencast.CursorModeEmbedded,
		Multiple:   false,
	})
	if err != nil {
		log.Fatalf("Failed to select sources: %v", err)
	}

	streams, err := sess.Start("", nil)
	if err != nil {
		log.Fatalf("Failed to start: %v", err)
	}
	if streams == nil {
		log.Fatal("Start was cancelled")
	}

	if len(streams) == 0 {
		log.Fatal("No streams available")
	}

	streamInfo := streams[0]
	width, height := streamInfo.Size[0], streamInfo.Size[1]
	fmt.Printf("Capturing: %dx%d\n", width, height)

	fd, err := sess.OpenPipeWireRemote(nil)
	if err != nil {
		log.Fatalf("Failed to open PipeWire remote: %v", err)
	}
	fmt.Printf("PipeWire fd: %d\n", fd)

	if !pipewire.IsAvailable() {
		log.Fatalf("PipeWire is not available on this system.")
	}

	pwStream, err := pipewire.NewStream(fd, uint32(streamInfo.NodeID), uint32(width), uint32(height))
	if err != nil {
		log.Fatalf("Failed to create PipeWire stream: %v", err)
	}

	pwStream.Start()

	cmd := exec.Command("ffmpeg",
		"-y",
		"-re",
		"-f", "rawvideo",
		"-pix_fmt", "bgra",
		"-s", fmt.Sprintf("%dx%d", width, height),
		"-r", "60",
		"-i", "pipe:0",
		"-c:v", "libx264",
		"-preset", "ultrafast",
		"-tune", "zerolatency",
		"-pix_fmt", "yuv420p",
		"-f", "mp4",
		"/home/alex/test/screencast/output.mp4",
	)
	cmd.Stdin = pwStream
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		log.Fatalf("Failed to start ffmpeg: %v", err)
	}

	fmt.Println("\nRecording... Press Ctrl+C to stop.")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		fmt.Println("\nStopping...")
		pwStream.Close() // Closing the stream closes its reader, which sends EOF to ffmpeg
		time.Sleep(500 * time.Millisecond)
		if cmd.Process != nil {
			cmd.Process.Signal(syscall.SIGTERM)
		}
	}()

	err = cmd.Wait()
	if err != nil {
		fmt.Printf("FFmpeg exited: %v\n", err)
	}

	sess.Close()
	fmt.Println("Output saved to /home/alex/test/screencast/output.mp4")
}
