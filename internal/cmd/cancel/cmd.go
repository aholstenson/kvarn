// Package cancel implements `kvarn cancel`, which stops a run that is still in
// flight.
package cancel

import (
	"context"
	"fmt"
	"os"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/cmd/client"
)

type Cmd struct {
	Addr      string `help:"Orchestrator address." default:"http://localhost:8080"`
	SessionID string `arg:"" name:"session-id" help:"Session to cancel."`
	Reason    string `help:"Reason to record on the session." default:""`
	APIKey    string `help:"API key for the orchestrator." env:"KVARN_API_KEY" default:""`
}

func (c *Cmd) Run() error {
	oc := client.NewOrchestrator(c.Addr, c.APIKey)

	resp, err := oc.CancelJob(context.Background(), connect.NewRequest(&v1.CancelJobRequest{
		SessionId: c.SessionID,
		Reason:    c.Reason,
	}))
	if err != nil {
		return fmt.Errorf("cancel job: %w", err)
	}

	// The orchestrator returns once the stop is signalled; the run still has to
	// unwind and tear its VM down before the session reaches `cancelled`.
	fmt.Fprintf(os.Stdout, "Cancelling %s (was %s)\n", c.SessionID, resp.Msg.PreviousState)
	return nil
}
