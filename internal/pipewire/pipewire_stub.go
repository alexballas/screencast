//go:build !linux

package pipewire

import (
	"errors"
	"io"
)

var ErrLibraryNotLoaded = errors.New("pipewire capture backend is only available on linux")

type Stream struct{}

func IsAvailable() bool {
	return false
}

func NewStream(fd int, nodeID uint32, width, height uint32) (*Stream, error) {
	return nil, ErrLibraryNotLoaded
}

func NewAudioStream() (*Stream, error) {
	return nil, ErrLibraryNotLoaded
}

func (s *Stream) Start() {}

func (s *Stream) Stop() {}

func (s *Stream) Read(p []byte) (int, error) {
	return 0, io.EOF
}

func (s *Stream) Close() error {
	return nil
}
