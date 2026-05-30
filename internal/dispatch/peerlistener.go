package dispatch

import (
	"log/slog"
	"net"
	"sync"

	"github.com/mdlayher/vsock"
)

// PeerCIDFromConn returns the vsock context-ID of the connection's remote
// peer. The second return value is false when the connection is not a vsock
// connection we recognise (e.g. vz's VirtioSocketConnection on macOS, where
// the underlying file descriptor doesn't surface a typed vsock address).
func PeerCIDFromConn(conn net.Conn) (uint32, bool) {
	addr := conn.RemoteAddr()
	if addr == nil {
		return 0, false
	}
	if va, ok := addr.(*vsock.Addr); ok {
		return va.ContextID, true
	}
	return 0, false
}

// WrapListener returns a listener that drops any accepted connection whose
// peer vsock CID does not match expected. When expected is zero the wrapper
// switches to trust-on-first-use: it records the first observed peer CID and
// rejects later connections from any other CID. Connections whose peer CID
// cannot be determined (e.g. macOS vz transport) are accepted as-is, because
// only one VM is reachable on the wrapped listener in that case anyway.
func WrapListener(inner net.Listener, expected uint32) net.Listener {
	return &peerListener{
		Listener: inner,
		expected: expected,
		tofu:     expected == 0,
	}
}

type peerListener struct {
	net.Listener

	expected uint32

	mu     sync.Mutex
	tofu   bool   // when true, lock onto the first observed peer CID
	locked uint32 // set once tofu has captured a peer CID
	tofuOK bool
}

func (l *peerListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		cid, ok := PeerCIDFromConn(conn)
		if !ok {
			// Peer identity unavailable on this transport; nothing useful to
			// enforce here. The bridge service still relies on Layer 4
			// (per-RPC token + Register binding) for impersonation defense.
			return conn, nil
		}
		if l.allow(cid) {
			return conn, nil
		}
		slog.Warn("rejecting bridge connection from unexpected peer", "cid", cid, "expected", l.expectedCID())
		conn.Close()
	}
}

func (l *peerListener) allow(cid uint32) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.tofu {
		if !l.tofuOK {
			l.locked = cid
			l.tofuOK = true
			return true
		}
		return cid == l.locked
	}
	return cid == l.expected
}

func (l *peerListener) expectedCID() uint32 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.tofu {
		if l.tofuOK {
			return l.locked
		}
		return 0
	}
	return l.expected
}
