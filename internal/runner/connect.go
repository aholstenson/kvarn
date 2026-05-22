package runner

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"

	"errors"
	"fmt"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
)

const streamChunkSize = 512 * 1024 // 512KB

func connectToOrchestrator(ctx context.Context, httpClient *http.Client, addr string, token string) error {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	client := kvarnv1connect.NewBridgeServiceClient(httpClient, addr)

	// Retry registration — the vsock modules or bridge server may not be ready
	// immediately after boot.
	var stream *connect.ServerStreamForClient[v1.RunnerCommand]
	for attempt := range 20 {
		var err error
		stream, err = client.Register(ctx, connect.NewRequest(&v1.RegisterRequest{
			Token:  token,
			VmInfo: gatherVmInfo(),
		}))
		if err == nil {
			break
		}
		slog.Warn("failed to register with orchestrator, retrying", "attempt", attempt+1, "error", err)
		select {
		case <-ctx.Done():
			return fmt.Errorf("register with orchestrator: %w", ctx.Err())
		case <-time.After(time.Second):
		}
	}
	if stream == nil {
		return errors.New("register with orchestrator: failed after retries")
	}
	defer stream.Close()

	h := NewHandler()
	defer h.Close()

	for stream.Receive() {
		cmd := stream.Msg()
		slog.Info("received command", "command_id", cmd.CommandId)

		result := &v1.CommandResult{
			CommandId: cmd.CommandId,
			Token:     token,
		}

		switch c := cmd.Command.(type) {
		case *v1.RunnerCommand_Exec:
			resp, execErr := h.Exec(ctx, connect.NewRequest(c.Exec))
			if execErr != nil {
				result.Error = execErr.Error()
			} else {
				result.Result = &v1.CommandResult_Exec{Exec: resp.Msg}
			}
		case *v1.RunnerCommand_UploadFiles:
			resp, execErr := h.UploadFiles(ctx, connect.NewRequest(c.UploadFiles))
			if execErr != nil {
				result.Error = execErr.Error()
			} else {
				result.Result = &v1.CommandResult_UploadFiles{UploadFiles: resp.Msg}
			}
		case *v1.RunnerCommand_ReadFile:
			resp, execErr := h.ReadFile(ctx, connect.NewRequest(c.ReadFile))
			if execErr != nil {
				result.Error = execErr.Error()
			} else {
				result.Result = &v1.CommandResult_ReadFile{ReadFile: resp.Msg}
			}
		case *v1.RunnerCommand_EditFile:
			resp, execErr := h.EditFile(ctx, connect.NewRequest(c.EditFile))
			if execErr != nil {
				result.Error = execErr.Error()
			} else {
				result.Result = &v1.CommandResult_EditFile{EditFile: resp.Msg}
			}
		case *v1.RunnerCommand_WriteFile:
			resp, execErr := h.WriteFile(ctx, connect.NewRequest(c.WriteFile))
			if execErr != nil {
				result.Error = execErr.Error()
			} else {
				result.Result = &v1.CommandResult_WriteFile{WriteFile: resp.Msg}
			}
		case *v1.RunnerCommand_CreateSession:
			resp, execErr := h.CreateSession(ctx, connect.NewRequest(c.CreateSession))
			if execErr != nil {
				result.Error = execErr.Error()
			} else {
				result.Result = &v1.CommandResult_CreateSession{CreateSession: resp.Msg}
			}
		case *v1.RunnerCommand_SessionExec:
			onOutput := func(stdout, stderr string) {
				client.ReportOutput(ctx, connect.NewRequest(&v1.OutputChunk{
					CommandId: cmd.CommandId,
					Token:     token,
					Stdout:    stdout,
					Stderr:    stderr,
				}))
			}
			resp, execErr := h.SessionExecWithOutput(ctx, c.SessionExec, onOutput)
			if execErr != nil {
				result.Error = execErr.Error()
			} else {
				result.Result = &v1.CommandResult_SessionExec{SessionExec: resp.Msg}
			}
		case *v1.RunnerCommand_CloseSession:
			resp, execErr := h.CloseSession(ctx, connect.NewRequest(c.CloseSession))
			if execErr != nil {
				result.Error = execErr.Error()
			} else {
				result.Result = &v1.CommandResult_CloseSession{CloseSession: resp.Msg}
			}
		case *v1.RunnerCommand_DownloadFile:
			written, execErr := handleDownloadFile(ctx, client, token, c.DownloadFile)
			if execErr != nil {
				result.Error = execErr.Error()
			} else {
				result.Result = &v1.CommandResult_DownloadFileResult{DownloadFileResult: &v1.FileStreamResult{BytesWritten: written}}
			}
		case *v1.RunnerCommand_UploadFile:
			written, execErr := handleUploadFile(ctx, client, token, c.UploadFile)
			if execErr != nil {
				result.Error = execErr.Error()
			} else {
				result.Result = &v1.CommandResult_UploadFileResult{UploadFileResult: &v1.FileStreamResult{BytesWritten: written}}
			}
		default:
			result.Error = "unknown command type"
		}

		if _, err := client.ReportResult(ctx, connect.NewRequest(result)); err != nil {
			return fmt.Errorf("report result for command %s: %w", cmd.CommandId, err)
		}
	}

	if err := stream.Err(); err != nil {
		return fmt.Errorf("command stream error: %w", err)
	}

	return nil
}

// handleDownloadFile calls DownloadFile on the orchestrator and writes the
// streamed data to a local file.
func handleDownloadFile(ctx context.Context, client kvarnv1connect.BridgeServiceClient, token string, cmd *v1.DownloadFileCommand) (int64, error) {
	stream, err := client.DownloadFile(ctx, connect.NewRequest(&v1.DownloadFileRequest{
		TransferId: cmd.TransferId,
		Token:      token,
	}))
	if err != nil {
		return 0, fmt.Errorf("call DownloadFile: %w", err)
	}
	defer stream.Close()

	// First chunk must be start metadata.
	if !stream.Receive() {
		if err := stream.Err(); err != nil {
			return 0, err
		}
		return 0, errors.New("empty download stream")
	}

	start := stream.Msg().GetStart()
	if start == nil {
		return 0, errors.New("first chunk must be start metadata")
	}

	destPath := cmd.Path
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return 0, fmt.Errorf("create parent dirs: %w", err)
	}

	mode := os.FileMode(0o644)
	if start.Mode != 0 {
		mode = os.FileMode(start.Mode)
	}

	f, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return 0, fmt.Errorf("create destination file: %w", err)
	}
	defer f.Close()

	var total int64
	for stream.Receive() {
		data := stream.Msg().GetData()
		if data == nil {
			continue
		}
		n, writeErr := f.Write(data)
		if writeErr != nil {
			return total, fmt.Errorf("write file: %w", writeErr)
		}
		total += int64(n)
	}
	if err := stream.Err(); err != nil {
		return total, err
	}

	// Fix ownership for files under /home/kvarn.
	chownToKvarn(destPath)

	return total, nil
}

// handleUploadFile reads a local file and streams it to the orchestrator via UploadFile.
func handleUploadFile(ctx context.Context, client kvarnv1connect.BridgeServiceClient, token string, cmd *v1.UploadFileCommand) (int64, error) {
	f, err := os.Open(cmd.Path)
	if err != nil {
		return 0, fmt.Errorf("open source file: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat source file: %w", err)
	}

	uploadStream := client.UploadFile(ctx)

	// Ensure the stream is always closed, even on early errors.
	var streamClosed bool
	defer func() {
		if !streamClosed {
			uploadStream.CloseAndReceive()
		}
	}()

	// Send start metadata.
	if err := uploadStream.Send(&v1.FileStreamChunk{
		Payload: &v1.FileStreamChunk_Start{Start: &v1.FileStreamStart{
			TransferId: cmd.TransferId,
			Token:      token,
			Path:       cmd.Path,
			Size:       info.Size(),
			Mode:       uint32(info.Mode().Perm()),
		}},
	}); err != nil {
		return 0, fmt.Errorf("send start metadata: %w", err)
	}

	// Stream file data in chunks.
	buf := make([]byte, streamChunkSize)
	var total int64
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			if err := uploadStream.Send(&v1.FileStreamChunk{
				Payload: &v1.FileStreamChunk_Data{Data: chunk},
			}); err != nil {
				return total, fmt.Errorf("send data chunk: %w", err)
			}
			total += int64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return total, fmt.Errorf("read source file: %w", readErr)
		}
	}

	streamClosed = true
	resp, err := uploadStream.CloseAndReceive()
	if err != nil {
		return total, fmt.Errorf("close upload stream: %w", err)
	}

	return resp.Msg.BytesWritten, nil
}

// chownToKvarn changes file ownership to the kvarn user if it exists.
func chownToKvarn(path string) {
	u, err := user.Lookup("kvarn")
	if err != nil {
		return
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	os.Chown(path, uid, gid)
}
