package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go2tv.app/screencast/hls"
)

func main() {
	ffmpegPath := os.Getenv("SCREENCAST_FFMPEG")
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}

	listenPort := hls.IntEnvClamped("SCREENCAST_HLS_PORT", 8080, 1, 65535)

	session, err := hls.Start(&hls.Options{
		FFmpegPath:   ffmpegPath,
		IncludeAudio: hls.BoolEnv("SCREENCAST_HLS_AUDIO", true),
		LogOutput:    os.Stderr,
	})
	if err != nil {
		log.Fatalf("start hls session: %v", err)
	}
	defer session.Close()

	handler := hls.NewDirectoryHandler(session.Dir(), nil)
	server := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", listenPort),
		Handler: handler,
	}

	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("http server error: %v", err)
		}
	}()

	fmt.Printf("Serving HLS on http://%s/playlist.m3u8\n", server.Addr)
	fmt.Println("Press Ctrl+C to stop")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sig)
	<-sig

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}
