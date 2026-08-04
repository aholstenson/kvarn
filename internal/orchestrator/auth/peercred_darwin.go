//go:build darwin

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
		uid, pid int
		credErr  error
	)
	if err := raw.Control(func(fd uintptr) {
		var xu *unix.Xucred
		xu, credErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if credErr != nil {
			return
		}
		uid = int(xu.Uid)
		// The pid is a separate, best-effort lookup; the uid is what names the
		// account, so a kernel that withholds the pid still gives a usable line.
		pid, _ = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	}); err != nil || credErr != nil {
		return ""
	}
	return fmt.Sprintf("uid=%d pid=%d", uid, pid)
}
