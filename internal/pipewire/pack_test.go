//go:build linux

package pipewire

import (
	"bytes"
	"testing"
)

func TestPackFrame(t *testing.T) {
	// 2x2 BGRx frame with distinct pixels.
	tight := []byte{
		1, 1, 1, 0, 2, 2, 2, 0,
		3, 3, 3, 0, 4, 4, 4, 0,
	}
	// Same frame with 8 bytes of row padding, as GPUs commonly align stride.
	padded := []byte{
		1, 1, 1, 0, 2, 2, 2, 0, 9, 9, 9, 9, 9, 9, 9, 9,
		3, 3, 3, 0, 4, 4, 4, 0, 9, 9, 9, 9, 9, 9, 9, 9,
	}

	tt := []struct {
		name       string
		src        []byte
		stride     int
		srcW, srcH int
		outW, outH int
		want       []byte
	}{
		{name: "tight stride", src: tight, stride: 8, srcW: 2, srcH: 2, outW: 2, outH: 2, want: tight},
		{name: "unknown stride assumes tight", src: tight, stride: 0, srcW: 2, srcH: 2, outW: 2, outH: 2, want: tight},
		{name: "padded stride is repacked", src: padded, stride: 16, srcW: 2, srcH: 2, outW: 2, outH: 2, want: tight},
		{name: "last row without padding", src: padded[:24], stride: 16, srcW: 2, srcH: 2, outW: 2, outH: 2, want: tight},
		// HiDPI: buffers in physical pixels, reader told logical size.
		{name: "negotiated size differs from output", src: tight, stride: 8, srcW: 2, srcH: 2, outW: 1, outH: 1},
		{name: "output size not set", src: tight, stride: 8, srcW: 2, srcH: 2},
		{name: "short buffer", src: tight[:12], stride: 8, srcW: 2, srcH: 2, outW: 2, outH: 2},
		{name: "stride below row size", src: tight, stride: 4, srcW: 2, srcH: 2, outW: 2, outH: 2},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			got := packFrame(tc.src, tc.stride, tc.srcW, tc.srcH, tc.outW, tc.outH)
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("packFrame = %v, want %v", got, tc.want)
			}
			if tc.want == nil && got != nil {
				t.Fatalf("packFrame = %v, want dropped frame", got)
			}
		})
	}
}
