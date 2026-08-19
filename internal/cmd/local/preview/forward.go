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

// Port mode is the plain case: no hostnames, so each site gets a loopback port
// and the URL a site is told about is the one a browser on this machine can
// open. Nothing here parses what it carries — a site is a port, and a port is
// spliced through byte for byte, which is why it works for anything the site
// speaks rather than only HTTP. Domain mode is in ingress.go.

// forwarder accepts on a host loopback port and splices each connection into a
// guest port. No routing, no TLS, no holding page, because there is exactly one
// site behind each listener and it is already up by the time anything connects.
type forwarder struct {
	listener  net.Listener
	guestPort uint16
	dial      func(ctx context.Context, port uint16) (net.Conn, error)
	log       *slog.Logger
	// onUnreachable reports the first connection that could not be delivered.
	// Without it the only symptom is a connection reset in the browser, which
	// looks like the preview never bound its port at all.
	onUnreachable func(err error)

	wg     sync.WaitGroup
	closed chan struct{}
	once   sync.Once
	// reported keeps the unreachable notice to one line: a browser opens many
	// connections, and repeating the same diagnosis for each buries everything
	// else.
	reported sync.Once
}

// startForward takes a bound listener and forwards everything it accepts to
// guestPort, until Close.
func startForward(
	ln net.Listener,
	guestPort uint16,
	dial func(context.Context, uint16) (net.Conn, error),
	log *slog.Logger,
	onUnreachable func(err error),
) *forwarder {
	f := &forwarder{
		listener:      ln,
		guestPort:     guestPort,
		dial:          dial,
		log:           log,
		onUnreachable: onUnreachable,
		closed:        make(chan struct{}),
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
// the client connection — that is what a server which is not listening looks
// like — but it is also reported once, since from the outside it is
// indistinguishable from kvarn never having bound the port.
func (f *forwarder) handle(client net.Conn) {
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	guest, err := f.dial(ctx, f.guestPort)
	if err != nil {
		f.log.Debug("preview forward dial failed", "port", f.guestPort, "error", err)
		if f.onUnreachable != nil && !canceled(ctx, err) {
			f.reported.Do(func() { f.onUnreachable(err) })
		}
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
func (f *forwarder) Close() error {
	f.once.Do(func() {
		close(f.closed)
		f.listener.Close()
	})
	f.wg.Wait()
	return nil
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
