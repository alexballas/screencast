package hls

import (
	"testing"

	"go2tv.app/screencast/capture"
)

func TestTargetFPSForPlatform(t *testing.T) {
	tests := []struct {
		name     string
		goos     string
		stream   *capture.Stream
		expected uint32
	}{
		{
			name:     "linux 1440p capped to 15",
			goos:     "linux",
			stream:   &capture.Stream{Width: 2560, Height: 1440, FrameRate: 30, PixelFormat: capture.PixelFormatBGRA},
			expected: 15,
		},
		{
			name:     "linux 4k capped to 10",
			goos:     "linux",
			stream:   &capture.Stream{Width: 3840, Height: 2160, FrameRate: 60, PixelFormat: capture.PixelFormatBGRA},
			expected: 10,
		},
		{
			name:     "linux 1080p keeps generic cap",
			goos:     "linux",
			stream:   &capture.Stream{Width: 1920, Height: 1080, FrameRate: 60, PixelFormat: capture.PixelFormatBGRA},
			expected: 60,
		},
		{
			name:     "darwin 1440p keeps generic highres cap",
			goos:     "darwin",
			stream:   &capture.Stream{Width: 2560, Height: 1440, FrameRate: 60, PixelFormat: capture.PixelFormatBGRA},
			expected: 30,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := targetFPSForPlatform(tc.goos, tc.stream); got != tc.expected {
				t.Fatalf("targetFPSForPlatform(%q) = %d, want %d", tc.goos, got, tc.expected)
			}
		})
	}
}
