//go:build linux

package auth

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// peerDescription reads the calling process's credentials off a unix socket for
// the audit log. An empty string means the kernel would not tell us, which is
// not a failure: the socket's permissions are what authenticate the caller, and
// this only names them.
func peerDescription(c net.Conn) string {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return ""
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return ""
	}
	var (
		cred    *unix.Ucred
		credErr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil || credErr != nil {
		return ""
	}
	return fmt.Sprintf("uid=%d pid=%d", cred.Uid, cred.Pid)
}
