package startjob

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
	"github.com/cockroachdb/errors"
)

type Cmd struct {
	Addr    string `help:"Orchestrator address." default:"http://localhost:8080"`
	Project string `arg:"" help:"Project name."`
	Prompt  string `arg:"" help:"Prompt for the agent."`
	Branch  string `help:"Branch override." default:""`
	Mode    string `help:"Agent mode: auto, implement, fix, review, research." default:"auto"`
	Watch   bool   `help:"Watch session until completion." default:"true"`
}

func (c *Cmd) Run() error {
	client := kvarnv1connect.NewOrchestratorServiceClient(http.DefaultClient, c.Addr)

	resp, err := client.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
		Project: c.Project,
		Prompt:  c.Prompt,
		Branch:  c.Branch,
		Mode:    c.Mode,
	}))
	if err != nil {
		return errors.Wrap(err, "start job")
	}

	sessionID := resp.Msg.SessionId
	fmt.Fprintf(os.Stdout, "Session: %s\n", sessionID)

	if !c.Watch {
		return nil
	}

	stream, err := client.WatchSession(context.Background(), connect.NewRequest(&v1.WatchSessionRequest{
		SessionId: sessionID,
	}))
	if err != nil {
		return errors.Wrap(err, "watch session")
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
		return errors.Wrap(err, "watch stream")
	}

	return nil
}
