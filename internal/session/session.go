package session

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/aholstenson/kvarn/internal/agent/cost"
)

// State represents the current phase of a session.
type State string

const (
	// StatePending is the durable backlog: the submission has been accepted and
	// persisted, but no orchestrator goroutine owns it yet. It is the only state
	// a job can be in without a run behind it, which is what lets the backlog
	// outlive the process that took the request.
	StatePending                State = "pending"
	StateQueued                 State = "queued"
	StateCloning                State = "cloning"
	StateProvisioning           State = "provisioning"
	StateTransferring           State = "transferring"
	StateInstallingDependencies State = "installing_dependencies"
	StateSetup                  State = "setup"
	StateRunning                State = "running"
	StateValidating             State = "validating"
	StateSubmitting             State = "submitting"
	StateCompleted              State = "completed"
	StateFailed                 State = "failed"
	// StateCancelled marks a run stopped on request. It is kept apart from
	// StateFailed so a deliberate stop does not read as a broken job.
	StateCancelled State = "cancelled"
)

// allStates is every state a session can be in, in lifecycle order. It backs
// States and ParseState, so a caller that filters on a state — the queue RPCs,
// the CLI that renders their help — validates against the same set the runtime
// moves through, with no second list to keep aligned.
var allStates = []State{
	StatePending,
	StateQueued,
	StateCloning,
	StateProvisioning,
	StateTransferring,
	StateInstallingDependencies,
	StateSetup,
	StateRunning,
	StateValidating,
	StateSubmitting,
	StateCompleted,
	StateFailed,
	StateCancelled,
}

// States returns every known state in lifecycle order.
func States() []State {
	return slices.Clone(allStates)
}

// ParseState resolves a state name, rejecting one that no session can ever be
// in. Filtering on an unknown state is a typo rather than a request for an
// empty result, and saying so is what stops `--state canceled` from reading as
// "nothing is cancelled".
func ParseState(name string) (State, error) {
	st := State(name)
	if !slices.Contains(allStates, st) {
		return "", fmt.Errorf("unknown state %q", name)
	}
	return st, nil
}

// terminalStates is the single source of truth for which states are final:
// IsTerminal answers from it, and stores that need the set in a query build it
// from TerminalStates, so a state added here cannot be missed in one of them.
var terminalStates = []State{StateCompleted, StateFailed, StateCancelled}

// IsTerminal returns true if the state is a final state.
func (s State) IsTerminal() bool {
	return slices.Contains(terminalStates, s)
}

// TerminalStates returns the final states, for stores that filter on them.
func TerminalStates() []State {
	return slices.Clone(terminalStates)
}

// restartableStates are the states a run can be returned to the backlog from
// when the orchestrator restarts underneath it, rather than being failed.
//
// The line is drawn where a run starts producing effects the host cannot take
// back. Everything up to and including setup has done nothing but read: a
// clone, a wait for capacity, a VM boot, a dependency install, a cache restore.
// The VM is gone and its work is worthless, but re-running from the start
// reaches the same place, so a restart during a burst costs latency and nothing
// else.
//
// Past that point it is no longer true. StateRunning and StateValidating have
// spent budget against the job's cost cap and hold agent work that only exists
// in a VM that no longer does, so a silent retry would recharge the cap and
// throw away the reasoning. StateSubmitting is the one that must never come
// back: a push may have landed and a pull request may exist without its URL
// having reached the store, and requeueing would open a second one.
//
// StatePending is deliberately absent. It is where a requeue lands, not
// something to requeue from — a pending session never left the backlog, so
// reconciliation leaves it untouched rather than charging it an attempt for a
// restart it did not participate in.
var restartableStates = []State{
	StateQueued,
	StateCloning,
	StateProvisioning,
	StateTransferring,
	StateInstallingDependencies,
	StateSetup,
}

// IsRestartable reports whether a run left in this state by a dead
// orchestrator can be returned to the backlog instead of being failed.
func (s State) IsRestartable() bool {
	return slices.Contains(restartableStates, s)
}

// RestartableStates returns the states a run can be requeued from, for stores
// that need the set in a query.
func RestartableStates() []State {
	return slices.Clone(restartableStates)
}

// Session tracks the lifecycle of a job execution.
type Session struct {
	ID          string
	ProjectName string
	Prompt      string
	Mode        string
	State       State
	// ModeSpecJSON is the inline mode definition the submission carried, as
	// JSON, or empty when it named a mode instead of defining one. It is opaque
	// here: the orchestrator owns the vocabulary, and the session store's job is
	// to hold what was asked for until the run resolves it.
	ModeSpecJSON string
	// Result is what the run produced in writing — a read-only mode's final
	// answer, or the summary that became the commit message. Empty until the
	// agent has produced one.
	Result string

	Message        string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	Error          string
	PullRequestURL string
	// PRRef identifies the pull request this session works on, in the forge's
	// own format. Set at creation for a continuation and at submission for a
	// fresh job; empty when no PR was opened.
	PRRef string
	// Continuation records that the submission named an existing pull request
	// to work on. PRRef cannot carry this by itself: a fresh job acquires one
	// as soon as it opens its pull request, so by the time a run has finished
	// the two kinds look alike. It is read where that difference decides
	// something — dispatch builds a continuation's context pack from the PR,
	// and a retry resubmits the same kind of run it is retrying.
	Continuation bool
	// HeadBranch is the branch the session's commits land on, BaseBranch the
	// branch the PR targets.
	HeadBranch string
	BaseBranch string
	// ParentSessionID records lineage for a continuation. The parent may
	// already have been pruned by retention; nothing depends on it existing.
	ParentSessionID string
	// Cost is the LLM spend snapshot for the run. Updated on warning, on
	// over-budget cancellation, and once at the end of the run.
	Cost cost.Report

	// KeyID is the API key that submitted the job. It is persisted because a
	// pending session outlives the request that created it, and the dispatcher
	// needs it to attribute the job to a tenant and to re-check that the key is
	// still allowed to run it. Empty when auth is disabled.
	KeyID string
	// Priority ranks this job against others in the backlog, higher first. It
	// is captured at submission from the project's config, so an operator's
	// later edit applies to jobs submitted after the change rather than
	// reshuffling a backlog that has already formed.
	Priority int
	// Attempts counts how many times the job has been dispatched. A run
	// requeued by startup reconciliation carries the count forward, so a job
	// that reliably kills the orchestrator is failed rather than retried
	// forever.
	Attempts int
	// QueuedAt is when the job last entered the backlog. It is distinct from
	// CreatedAt so a requeued job ages from its return to the queue, and it is
	// what the dispatcher's ordering and the backlog age limit both read.
	QueuedAt time.Time
	// IdempotencyKey is the caller-chosen key that claims this submission. It is
	// unique per project among the sessions that carry one, which is what makes
	// a retried StartJob return this session instead of starting a second run.
	// Empty when the caller supplied no key.
	IdempotencyKey string
	// Metadata is the caller's own annotations on the submission — which ticket,
	// which chat message, which upstream run asked for this. Nothing about how a
	// run behaves depends on a key: it is filtered on by exact match, and read by
	// name only where an operator's pull-request template asks for one.
	//
	// Written at creation and never updated, so it stays a record of what was
	// asked for. Nil when the submission carried none.
	Metadata map[string]string
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

// DependencyOutputEvent carries stdout/stderr from a dependency installation
// or from a registered tool provisioning itself — one phase as far as a viewer
// is concerned, and one event kind.
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

// PullRequestEvent carries information about the PR a session works on.
type PullRequestEvent struct {
	SessionID string
	URL       string
	// Ref is the forge's own identifier for the PR; opaque to kvarn.
	Ref    string
	Branch string
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
// CreateParams describes a session to create. ProjectName, Prompt and Mode are
// always set; the PR fields are populated for a feedback run, which knows its
// pull request up front, and left empty for a fresh job.
type CreateParams struct {
	ProjectName string
	Prompt      string
	Mode        string
	// ModeSpecJSON is the inline mode definition the submission carried; see
	// the field of the same name on Session. Empty for a submission that named
	// a mode instead of defining one.
	ModeSpecJSON    string
	PRRef           string
	HeadBranch      string
	BaseBranch      string
	ParentSessionID string
	KeyID           string
	Priority        int
	// Continuation marks a submission that named an existing pull request; see
	// the field of the same name on Session.
	Continuation bool
	// IdempotencyKey claims the submission for the caller's key; see the field
	// of the same name on Session. Empty for a submission with no key.
	IdempotencyKey string
	// Metadata is the caller's annotations on the submission; see the field of
	// the same name on Session. Nil for a submission that carried none.
	Metadata map[string]string
}

type Manager interface {
	// Create persists a new session. It returns ErrIdempotencyConflict when the
	// params carry an idempotency key that another session in the project
	// already holds; FindByIdempotencyKey then resolves that session.
	Create(ctx context.Context, params CreateParams) (*Session, error)
	Get(ctx context.Context, id string) (*Session, error)
	// FindByIdempotencyKey returns the session holding key within project, or
	// nil when none does.
	FindByIdempotencyKey(ctx context.Context, project, key string) (*Session, error)
	List(ctx context.Context, filter SessionFilter) ([]*Session, error)
	UpdateState(ctx context.Context, id string, state State, message string) error
	// UpdateCost persists the latest cost snapshot on the session. Watchers
	// see it on the next state change; mid-run snapshots are also broadcast
	// via an explicit CostEvent through EmitEvent.
	UpdateCost(ctx context.Context, id string, report cost.Report) error
	// SetPullRequest persists the PR URL, ref and head branch on the session
	// and broadcasts a PullRequestEvent.
	SetPullRequest(ctx context.Context, id, url, ref, branch string) error
	// SetResult persists what the run produced in writing. It is the durable
	// copy of an answer that a mode delivering nowhere would otherwise leave
	// only in the event log.
	SetResult(ctx context.Context, id, result string) error
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

	// ListPending returns backlog entries in dispatch order.
	ListPending(ctx context.Context, q PendingQuery) ([]*Session, error)
	// CountPending returns how many sessions are waiting in the backlog.
	CountPending(ctx context.Context) (int, error)
	// TransitionPending atomically moves a session out of StatePending and
	// broadcasts the resulting state change, reporting false when the session
	// had already left the backlog.
	TransitionPending(ctx context.Context, id string, to PendingTransition) (bool, error)
	// UpdatePendingPriority reorders a backlog entry, returning the priority it
	// replaced and false when the session is no longer pending.
	UpdatePendingPriority(ctx context.Context, id string, priority int) (int, bool, error)
	// ExpirePending fails backlog entries queued before cutoff.
	ExpirePending(ctx context.Context, cutoff time.Time, reason string) ([]string, error)
	// RequeueRun returns a live run to the backlog and broadcasts the state
	// change, reporting false when the run had already advanced past the
	// states it is safe to re-run from.
	RequeueRun(ctx context.Context, id string, opts RequeueOpts) (bool, error)
}
