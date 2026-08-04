package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"text/tabwriter"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/cmd/client"
)

// CancelCmd stops one job by id, or every job matching a filter. The two forms
// are one command because they are one intent — stop this work — and which one
// applies is decided by whether the caller can name the session.
type CancelCmd struct {
	connectFlags

	SessionID string `arg:"" name:"session-id" optional:"" help:"Session to cancel. Omit to cancel by filter instead."`
	Reason    string `help:"Reason to record on the cancelled sessions."`

	Project string   `help:"Cancel jobs for this project."`
	State   []string `help:"Cancel jobs in these states (repeatable, comma-separated)." placeholder:"STATE"`
	Mode    string   `help:"Cancel jobs run in this agent mode."`
	PRRef   string   `help:"Cancel jobs working on this pull request." name:"pr-ref"`
	All     bool     `help:"Required to cancel every active job when no other filter is given."`
	Limit   int      `help:"Maximum jobs to cancel in one sweep." default:"0"`
	DryRun  bool     `help:"List what would be cancelled without cancelling it." name:"dry-run"`
	JSON    bool     `help:"Emit JSON instead of a table." name:"json"`
}

func (c *CancelCmd) Run() error {
	ctx := context.Background()
	oc := c.client()

	if c.SessionID != "" {
		if c.Project != "" || len(c.State) > 0 || c.Mode != "" || c.PRRef != "" || c.All {
			return errors.New("cancel takes either a session id or filter flags, not both")
		}
		if c.DryRun {
			return errors.New("--dry-run applies to a filtered cancel, not to a single session")
		}
		resp, err := oc.CancelJob(ctx, connect.NewRequest(&v1.CancelJobRequest{
			SessionId: c.SessionID,
			Reason:    c.Reason,
		}))
		if err != nil {
			return fmt.Errorf("cancel job: %w", err)
		}
		// The orchestrator returns once the stop is signalled; the run still has
		// to unwind and tear its VM down before the session reaches `cancelled`.
		fmt.Fprintf(os.Stdout, "Cancelling %s (was %s)\n", c.SessionID, resp.Msg.PreviousState)
		return nil
	}

	resp, err := oc.CancelJobs(ctx, connect.NewRequest(&v1.CancelJobsRequest{
		Project: c.Project,
		States:  c.State,
		Mode:    c.Mode,
		PrRef:   c.PRRef,
		Reason:  c.Reason,
		DryRun:  c.DryRun,
		All:     c.All,
		Limit:   int32(c.Limit),
	}))
	if err != nil {
		return fmt.Errorf("cancel jobs: %w", err)
	}

	if c.JSON {
		return PrintJSON(resp.Msg)
	}

	jobs := resp.Msg.Jobs
	if len(jobs) == 0 {
		fmt.Fprintln(os.Stdout, "No matching jobs")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tPROJECT\tWAS\tRESULT")
	for _, j := range jobs {
		result := "cancelling"
		if c.DryRun {
			result = "would cancel"
		}
		if j.Error != "" {
			result = j.Error
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", j.SessionId, j.Project, j.PreviousState, result)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	verb := "Cancelling"
	if c.DryRun {
		verb = "Would cancel"
	}
	fmt.Fprintf(os.Stdout, "\n%s %d job(s)\n", verb, len(jobs))
	return nil
}

// RetryCmd resubmits a finished job. The new session is returned; the original
// is left as the record of what happened.
type RetryCmd struct {
	connectFlags

	SessionID string `arg:"" name:"session-id" help:"Finished session to resubmit."`
	Prompt    string `help:"Replace the original prompt (or feedback) with this text."`
	Watch     bool   `help:"Stream the new session until it reaches a terminal state." negatable:""`
}

func (c *RetryCmd) Run() error {
	ctx := context.Background()
	oc := c.client()

	resp, err := oc.RetryJob(ctx, connect.NewRequest(&v1.RetryJobRequest{
		SessionId: c.SessionID,
		Prompt:    c.Prompt,
	}))
	if err != nil {
		return fmt.Errorf("retry job: %w", err)
	}

	sessionID := resp.Msg.SessionId
	fmt.Fprintf(os.Stdout, "Session: %s\n", sessionID)

	if !c.Watch {
		return nil
	}
	return client.WatchSession(ctx, oc, sessionID)
}

// PriorityCmd reorders a job that is still waiting in the backlog.
type PriorityCmd struct {
	connectFlags

	SessionID string `arg:"" name:"session-id" help:"Session to reorder."`
	Priority  int    `arg:"" help:"New priority; higher runs sooner."`
}

func (c *PriorityCmd) Run() error {
	resp, err := c.client().SetJobPriority(context.Background(),
		connect.NewRequest(&v1.SetJobPriorityRequest{
			SessionId: c.SessionID,
			Priority:  int32(c.Priority),
		}))
	if err != nil {
		return fmt.Errorf("set priority: %w", err)
	}

	fmt.Fprintf(os.Stdout, "Priority of %s: %d -> %d\n",
		c.SessionID, resp.Msg.PreviousPriority, c.Priority)
	return nil
}
