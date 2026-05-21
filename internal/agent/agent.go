package agent

import (
	"context"

	"github.com/aholstenson/kvarn/internal/agent/repocontext"
	"github.com/aholstenson/kvarn/internal/sandbox"
)

// ProgressEvent represents an event emitted by an agent during execution.
type ProgressEvent interface {
	isProgressEvent()
}

// ProgressTextMessage carries a complete LLM reply (intermediate or final).
type ProgressTextMessage struct {
	AgentID string // empty for the parent agent; sub-agent identifier otherwise
	Text    string
	Final   bool
}

func (ProgressTextMessage) isProgressEvent() {}

// ProgressToolUse signals that the agent is invoking a tool.
type ProgressToolUse struct {
	AgentID       string
	ToolID        string
	ArgumentsJSON string
}

func (ProgressToolUse) isProgressEvent() {}

// ProgressToolResult carries the result of a tool invocation.
type ProgressToolResult struct {
	AgentID string
	ToolID  string
	Result  string
	IsError bool
}

func (ProgressToolResult) isProgressEvent() {}

// Mode identifies the high-level steering mode of an agent run (e.g. implement,
// review). Concrete Mode values are owned by individual agent implementations;
// this package only carries them on Context so callers can select one.
type Mode interface {
	ModeName() string
}

// Context holds all the information an agent needs to perform work inside a VM.
type Context struct {
	ProjectName string
	RepoURL     string
	Branch      string
	WorkingDir  string // path inside VM
	SessionID   string // persistent shell session ID
	Prompt      string
	Mode        Mode // optional; agent supplies its own default when nil
	Runner      sandbox.RunnerProxy
	RepoContext *repocontext.RepoContext
	OnProgress  func(event ProgressEvent)
}

// Result holds the outcome of an agent run, including a summary suitable for
// commit messages and PR descriptions.
type Result struct {
	Title       string // short summary for commit message / PR title
	Description string // detailed description for PR body
}

// Agent defines the interface for agentic execution inside a VM.
type Agent interface {
	Run(ctx context.Context, agentCtx *Context) (*Result, error)
}
