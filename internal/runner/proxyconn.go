package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
)

// Preview traffic has to reach servers the guest itself can see but nothing
// outside it can: a container published on 127.0.0.1, a dev server bound to
// localhost. The runner is inside the VM, so it dials on the caller's behalf
// and carries the bytes over the bridge it is already connected to.

// connDialTimeout bounds one attempt to reach a guest address. It is short
// because both candidate addresses are on this machine: a server that is
// listening answers immediately, and one that is not fails immediately too.
const connDialTimeout = 5 * time.Second

// connChunkSize bounds one message of proxied data.
const connChunkSize = 32 * 1024

// dialGuestPort opens a connection to port from inside the guest. Loopback is
// tried first because it is where a published container port and a dev server
// most often live, and a server bound to all interfaces answers there too; the
// VM's own address is the fallback for the rarer server that binds only it.
func dialGuestPort(ctx context.Context, port uint32) (net.Conn, string, error) {
	var errs []error
	for _, host := range dialCandidates() {
		addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
		dialCtx, cancel := context.WithTimeout(ctx, connDialTimeout)
		var d net.Dialer
		conn, err := d.DialContext(dialCtx, "tcp", addr)
		cancel()
		if err == nil {
			return conn, addr, nil
		}
		errs = append(errs, err)
	}
	return nil, "", fmt.Errorf("dial port %d in the guest: %w", port, errors.Join(errs...))
}

// dialCandidates lists the guest addresses a server may be listening on, in the
// order they are tried.
func dialCandidates() []string {
	hosts := []string{"127.0.0.1"}

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return hosts
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() {
			continue
		}
		if ip4 := ipnet.IP.To4(); ip4 != nil {
			hosts = append(hosts, ip4.String())
		}
	}
	return hosts
}

// handleDialConnection opens the guest connection and, once it is up, starts
// the two streams that carry it. It returns as soon as the connection exists so
// the caller learns immediately whether anything is listening — the pumps run
// on beyond it, for as long as the connection lives.
func handleDialConnection(
	ctx context.Context,
	client kvarnv1connect.BridgeServiceClient,
	token string,
	cmd *v1.DialConnectionCommand,
) (*v1.DialConnectionResult, error) {
	conn, addr, err := dialGuestPort(ctx, cmd.Port)
	if err != nil {
		return nil, err
	}

	go proxyConnection(ctx, client, token, cmd.ConnectionId, conn)

	return &v1.DialConnectionResult{Address: addr}, nil
}

// proxyConnection splices one guest connection into its pair of bridge streams
// and closes it once both directions are done.
func proxyConnection(
	ctx context.Context,
	client kvarnv1connect.BridgeServiceClient,
	token string,
	connectionID string,
	conn net.Conn,
) {
	defer conn.Close()

	done := make(chan struct{}, 2)

	go func() {
		defer func() { done <- struct{}{} }()
		if err := pumpToGuest(ctx, client, token, connectionID, conn); err != nil {
			slog.Debug("proxied connection inbound ended", "connection_id", connectionID, "error", err)
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		if err := pumpFromGuest(ctx, client, token, connectionID, conn); err != nil {
			slog.Debug("proxied connection outbound ended", "connection_id", connectionID, "error", err)
		}
	}()

	<-done
	<-done
}

// pumpToGuest drains what the client sent into the guest socket, then
// half-closes it so a server waiting for the end of a request sees it.
func pumpToGuest(
	ctx context.Context,
	client kvarnv1connect.BridgeServiceClient,
	token string,
	connectionID string,
	conn net.Conn,
) error {
	stream, err := client.ReadConnection(ctx, connect.NewRequest(&v1.ReadConnectionRequest{
		ConnectionId: connectionID,
		Token:        token,
	}))
	if err != nil {
		return fmt.Errorf("open inbound stream: %w", err)
	}
	defer stream.Close()

	for stream.Receive() {
		data := stream.Msg().GetData()
		if len(data) == 0 {
			continue
		}
		if _, err := conn.Write(data); err != nil {
			return fmt.Errorf("write to guest: %w", err)
		}
	}
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
	return stream.Err()
}

// pumpFromGuest streams what the guest server answers back out to the client.
func pumpFromGuest(
	ctx context.Context,
	client kvarnv1connect.BridgeServiceClient,
	token string,
	connectionID string,
	conn net.Conn,
) error {
	stream := client.WriteConnection(ctx)

	if err := stream.Send(&v1.ConnectionChunk{
		Payload: &v1.ConnectionChunk_Start{Start: &v1.ConnectionStreamStart{
			ConnectionId: connectionID,
			Token:        token,
		}},
	}); err != nil {
		stream.CloseAndReceive()
		return fmt.Errorf("open outbound stream: %w", err)
	}

	buf := make([]byte, connChunkSize)
	for {
		n, readErr := conn.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			if err := stream.Send(&v1.ConnectionChunk{
				Payload: &v1.ConnectionChunk_Data{Data: chunk},
			}); err != nil {
				stream.CloseAndReceive()
				return fmt.Errorf("send guest data: %w", err)
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				stream.CloseAndReceive()
				return fmt.Errorf("read from guest: %w", readErr)
			}
			break
		}
	}

	if _, err := stream.CloseAndReceive(); err != nil {
		return fmt.Errorf("close outbound stream: %w", err)
	}
	return nil
}
