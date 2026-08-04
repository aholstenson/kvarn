package startjob

import (
	"context"
	"fmt"
	"os"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/cmd/client"
)

type Cmd struct {
	Addr    string `help:"Orchestrator address." default:"http://localhost:8080"`
	Project string `arg:"" help:"Project name."`
	Prompt  string `arg:"" help:"Prompt for the agent."`
	Branch  string `help:"Branch override." default:""`
	Mode    string `help:"Agent mode: auto, implement, fix, feedback, review, research." default:"auto"`
	Watch   bool   `help:"Stream the session until it reaches a terminal state." negatable:""`
	APIKey  string `help:"API key for the orchestrator." env:"KVARN_API_KEY" default:""`
}

func (c *Cmd) Run() error {
	oc := client.NewOrchestrator(c.Addr, c.APIKey)

	resp, err := oc.StartJob(context.Background(), connect.NewRequest(&v1.StartJobRequest{
		Project: c.Project,
		Prompt:  c.Prompt,
		Branch:  c.Branch,
		Mode:    c.Mode,
	}))
	if err != nil {
		return fmt.Errorf("start job: %w", err)
	}

	sessionID := resp.Msg.SessionId
	fmt.Fprintf(os.Stdout, "Session: %s\n", sessionID)

	if !c.Watch {
		return nil
	}

	return client.WatchSession(context.Background(), oc, sessionID)
}
