package preview

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
)

// A local preview has no hostnames. The orchestrator routes by Host header into
// a VM because it owns a domain and a fronting layer; a developer's machine has
// neither, so each app gets a loopback port instead and the URL an app is told
// about is the one a browser on this machine can actually open.

// forwarder accepts on a host loopback port and splices each connection into a
// guest port. It is the whole of local ingress: no routing, no TLS, no holding
// page, because there is exactly one preview and it is already up by the time
// anything connects.
type forwarder struct {
	listener net.Listener
	guestPort uint16
	dial     func(ctx context.Context, port uint16) (net.Conn, error)
	log      *slog.Logger

	wg     sync.WaitGroup
	closed chan struct{}
	once   sync.Once
}

// startForward takes a bound listener and forwards everything it accepts to
// guestPort, until Close.
func startForward(ln net.Listener, guestPort uint16, dial func(context.Context, uint16) (net.Conn, error), log *slog.Logger) *forwarder {
	f := &forwarder{
		listener:  ln,
		guestPort: guestPort,
		dial:      dial,
		log:       log,
		closed:    make(chan struct{}),
	}
	f.wg.Add(1)
	go f.serve()
	return f
}

// Port is the host port actually bound, which matters when the caller asked for
// port 0 and let the kernel choose.
func (f *forwarder) Port() uint16 {
	return uint16(f.listener.Addr().(*net.TCPAddr).Port)
}

func (f *forwarder) serve() {
	defer f.wg.Done()
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			select {
			case <-f.closed:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			f.log.Debug("preview forward accept failed", "port", f.guestPort, "error", err)
			continue
		}
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			f.handle(conn)
		}()
	}
}

// handle splices one accepted connection into the guest. A dial failure closes
// the connection rather than reporting anything: the client is a browser
// talking HTTP to a server that is not listening, and a connection reset is
// what that actually looks like.
func (f *forwarder) handle(client net.Conn) {
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	guest, err := f.dial(ctx, f.guestPort)
	if err != nil {
		f.log.Debug("preview forward dial failed", "port", f.guestPort, "error", err)
		return
	}
	defer guest.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	// Half-close in each direction as it drains, so a client that has finished
	// sending still gets the response rather than having the whole connection
	// torn down by whichever side finished first.
	go func() {
		defer wg.Done()
		io.Copy(guest, client)
		if cw, ok := guest.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		io.Copy(client, guest)
		if cw, ok := client.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
	}()
	wg.Wait()
}

// Close stops accepting and waits for in-flight connections to finish.
func (f *forwarder) Close() {
	f.once.Do(func() {
		close(f.closed)
		f.listener.Close()
	})
	f.wg.Wait()
}

// bindHostPort binds a loopback port for an app. The guest port is tried first
// so the local URL matches what the app thinks it is serving on, which is what
// makes hardcoded links and dev-server HMR endpoints work. When it is taken —
// most often by the very server this preview replaces, running outside the VM —
// the kernel picks a free one instead.
//
// It returns the listener rather than a number so nothing can take the port in
// between choosing it and serving on it.
func bindHostPort(want uint16) (net.Listener, error) {
	if want != 0 {
		if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", want)); err == nil {
			return ln, nil
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("bind a loopback port: %w", err)
	}
	return ln, nil
}
