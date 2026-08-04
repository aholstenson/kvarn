//go:build !linux && !darwin

package auth

import "net"

// peerDescription has no portable implementation. The audit line loses the
// peer's uid, not the guarantee that kept other accounts out: the socket's
// permissions are what authenticate the caller everywhere.
func peerDescription(net.Conn) string { return "" }
