package hls

import (
	"testing"

	"go2tv.app/screencast/capture"
)

func TestTargetFPS(t *testing.T) {
	tests := []struct {
		name     string
		stream   *capture.Stream
		expected uint32
	}{
		{
			name:     "1440p capped to 30",
			stream:   &capture.Stream{Width: 2560, Height: 1440, FrameRate: 30, PixelFormat: capture.PixelFormatBGRA},
			expected: 30,
		},
		{
			name:     "4k capped to 30",
			stream:   &capture.Stream{Width: 3840, Height: 2160, FrameRate: 60, PixelFormat: capture.PixelFormatBGRA},
			expected: 30,
		},
		{
			name:     "1080p keeps max frame rate",
			stream:   &capture.Stream{Width: 1920, Height: 1080, FrameRate: 60, PixelFormat: capture.PixelFormatBGRA},
			expected: 60,
		},
		{
			name:     "unknown frame rate falls back to max frame rate",
			stream:   &capture.Stream{Width: 1280, Height: 720, FrameRate: 0, PixelFormat: capture.PixelFormatBGRA},
			expected: 60,
		},
		{
			name:     "high source frame rate capped to max frame rate",
			stream:   &capture.Stream{Width: 1280, Height: 720, FrameRate: 144, PixelFormat: capture.PixelFormatBGRA},
			expected: 60,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := targetFPS(tc.stream); got != tc.expected {
				t.Fatalf("targetFPS() = %d, want %d", got, tc.expected)
			}
		})
	}
}
