package pipeline

import "testing"

func TestLockedBufferTailOnEmpty(t *testing.T) {
	b := &lockedBuffer{}
	if got := b.Tail(64); got != "no ffmpeg stderr output" {
		t.Fatalf("Tail() on empty buffer = %q", got)
	}
}

// TestLockedBufferKeepsCorrectTailAcrossTrims writes several times the cap and
// checks both that growth stays bounded and that the retained bytes still match
// what was written. The trim rewrites the buffer in place over storage the
// retained slice aliases, so a bad copy would corrupt the tail rather than fail
// loudly.
func TestLockedBufferKeepsCorrectTailAcrossTrims(t *testing.T) {
	b := &lockedBuffer{}

	// Letters only: Tail trims surrounding space, so whitespace at a boundary
	// would make the comparison lie.
	var want []byte
	const chunkSize = 100 * 1024
	for c := range 40 {
		chunk := make([]byte, chunkSize)
		for i := range chunk {
			chunk[i] = byte('a' + (c*chunkSize+i)%26)
		}
		n, err := b.Write(chunk)
		if err != nil {
			t.Fatalf("Write() error = %v", err)
		}
		if n != len(chunk) {
			t.Fatalf("Write() = %d bytes, want %d", n, len(chunk))
		}
		want = append(want, chunk...)

		if got := b.buf.Len(); got > maxStderrCaptureBytes {
			t.Fatalf("buffer grew to %d bytes, want at most %d", got, maxStderrCaptureBytes)
		}
	}

	// Well inside keepStderrCaptureBytes, so every trim must have preserved it.
	const tailBytes = 64 * 1024
	if got, expect := b.Tail(tailBytes), string(want[len(want)-tailBytes:]); got != expect {
		t.Fatalf("Tail(%d) does not match the last %d bytes written", tailBytes, tailBytes)
	}
}
