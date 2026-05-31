package agent

import (
	"context"
	"log/slog"
)

// NoopAgent is a placeholder agent that logs and returns.
type NoopAgent struct{}

func (n *NoopAgent) Start(_ context.Context, agentCtx *Context) (Conversation, error) {
	slog.Info("noop agent invoked",
		"project", agentCtx.ProjectName,
		"prompt", agentCtx.Prompt,
		"working_dir", agentCtx.WorkingDir,
	)
	return &noopConversation{}, nil
}

type noopConversation struct{}

func (n *noopConversation) Run(_ context.Context, _ string) (string, error) {
	return "", nil
}

func (n *noopConversation) Summarize(_ context.Context) (*Result, error) {
	return &Result{}, nil
}

func (n *noopConversation) Close() error {
	return nil
}
