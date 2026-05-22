//go:build darwin

package link

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// SocketPairFrameRW reads and writes raw ethernet frames over an
// AF_UNIX SOCK_DGRAM socket pair, the format vz.NewFileHandleNetworkDeviceAttachment
// expects: each datagram is exactly one ethernet frame.
type SocketPairFrameRW struct {
	fd *os.File
}

// NewSocketPairFrameRW takes ownership of the supplied file. Callers are
// responsible for closing the OTHER end (the one passed to vz).
func NewSocketPairFrameRW(fd *os.File) *SocketPairFrameRW {
	return &SocketPairFrameRW{fd: fd}
}

func (s *SocketPairFrameRW) ReadFrame() ([]byte, error) {
	buf := make([]byte, 65536)
	n, _, _, _, err := unix.Recvmsg(int(s.fd.Fd()), buf, nil, 0)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func (s *SocketPairFrameRW) WriteFrame(frame []byte) error {
	if err := unix.Sendmsg(int(s.fd.Fd()), frame, nil, nil, 0); err != nil {
		return fmt.Errorf("sendmsg: %w", err)
	}
	return nil
}

func (s *SocketPairFrameRW) Close() error {
	return s.fd.Close()
}

// CreateSocketPair returns a SOCK_DGRAM AF_UNIX socket pair. One end is
// for the netstack; the other is handed to vz.NewFileHandleNetworkDeviceAttachment.
func CreateSocketPair() (host, vm *os.File, err error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("socketpair: %w", err)
	}
	host = os.NewFile(uintptr(fds[0]), "kvarn-vm-net-host")
	vm = os.NewFile(uintptr(fds[1]), "kvarn-vm-net-vm")
	return host, vm, nil
}
