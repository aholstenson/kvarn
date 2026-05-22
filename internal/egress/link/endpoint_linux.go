//go:build linux

package link

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sync"
)

// StreamFrameRW reads and writes ethernet frames over qemu's
// `-netdev stream,addr.type=fd` transport. Stream mode prefixes each frame
// with a 4-byte big-endian length.
type StreamFrameRW struct {
	fd     *os.File
	rdMu   sync.Mutex
	wrMu   sync.Mutex
	header [4]byte
}

// NewStreamFrameRW takes ownership of fd. Callers are responsible for the
// other end of the socket pair (the one handed to qemu via ExtraFiles).
func NewStreamFrameRW(fd *os.File) *StreamFrameRW {
	return &StreamFrameRW{fd: fd}
}

func (s *StreamFrameRW) ReadFrame() ([]byte, error) {
	s.rdMu.Lock()
	defer s.rdMu.Unlock()

	if _, err := io.ReadFull(s.fd, s.header[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(s.header[:])
	if n == 0 || n > 65536 {
		return nil, fmt.Errorf("invalid frame length %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(s.fd, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func (s *StreamFrameRW) WriteFrame(frame []byte) error {
	s.wrMu.Lock()
	defer s.wrMu.Unlock()

	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(frame)))
	if _, err := s.fd.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := s.fd.Write(frame); err != nil {
		return err
	}
	return nil
}

func (s *StreamFrameRW) Close() error { return s.fd.Close() }
