package agent

import (
	"context"

	"github.com/aholstenson/kvarn/internal/agent/cost"
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

// ProgressCostUpdate carries a cost update for the running job. Kind reports
// what the update represents: a soft warning (the WarnFraction was just
// crossed), an over-budget signal (the hard limit was hit), or a final
// snapshot at end of run.
type ProgressCostUpdate struct {
	Kind   CostUpdateKind
	Report cost.Report
	Limit  cost.Limit
}

func (ProgressCostUpdate) isProgressEvent() {}

// CostUpdateKind tags the meaning of a ProgressCostUpdate.
type CostUpdateKind int

const (
	CostUpdateWarning CostUpdateKind = iota + 1
	CostUpdateOverBudget
	CostUpdateFinal
)

// Mode identifies the high-level steering mode of an agent run (e.g. implement,
// review). Concrete Mode values are owned by individual agent implementations;
// this package only carries them on Context so callers can select one.
type Mode interface {
	ModeName() string
	// WritesChanges reports whether the mode is expected to modify files.
	// Read-only modes (review, research) return false; the orchestrator and
	// `run` CLI use this to skip validation and PR submission.
	WritesChanges() bool
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
	// Cost is the per-job spend tracker. When non-nil the agent should record
	// LLM token usage through it and consult it for budget enforcement.
	Cost *cost.Tracker
}

// Result holds the outcome of an agent run, including a summary suitable for
// commit messages and PR descriptions.
type Result struct {
	Title       string // short summary for commit message / PR title
	Description string // detailed description for PR body
	// Cost is the final spend snapshot for the run. Populated on both success
	// and failure paths so partial spend can still be reported.
	Cost cost.Report
}

// Agent defines the interface for agentic execution inside a VM.
type Agent interface {
	Run(ctx context.Context, agentCtx *Context) (*Result, error)
}
