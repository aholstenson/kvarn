package session

import (
	"context"
	"time"

	"github.com/aholstenson/kvarn/internal/agent/cost"
)

// State represents the current phase of a session.
type State string

const (
	StatePending                State = "pending"
	StateQueued                 State = "queued"
	StateCloning                State = "cloning"
	StateProvisioning           State = "provisioning"
	StateTransferring           State = "transferring"
	StateInstallingDependencies State = "installing_dependencies"
	StatePullingImage           State = "pulling_image"
	StateSetup                  State = "setup"
	StateRunning                State = "running"
	StateValidating             State = "validating"
	StateSubmitting             State = "submitting"
	StateCompleted              State = "completed"
	StateFailed                 State = "failed"
)

// IsTerminal returns true if the state is a final state.
func (s State) IsTerminal() bool {
	return s == StateCompleted || s == StateFailed
}

// Session tracks the lifecycle of a job execution.
type Session struct {
	ID             string
	ProjectName    string
	Prompt         string
	Mode           string
	State          State
	Message        string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	Error          string
	PullRequestURL string
	// Cost is the LLM spend snapshot for the run. Updated on warning, on
	// over-budget cancellation, and once at the end of the run.
	Cost cost.Report
}

// Event represents something that happened to a session.
type Event interface {
	isSessionEvent()
}

// StateChangeEvent carries a full session snapshot after a state/message update.
type StateChangeEvent struct {
	Session *Session
}

func (StateChangeEvent) isSessionEvent() {}

// AgentMessageEvent carries a complete LLM reply (intermediate or final).
type AgentMessageEvent struct {
	SessionID string
	AgentID   string // empty for the parent agent; sub-agent identifier otherwise
	Text      string
	Final     bool
}

func (AgentMessageEvent) isSessionEvent() {}

// AgentToolUseEvent signals that the agent is invoking a tool.
type AgentToolUseEvent struct {
	SessionID     string
	AgentID       string
	ToolID        string
	ArgumentsJSON string
}

func (AgentToolUseEvent) isSessionEvent() {}

// AgentToolResultEvent carries the result of a tool invocation.
type AgentToolResultEvent struct {
	SessionID string
	AgentID   string
	ToolID    string
	Result    string
	IsError   bool
}

func (AgentToolResultEvent) isSessionEvent() {}

// StepPhase indicates which phase a step belongs to.
type StepPhase int

const (
	StepPhaseSetup              StepPhase = 1
	StepPhaseHealthCheck        StepPhase = 2
	StepPhaseValidationRequired StepPhase = 3
	StepPhaseValidationAdvisory StepPhase = 4
)

// StepResultEvent carries the outcome of a single setup/validation step execution.
type StepResultEvent struct {
	SessionID string
	Name      string
	Phase     StepPhase
	ExitCode  int32
	Stdout    string
	Stderr    string
	Passed    bool
	Skipped   bool
}

func (StepResultEvent) isSessionEvent() {}

// StepOutputEvent carries incremental stdout/stderr output from a running step.
type StepOutputEvent struct {
	SessionID string
	Name      string
	Phase     StepPhase
	Stdout    string
	Stderr    string
}

func (StepOutputEvent) isSessionEvent() {}

// VmInfoEvent carries VM hardware/resource information reported by the runner.
type VmInfoEvent struct {
	SessionID   string
	CpuCount    int32
	CpuModel    string
	MemTotalMB  int64
	MemAvailMB  int64
	DiskUsedMB  int64
	DiskTotalMB int64
}

func (VmInfoEvent) isSessionEvent() {}

// TransferProgressEvent carries file transfer progress.
type TransferProgressEvent struct {
	SessionID  string
	BytesSent  int64
	TotalBytes int64
}

func (TransferProgressEvent) isSessionEvent() {}

// DependencyOutputEvent carries stdout/stderr from a dependency installation.
type DependencyOutputEvent struct {
	SessionID string
	Stdout    string
	Stderr    string
}

func (DependencyOutputEvent) isSessionEvent() {}

// CacheProgressEvent carries per-path cache restore/save progress.
type CacheProgressEvent struct {
	SessionID string
	Path      string
	Index     int
	Total     int
	Restoring bool
}

func (CacheProgressEvent) isSessionEvent() {}

// ConsoleOutputEvent carries serial console output from the VM.
type ConsoleOutputEvent struct {
	SessionID string
	Output    string
}

func (ConsoleOutputEvent) isSessionEvent() {}

// PullRequestEvent carries information about a PR created for a session.
type PullRequestEvent struct {
	SessionID string
	URL       string
	Number    int
	Branch    string
}

func (PullRequestEvent) isSessionEvent() {}

// CostUpdateKind identifies what kind of cost transition a CostEvent reports.
type CostUpdateKind int

const (
	CostUpdateWarning    CostUpdateKind = 1
	CostUpdateOverBudget CostUpdateKind = 2
	CostUpdateFinal      CostUpdateKind = 3
)

// CostEvent carries an LLM spend snapshot, either when a budget transition
// fires mid-run (warning, over-budget) or as a final summary at run end.
type CostEvent struct {
	SessionID string
	Kind      CostUpdateKind
	Report    cost.Report
	Limit     cost.Limit
}

func (CostEvent) isSessionEvent() {}

// WatchEvent pairs an Event with the durable sequence number assigned when it
// was persisted. Seq is 0 for ephemeral events (broadcast live-only, never
// replayed).
type WatchEvent struct {
	Seq   int64
	Event Event
}

// Manager provides operations for managing sessions. It owns the live pub/sub
// hub and delegates all persistence to a Store, layering replayable history and
// reconnect-from-cursor streaming on top.
type Manager interface {
	Create(ctx context.Context, projectName string, prompt string, mode string) (*Session, error)
	Get(ctx context.Context, id string) (*Session, error)
	List(ctx context.Context, filter SessionFilter) ([]*Session, error)
	UpdateState(ctx context.Context, id string, state State, message string) error
	// UpdateCost persists the latest cost snapshot on the session. Watchers
	// see it on the next state change; mid-run snapshots are also broadcast
	// via an explicit CostEvent through EmitEvent.
	UpdateCost(ctx context.Context, id string, report cost.Report) error
	// SetPullRequest persists the PR URL on the session and broadcasts a
	// PullRequestEvent.
	SetPullRequest(ctx context.Context, id, url string, number int, branch string) error
	Fail(ctx context.Context, id string, err error) error
	// EmitEvent persists the event when its kind is durable and broadcasts it to
	// watchers; ephemeral kinds are broadcast live-only with Seq 0.
	EmitEvent(ctx context.Context, id string, event Event) error
	// Watch returns a channel that replays history with seq > fromSeq, then
	// streams live events. The channel is closed when the session reaches a
	// terminal state or ctx is cancelled.
	Watch(ctx context.Context, id string, fromSeq int64) (<-chan WatchEvent, error)
	// ListEvents returns durable history with seq > afterSeq for polling.
	ListEvents(ctx context.Context, id string, afterSeq int64, limit int) ([]WatchEvent, error)
}
