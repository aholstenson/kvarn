package jobs

import (
	"context"
	"fmt"
	"os"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/cmd/client"
)

// StartCmd submits a new project-aware job to the orchestrator.
type StartCmd struct {
	connectFlags

	Project string `arg:"" help:"Project name."`
	Prompt  string `arg:"" help:"Prompt for the agent."`
	Branch  string `help:"Branch override." default:""`
	Mode    string `help:"Agent mode: auto, implement, fix, feedback, review, research." default:"auto"`
	Watch   bool   `help:"Stream the session until it reaches a terminal state." negatable:""`

	IdempotencyKey string `help:"Key that makes this submission replayable: resending it returns the session the first request created instead of starting a second job." default:""`
}

func (c *StartCmd) Run() error {
	ctx := context.Background()
	oc := c.client()

	resp, err := oc.StartJob(ctx, connect.NewRequest(&v1.StartJobRequest{
		Project: c.Project,
		Prompt:  c.Prompt,
		Branch:  c.Branch,
		Mode:    c.Mode,

		IdempotencyKey: c.IdempotencyKey,
	}))
	if err != nil {
		return fmt.Errorf("start job: %w", err)
	}

	sessionID := resp.Msg.SessionId
	fmt.Fprintf(os.Stdout, "Session: %s\n", sessionID)
	if resp.Msg.Duplicate {
		fmt.Fprintf(os.Stdout, "Idempotency key %q already started this job; no new job was submitted.\n", c.IdempotencyKey)
	}

	if !c.Watch {
		return nil
	}

	return client.WatchSession(ctx, oc, sessionID)
}
