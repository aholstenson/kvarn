package client

import (
	"context"
	"fmt"
	"os"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
)

// WatchSession streams a session's events to stdout/stderr until it reaches a
// terminal state. Shared by the commands that start work and then follow it.
func WatchSession(ctx context.Context, oc kvarnv1connect.OrchestratorServiceClient, sessionID string) error {
	stream, err := oc.WatchSession(ctx, connect.NewRequest(&v1.WatchSessionRequest{
		SessionId: sessionID,
	}))
	if err != nil {
		return fmt.Errorf("watch session: %w", err)
	}
	defer stream.Close()

	for stream.Receive() {
		update := stream.Msg()
		switch e := update.Event.(type) {
		case *v1.SessionUpdate_StateChange:
			sc := e.StateChange
			if sc.Error != "" {
				fmt.Fprintf(os.Stderr, "[%s] %s: %s\n", sc.State, sc.Message, sc.Error)
			} else {
				fmt.Fprintf(os.Stdout, "[%s] %s\n", sc.State, sc.Message)
			}
		case *v1.SessionUpdate_AgentMessage:
			if e.AgentMessage.Final {
				fmt.Fprintln(os.Stdout, e.AgentMessage.Text)
			}
		case *v1.SessionUpdate_AgentToolUse:
			fmt.Fprintf(os.Stdout, "=> %s %s\n", e.AgentToolUse.ToolId, e.AgentToolUse.ArgumentsJson)
		case *v1.SessionUpdate_AgentToolResult:
			if e.AgentToolResult.IsError {
				fmt.Fprintf(os.Stderr, "   error: %s\n", e.AgentToolResult.Result)
			}
		case *v1.SessionUpdate_PullRequestCreated:
			pr := e.PullRequestCreated
			fmt.Fprintf(os.Stdout, "[pr] %s (%s)\n", pr.Url, pr.Branch)
		case *v1.SessionUpdate_VmInfo:
			vi := e.VmInfo
			fmt.Fprintf(os.Stdout, "[vm] %d cores, %d MB memory, %d/%d MB disk\n",
				vi.CpuCount, vi.MemTotalMb, vi.DiskUsedMb, vi.DiskTotalMb)
		case *v1.SessionUpdate_DependencyOutput:
			do := e.DependencyOutput
			if do.Stdout != "" {
				fmt.Fprintf(os.Stdout, "[deps] %s", do.Stdout)
			}
			if do.Stderr != "" {
				fmt.Fprintf(os.Stderr, "[deps] %s", do.Stderr)
			}
		case *v1.SessionUpdate_CacheProgress:
			cp := e.CacheProgress
			action := "saving"
			if cp.Restoring {
				action = "restoring"
			}
			fmt.Fprintf(os.Stdout, "[cache] %s %s (%d/%d)\n", action, cp.Path, cp.Index, cp.Total)
		}
	}

	if err := stream.Err(); err != nil {
		return fmt.Errorf("watch stream: %w", err)
	}
	return nil
}
