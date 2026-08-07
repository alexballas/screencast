package pipeline

import "testing"

// TestQueueSizesNormalize pins the three things every knob does: an unset value
// takes the default, and anything outside the workable range is pulled to the
// nearest edge rather than rejected.
func TestQueueSizesNormalize(t *testing.T) {
	tests := []struct {
		name string
		in   QueueSizes
		want QueueSizes
	}{
		{
			name: "zero takes the defaults",
			want: QueueSizes{
				Video:      DefaultVideoQueueSize,
				Audio:      DefaultAudioQueueSize,
				AudioChunk: DefaultAudioChunkSize,
				AudioRelay: DefaultAudioRelayQueue,
			},
		},
		{
			name: "in range is left alone",
			in:   QueueSizes{Video: 512, Audio: 1024, AudioChunk: 2048, AudioRelay: 64},
			want: QueueSizes{Video: 512, Audio: 1024, AudioChunk: 2048, AudioRelay: 64},
		},
		{
			name: "below the range clamps up",
			in:   QueueSizes{Video: 1, Audio: 1, AudioChunk: 1, AudioRelay: 1},
			want: QueueSizes{Video: 128, Audio: 256, AudioChunk: 512, AudioRelay: 8},
		},
		{
			name: "negative clamps up too",
			in:   QueueSizes{Video: -1, Audio: -1, AudioChunk: -1, AudioRelay: -1},
			want: QueueSizes{Video: 128, Audio: 256, AudioChunk: 512, AudioRelay: 8},
		},
		{
			name: "above the range clamps down",
			in:   QueueSizes{Video: 1 << 20, Audio: 1 << 20, AudioChunk: 1 << 20, AudioRelay: 1 << 20},
			want: QueueSizes{Video: 16384, Audio: 32768, AudioChunk: 32768, AudioRelay: 4096},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.Normalize(); got != tt.want {
				t.Fatalf("QueueSizes%+v.Normalize() = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}
