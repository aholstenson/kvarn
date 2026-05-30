package dispatch_test

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/mdlayher/vsock"

	"github.com/aholstenson/kvarn/internal/dispatch"
)

var _ = Describe("WrapListener", func() {
	It("passes through peers with the expected CID", func() {
		inner := newScriptedListener(
			&fakeVsockConn{remote: &vsock.Addr{ContextID: 42, Port: 1}},
		)
		l := dispatch.WrapListener(inner, 42)

		conn, err := l.Accept()
		Expect(err).NotTo(HaveOccurred())
		Expect(conn).NotTo(BeNil())
		conn.Close()
	})

	It("drops peers with a mismatched CID and keeps accepting", func() {
		bad := &fakeVsockConn{remote: &vsock.Addr{ContextID: 7, Port: 1}}
		good := &fakeVsockConn{remote: &vsock.Addr{ContextID: 42, Port: 1}}
		inner := newScriptedListener(bad, good)
		l := dispatch.WrapListener(inner, 42)

		conn, err := l.Accept()
		Expect(err).NotTo(HaveOccurred())
		Expect(conn.(*fakeVsockConn)).To(BeIdenticalTo(good))
		Expect(bad.closed.Load()).To(BeTrue())
		conn.Close()
	})

	It("in TOFU mode locks onto the first observed peer CID", func() {
		first := &fakeVsockConn{remote: &vsock.Addr{ContextID: 9, Port: 1}}
		stranger := &fakeVsockConn{remote: &vsock.Addr{ContextID: 11, Port: 1}}
		second := &fakeVsockConn{remote: &vsock.Addr{ContextID: 9, Port: 1}}
		inner := newScriptedListener(first, stranger, second)
		l := dispatch.WrapListener(inner, 0)

		c1, err := l.Accept()
		Expect(err).NotTo(HaveOccurred())
		Expect(c1.(*fakeVsockConn)).To(BeIdenticalTo(first))

		// The stranger is silently dropped; the next Accept returns the
		// matching second peer instead.
		c2, err := l.Accept()
		Expect(err).NotTo(HaveOccurred())
		Expect(c2.(*fakeVsockConn)).To(BeIdenticalTo(second))
		Expect(stranger.closed.Load()).To(BeTrue())

		c1.Close()
		c2.Close()
	})

	It("accepts connections whose peer CID cannot be determined", func() {
		// A non-vsock conn (e.g. TCP) returns ok=false from PeerCIDFromConn,
		// which the wrapper treats as 'cannot enforce' rather than reject.
		opaque := &fakeOpaqueConn{}
		inner := newScriptedListener(opaque)
		l := dispatch.WrapListener(inner, 42)

		conn, err := l.Accept()
		Expect(err).NotTo(HaveOccurred())
		Expect(conn.(*fakeOpaqueConn)).To(BeIdenticalTo(opaque))
		Expect(opaque.closed.Load()).To(BeFalse())
		conn.Close()
	})

	It("propagates Accept errors from the inner listener", func() {
		inner := &errListener{err: errors.New("boom")}
		l := dispatch.WrapListener(inner, 42)

		_, err := l.Accept()
		Expect(err).To(MatchError("boom"))
	})
})

// --- helpers ---

// scriptedListener returns a fixed sequence of net.Conns from successive
// Accept calls, then blocks forever on subsequent calls so the test goroutine
// doesn't busy-loop while we observe state.
type scriptedListener struct {
	mu    sync.Mutex
	queue []net.Conn
	done  chan struct{}
}

func newScriptedListener(conns ...net.Conn) *scriptedListener {
	return &scriptedListener{queue: conns, done: make(chan struct{})}
}

func (s *scriptedListener) Accept() (net.Conn, error) {
	s.mu.Lock()
	if len(s.queue) > 0 {
		conn := s.queue[0]
		s.queue = s.queue[1:]
		s.mu.Unlock()
		return conn, nil
	}
	s.mu.Unlock()
	<-s.done
	return nil, net.ErrClosed
}

func (s *scriptedListener) Close() error   { close(s.done); return nil }
func (s *scriptedListener) Addr() net.Addr { return &fakeAddr{} }

type errListener struct{ err error }

func (e *errListener) Accept() (net.Conn, error) { return nil, e.err }
func (e *errListener) Close() error              { return nil }
func (e *errListener) Addr() net.Addr            { return &fakeAddr{} }

type fakeAddr struct{}

func (fakeAddr) Network() string { return "fake" }
func (fakeAddr) String() string  { return "fake" }

type fakeVsockConn struct {
	remote *vsock.Addr
	closed atomic.Bool
}

func (f *fakeVsockConn) Read(_ []byte) (int, error)         { return 0, errors.New("nope") }
func (f *fakeVsockConn) Write(_ []byte) (int, error)        { return 0, errors.New("nope") }
func (f *fakeVsockConn) Close() error                       { f.closed.Store(true); return nil }
func (f *fakeVsockConn) LocalAddr() net.Addr                { return &vsock.Addr{ContextID: 2, Port: 1024} }
func (f *fakeVsockConn) RemoteAddr() net.Addr               { return f.remote }
func (f *fakeVsockConn) SetDeadline(time.Time) error        { return nil }
func (f *fakeVsockConn) SetReadDeadline(time.Time) error    { return nil }
func (f *fakeVsockConn) SetWriteDeadline(time.Time) error   { return nil }

type fakeOpaqueConn struct {
	closed atomic.Bool
}

func (f *fakeOpaqueConn) Read(_ []byte) (int, error)       { return 0, errors.New("nope") }
func (f *fakeOpaqueConn) Write(_ []byte) (int, error)      { return 0, errors.New("nope") }
func (f *fakeOpaqueConn) Close() error                     { f.closed.Store(true); return nil }
func (f *fakeOpaqueConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (f *fakeOpaqueConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (f *fakeOpaqueConn) SetDeadline(time.Time) error      { return nil }
func (f *fakeOpaqueConn) SetReadDeadline(time.Time) error  { return nil }
func (f *fakeOpaqueConn) SetWriteDeadline(time.Time) error { return nil }
