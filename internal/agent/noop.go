package agent

import (
	"context"
	"log/slog"
)

// NoopAgent is a placeholder agent that logs and returns.
type NoopAgent struct{}

func (n *NoopAgent) Run(_ context.Context, agentCtx *Context) (*Result, error) {
	slog.Info("noop agent invoked",
		"project", agentCtx.ProjectName,
		"prompt", agentCtx.Prompt,
		"working_dir", agentCtx.WorkingDir,
	)
	return &Result{}, nil
}
