// Package feedback implements `kvarn feedback`, which continues work on an
// existing pull request instead of opening a new one.
package feedback

import (
	"context"
	"fmt"
	"os"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/cmd/client"
)

type Cmd struct {
	Addr     string `help:"Orchestrator address." default:"http://localhost:8080"`
	Project  string `arg:"" help:"Project name."`
	PRRef    string `arg:"" name:"pr-ref" help:"Pull request reference, in the forge's own format (GitHub: the PR number)."`
	Feedback string `arg:"" help:"Feedback for the agent to address."`
	Mode     string `help:"Agent mode; defaults to feedback." default:""`
	Watch    bool   `help:"Watch session until completion." default:"true"`
	APIKey   string `help:"API key for the orchestrator." env:"KVARN_API_KEY" default:""`
}

func (c *Cmd) Run() error {
	oc := client.NewOrchestrator(c.Addr, c.APIKey)

	resp, err := oc.SubmitFeedback(context.Background(), connect.NewRequest(&v1.SubmitFeedbackRequest{
		Project:  c.Project,
		PrRef:    c.PRRef,
		Feedback: c.Feedback,
		Mode:     c.Mode,
	}))
	if err != nil {
		return fmt.Errorf("submit feedback: %w", err)
	}

	sessionID := resp.Msg.SessionId
	fmt.Fprintf(os.Stdout, "Session: %s\n", sessionID)

	if !c.Watch {
		return nil
	}

	return client.WatchSession(context.Background(), oc, sessionID)
}
