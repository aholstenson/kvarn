package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/dispatch"
)

// A server inside the VM is reachable through the runner rather than through
// the VM's network address, because the runner is the only thing that can see
// the guest's own loopback — which is where a published container port and a
// dev server bound to localhost both live. One path serves every case: a server
// listening on all interfaces answers on loopback too.

// GuestDialer opens a connection to a port inside the guest. It is separate
// from RunnerProxy because only the bridge-backed proxy can do it: the bytes
// travel on streams the runner opens back to the bridge, which a proxy not
// attached to a live bridge has no way to receive.
type GuestDialer interface {
	DialGuest(ctx context.Context, port uint16) (net.Conn, error)
}

// DialGuest opens a TCP connection to a port inside the guest, carried over the
// bridge by the runner. The returned connection is live once the guest socket
// is open, so a caller that gets no error knows something is listening.
func (p *BridgeProxy) DialGuest(ctx context.Context, port uint16) (net.Conn, error) {
	connectionID, err := p.generateTransferID()
	if err != nil {
		return nil, err
	}

	// Two pipes, one per direction. They are unbuffered, so a slow end of the
	// connection slows the other rather than growing memory per connection.
	toGuestR, toGuestW := io.Pipe()
	fromGuestR, fromGuestW := io.Pipe()

	// Register before dialling: the runner opens both streams as soon as its
	// socket is up, which can be before the dial command's result gets back.
	p.runner.RegisterConn(connectionID, &dispatch.PendingConn{
		Reader: toGuestR,
		Writer: fromGuestW,
	})

	cleanup := func() {
		p.runner.RemoveConn(connectionID)
		toGuestR.Close()
		toGuestW.Close()
		fromGuestR.Close()
		fromGuestW.Close()
	}

	if _, err := p.sendAndWait(ctx, &v1.RunnerCommand{
		CommandId: p.nextCommandID(),
		Command: &v1.RunnerCommand_DialConnection{DialConnection: &v1.DialConnectionCommand{
			ConnectionId: connectionID,
			Port:         uint32(port),
		}},
	}); err != nil {
		cleanup()
		return nil, err
	}

	return &guestConn{
		port:      port,
		toGuest:   toGuestW,
		fromGuest: fromGuestR,
		close: func() {
			p.runner.RemoveConn(connectionID)
			toGuestW.Close()
			fromGuestR.Close()
		},
	}, nil
}

// guestConn is the host end of a connection the runner holds open inside the
// guest. Writes go to the runner, reads come back from it.
type guestConn struct {
	port      uint16
	toGuest   *io.PipeWriter
	fromGuest *io.PipeReader
	close     func()
	once      sync.Once
}

func (c *guestConn) Read(b []byte) (int, error) { return c.fromGuest.Read(b) }

func (c *guestConn) Write(b []byte) (int, error) { return c.toGuest.Write(b) }

// CloseWrite ends the outgoing direction only, so a client that has finished
// sending still gets the answer. The runner turns it into a half-close on the
// guest socket.
func (c *guestConn) CloseWrite() error { return c.toGuest.Close() }

func (c *guestConn) Close() error {
	c.once.Do(c.close)
	return nil
}

func (c *guestConn) LocalAddr() net.Addr  { return guestAddr{port: 0} }
func (c *guestConn) RemoteAddr() net.Addr { return guestAddr{port: c.port} }

// Deadlines are not offered: the connection is a pair of pipes rather than a
// socket, and every caller bounds its work with a context instead.
func (c *guestConn) SetDeadline(time.Time) error      { return errors.ErrUnsupported }
func (c *guestConn) SetReadDeadline(time.Time) error  { return errors.ErrUnsupported }
func (c *guestConn) SetWriteDeadline(time.Time) error { return errors.ErrUnsupported }

// guestAddr names the far end of a proxied connection for anything that logs it.
type guestAddr struct{ port uint16 }

func (guestAddr) Network() string  { return "kvarn-guest" }
func (a guestAddr) String() string { return fmt.Sprintf("guest:%d", a.port) }
