package jobs

import (
	"context"
	"fmt"
	"os"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
)

// ResultCmd prints what a job produced in writing. It is how a run that
// delivers nowhere — a review, a research answer, anything in a mode with
// `deliver: none` — is read: the text goes to stdout, so it pipes.
type ResultCmd struct {
	connectFlags

	SessionID string `arg:"" name:"session-id" help:"Session to read the result of."`
	JSON      bool   `help:"Emit JSON instead of the bare result text." name:"json"`
}

func (c *ResultCmd) Run() error {
	ctx := context.Background()
	oc := c.client()

	resp, err := oc.GetSessionResult(ctx, connect.NewRequest(&v1.GetSessionResultRequest{
		SessionId: c.SessionID,
	}))
	if err != nil {
		return fmt.Errorf("get job result: %w", err)
	}

	if c.JSON {
		return PrintJSON(resp.Msg)
	}

	// An empty result is not an empty answer — it is a run that has not
	// produced one yet, or one that failed before it could — so say which,
	// on stderr, leaving stdout clean for the text itself.
	if resp.Msg.Result == "" {
		fmt.Fprintf(os.Stderr, "Job %s is %s and has produced no result.\n", resp.Msg.SessionId, resp.Msg.State)
		return nil
	}

	fmt.Fprintln(os.Stdout, resp.Msg.Result)
	return nil
}
