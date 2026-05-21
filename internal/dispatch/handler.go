package dispatch

import (
	"context"
	"io"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
	"github.com/cockroachdb/errors"
)

const streamChunkSize = 512 * 1024 // 512KB

// Handler implements kvarnv1connect.BridgeServiceHandler by dispatching
// Register and ReportResult calls through a Registry.
type Handler struct {
	kvarnv1connect.UnimplementedBridgeServiceHandler
	registry *Registry
}

// NewHandler creates a Handler backed by the given registry.
func NewHandler(registry *Registry) *Handler {
	return &Handler{registry: registry}
}

// Register implements BridgeServiceHandler. The runner calls this to receive commands.
func (h *Handler) Register(ctx context.Context, req *connect.Request[v1.RegisterRequest], stream *connect.ServerStream[v1.RunnerCommand]) error {
	token := req.Msg.Token
	pr, ok := h.registry.Lookup(token)
	if !ok {
		return connect.NewError(connect.CodeNotFound, errors.New("unknown token"))
	}

	// Store VM info from the runner.
	pr.VmInfo = req.Msg.VmInfo

	// Signal that the runner is connected.
	pr.MarkReady()

	// Stream commands to the runner until context is done.
	for {
		select {
		case cmd := <-pr.CommandCh:
			if err := stream.Send(cmd); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// ReportResult implements BridgeServiceHandler. The runner calls this to return results.
func (h *Handler) ReportResult(_ context.Context, req *connect.Request[v1.CommandResult]) (*connect.Response[v1.ReportResultResponse], error) {
	token := req.Msg.Token
	pr, ok := h.registry.Lookup(token)
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("unknown token"))
	}

	pr.ResultCh <- req.Msg

	return connect.NewResponse(&v1.ReportResultResponse{}), nil
}

// ReportOutput implements BridgeServiceHandler. The runner calls this to stream output chunks.
func (h *Handler) ReportOutput(_ context.Context, req *connect.Request[v1.OutputChunk]) (*connect.Response[v1.ReportOutputResponse], error) {
	token := req.Msg.Token
	pr, ok := h.registry.Lookup(token)
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("unknown token"))
	}

	// Non-blocking send to avoid blocking the runner on slow consumers.
	select {
	case pr.OutputCh <- req.Msg:
	default:
	}

	return connect.NewResponse(&v1.ReportOutputResponse{}), nil
}

// DownloadFile implements BridgeServiceHandler. The runner calls this to
// download a file from the orchestrator as a server-streamed sequence of chunks.
func (h *Handler) DownloadFile(_ context.Context, req *connect.Request[v1.DownloadFileRequest], stream *connect.ServerStream[v1.FileStreamChunk]) error {
	pr, ok := h.registry.Lookup(req.Msg.Token)
	if !ok {
		return connect.NewError(connect.CodeNotFound, errors.New("unknown token"))
	}

	t, ok := pr.LookupTransfer(req.Msg.TransferId)
	if !ok {
		return connect.NewError(connect.CodeNotFound, errors.New("unknown transfer"))
	}
	defer func() {
		t.Reader.Close()
		pr.RemoveTransfer(req.Msg.TransferId)
		close(t.Done)
	}()

	// Send metadata as the first chunk.
	if err := stream.Send(&v1.FileStreamChunk{
		Payload: &v1.FileStreamChunk_Start{Start: t.Meta},
	}); err != nil {
		return err
	}

	// Stream data in fixed-size chunks.
	buf := make([]byte, streamChunkSize)
	var total int64
	for {
		n, readErr := t.Reader.Read(buf)
		if n > 0 {
			total += int64(n)
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			if err := stream.Send(&v1.FileStreamChunk{
				Payload: &v1.FileStreamChunk_Data{Data: chunk},
			}); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return errors.Wrap(readErr, "read transfer data")
		}
	}

	return nil
}

// UploadFile implements BridgeServiceHandler. The runner calls this to
// upload a file to the orchestrator as a client-streamed sequence of chunks.
func (h *Handler) UploadFile(_ context.Context, clientStream *connect.ClientStream[v1.FileStreamChunk]) (*connect.Response[v1.FileStreamResult], error) {
	// First message must be the start metadata.
	if !clientStream.Receive() {
		if err := clientStream.Err(); err != nil {
			return nil, err
		}
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("empty stream"))
	}

	first := clientStream.Msg()
	start := first.GetStart()
	if start == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("first chunk must be start metadata"))
	}

	pr, ok := h.registry.Lookup(start.Token)
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("unknown token"))
	}

	t, ok := pr.LookupTransfer(start.TransferId)
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("unknown transfer"))
	}
	var writeErr error
	defer func() {
		pr.RemoveTransfer(start.TransferId)
		// Use CloseWithError if available (e.g. io.PipeWriter) to propagate
		// errors to the reader side; fall back to plain Close.
		if pwc, ok := t.Writer.(interface{ CloseWithError(error) error }); ok && writeErr != nil {
			pwc.CloseWithError(writeErr)
		} else {
			t.Writer.Close()
		}
		close(t.Done)
	}()

	// Receive data chunks and write to the transfer writer.
	var total int64
	for clientStream.Receive() {
		data := clientStream.Msg().GetData()
		if data == nil {
			continue
		}
		n, err := t.Writer.Write(data)
		if err != nil {
			writeErr = err
			return nil, errors.Wrap(err, "write transfer data")
		}
		total += int64(n)
	}
	if err := clientStream.Err(); err != nil {
		writeErr = err
		return nil, err
	}

	return connect.NewResponse(&v1.FileStreamResult{
		BytesWritten: total,
	}), nil
}
