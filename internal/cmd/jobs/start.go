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
	Branch  string `help:"Branch to start from; defaults to the project's default branch. Mutually exclusive with --pr-ref." default:""`
	PRRef   string `name:"pr-ref" help:"Continue this pull request instead of opening one: run on its head branch and push a follow-up commit. In the forge's own format (GitHub: the PR number)." default:""`
	Mode    string `help:"Agent mode: auto, implement, fix, feedback, review, research. Defaults to feedback with --pr-ref and auto otherwise." default:""`
	Watch   bool   `help:"Stream the session until it reaches a terminal state." negatable:""`

	IdempotencyKey string `help:"Key that makes this submission replayable: resending it returns the session the first request created instead of starting a second job." default:""`
}

func (c *StartCmd) Run() error {
	ctx := context.Background()
	oc := c.client()

	req := &v1.StartJobRequest{
		Project: c.Project,
		Prompt:  c.Prompt,
		Mode:    c.Mode,

		IdempotencyKey: c.IdempotencyKey,
	}
	// Only one starting point reaches the wire. Leaving both unset is the
	// project's default branch, which is what a caller who named neither meant.
	switch {
	case c.PRRef != "" && c.Branch != "":
		return fmt.Errorf("--branch and --pr-ref are alternatives: a pull request already fixes the branch its commits land on")
	case c.PRRef != "":
		req.StartFrom = &v1.StartJobRequest_PrRef{PrRef: c.PRRef}
	case c.Branch != "":
		req.StartFrom = &v1.StartJobRequest_Branch{Branch: c.Branch}
	}

	resp, err := oc.StartJob(ctx, connect.NewRequest(req))
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
