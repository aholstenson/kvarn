package orchestrator

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/agent"
	"github.com/aholstenson/kvarn/internal/agent/coding"
	"github.com/aholstenson/kvarn/internal/agent/cost"
	"github.com/aholstenson/kvarn/internal/agent/repocontext"
	"github.com/aholstenson/kvarn/internal/config/apikey"
	"github.com/aholstenson/kvarn/internal/config/credential"
	forgeconfig "github.com/aholstenson/kvarn/internal/config/forge"
	"github.com/aholstenson/kvarn/internal/config/limits"
	modelcfg "github.com/aholstenson/kvarn/internal/config/model"
	"github.com/aholstenson/kvarn/internal/config/project"
	"github.com/aholstenson/kvarn/internal/config/secret"
	"github.com/aholstenson/kvarn/internal/dispatch"
	egressproxy "github.com/aholstenson/kvarn/internal/egress/proxy"
	"github.com/aholstenson/kvarn/internal/forge"
	"github.com/aholstenson/kvarn/internal/logging"
	"github.com/aholstenson/kvarn/internal/observability/metrics"
	"github.com/aholstenson/kvarn/internal/observability/reqid"
	"github.com/aholstenson/kvarn/internal/orchestrator/auth"
	"github.com/aholstenson/kvarn/internal/orchestrator/scheduler"
	projconfig "github.com/aholstenson/kvarn/internal/project"
	"github.com/aholstenson/kvarn/internal/sandbox"
	"github.com/aholstenson/kvarn/internal/sandbox/cache"
	"github.com/aholstenson/kvarn/internal/sandbox/transfer"
	"github.com/aholstenson/kvarn/internal/scm"
	gitscm "github.com/aholstenson/kvarn/internal/scm/git"
	"github.com/aholstenson/kvarn/internal/scm/mirror"
	"github.com/aholstenson/kvarn/internal/session"
	"github.com/aholstenson/kvarn/internal/vm"
	llms "github.com/aholstenson/llms-go"
	"go.opentelemetry.io/otel/metric"
	otelnoop "go.opentelemetry.io/otel/metric/noop"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// worklogEntry is one line in the per-job work log posted as a PR comment.
type worklogEntry struct {
	kind    worklogKind
	toolID  string
	args    string
	text    string
	isError bool
}

type worklogKind int

const (
	worklogText worklogKind = iota
	worklogToolUse
	worklogToolError
)

// worklogCollector accumulates agent progress events for later use in the PR
// comment. Safe for concurrent appends from the streaming callback.
type worklogCollector struct {
	mu      sync.Mutex
	entries []worklogEntry
}

func (w *worklogCollector) appendText(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	w.mu.Lock()
	w.entries = append(w.entries, worklogEntry{kind: worklogText, text: text})
	w.mu.Unlock()
}

func (w *worklogCollector) appendToolUse(toolID, args string) {
	w.mu.Lock()
	w.entries = append(w.entries, worklogEntry{kind: worklogToolUse, toolID: toolID, args: args})
	w.mu.Unlock()
}

func (w *worklogCollector) appendToolError(toolID, errLine string) {
	w.mu.Lock()
	w.entries = append(w.entries, worklogEntry{kind: worklogToolError, toolID: toolID, text: errLine, isError: true})
	w.mu.Unlock()
}

func (w *worklogCollector) snapshot() []worklogEntry {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]worklogEntry, len(w.entries))
	copy(out, w.entries)
	return out
}

// shortArgs trims a tool-call arguments JSON blob to a single-line preview.
func shortArgs(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 80 {
		return s[:79] + "…"
	}
	return s
}

// firstLine returns the first non-empty line of s, trimmed.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

// formatWorklogComment renders the original prompt and a collapsible work log
// for posting as a PR comment. When includeCost is true and report has any
// recorded spend, a "## Cost" section is appended after the work log.
func formatWorklogComment(prompt string, entries []worklogEntry, includeCost bool, report cost.Report) string {
	var sb strings.Builder
	sb.WriteString("## Task\n\n")
	sb.WriteString(strings.TrimSpace(prompt))
	writeWorklog(&sb, entries)
	writeCostSection(&sb, includeCost, report)
	return sb.String()
}

// formatFollowupComment renders the comment posted after a feedback run: the
// feedback that was addressed, the agent's own account of what it changed, and
// the same work log / cost sections a fresh run posts.
func formatFollowupComment(feedback, summary string, entries []worklogEntry, includeCost bool, report cost.Report) string {
	var sb strings.Builder
	sb.WriteString("## Feedback addressed\n\n")
	sb.WriteString(strings.TrimSpace(feedback))
	if summary = strings.TrimSpace(summary); summary != "" {
		sb.WriteString("\n\n## Changes\n\n")
		sb.WriteString(summary)
	}
	writeWorklog(&sb, entries)
	writeCostSection(&sb, includeCost, report)
	return sb.String()
}

// writeWorklog appends the collapsible work-log section, or nothing when there
// is no log to show.
func writeWorklog(sb *strings.Builder, entries []worklogEntry) {
	if len(entries) == 0 {
		return
	}
	sb.WriteString("\n\n<details>\n<summary>Work log</summary>\n\n")
	for _, e := range entries {
		switch e.kind {
		case worklogText:
			sb.WriteString("- ")
			sb.WriteString(firstLine(e.text))
			sb.WriteString("\n")
		case worklogToolUse:
			sb.WriteString("- Tool: ")
			sb.WriteString(e.toolID)
			if e.args != "" {
				sb.WriteString(" ")
				sb.WriteString(e.args)
			}
			sb.WriteString("\n")
		case worklogToolError:
			sb.WriteString("- Tool failed: ")
			sb.WriteString(e.toolID)
			if e.text != "" {
				sb.WriteString(" — ")
				sb.WriteString(e.text)
			}
			sb.WriteString("\n")
		}
	}
	sb.WriteString("\n</details>")
}

// writeCostSection appends the "## Cost" block when cost reporting is enabled
// and any spend was recorded.
func writeCostSection(sb *strings.Builder, includeCost bool, report cost.Report) {
	if !includeCost || (report.InputTokens == 0 && report.OutputTokens == 0 && report.TotalUSD == 0) {
		return
	}
	sb.WriteString("\n\n")
	sb.WriteString(formatCostSection(report))
}

// formatCostSection renders the per-job LLM spend as a "## Cost" markdown
// block: a totals line plus a per-model table.
func formatCostSection(report cost.Report) string {
	var sb strings.Builder
	sb.WriteString("## Cost\n\n")
	fmt.Fprintf(&sb, "Total: $%.4f — %d input / %d output / %d cached tokens\n",
		report.TotalUSD, report.InputTokens, report.OutputTokens, report.CachedTokens)
	if len(report.PerModel) > 0 {
		ids := make([]string, 0, len(report.PerModel))
		for id := range report.PerModel {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		sb.WriteString("\n| Model | Input | Output | Cached | USD |\n")
		sb.WriteString("|-------|------:|-------:|-------:|----:|\n")
		for _, id := range ids {
			m := report.PerModel[id]
			fmt.Fprintf(&sb, "| %s | %d | %d | %d | $%.4f |\n",
				m.ModelID, m.InputTokens, m.OutputTokens, m.CachedTokens, m.TotalUSD)
		}
	}
	return sb.String()
}

// Sandbox represents a running sandbox environment that runJob interacts with.
type Sandbox interface {
	GetRunner() sandbox.RunnerProxy
	GetShellSessionID() string
	GetWorkingDir() string
	RunSetup(ctx context.Context, cfg *projconfig.Config, onDone sandbox.OnStepDone, onOutput sandbox.OnOutput) (*sandbox.SetupResult, error)
	RunValidation(ctx context.Context, cfg *projconfig.Config, changedFiles []string, onDone sandbox.OnStepDone, onOutput sandbox.OnOutput) (*sandbox.ValidationResult, error)
	ChangedFiles(ctx context.Context) ([]string, error)
	ExtractChanges(ctx context.Context, destDir string) error
	SaveCache(ctx context.Context) error
	Close()
}

// SandboxFactory creates a Sandbox from the given options.
type SandboxFactory func(ctx context.Context, opts sandbox.Opts) (Sandbox, error)

// defaultSandboxFactory starts a real sandbox and returns it (satisfies Sandbox
// via the getter methods on *sandbox.Session).
func defaultSandboxFactory(ctx context.Context, opts sandbox.Opts) (Sandbox, error) {
	return sandbox.Start(ctx, opts)
}

type Service struct {
	provider         vm.Provider
	registry         *dispatch.Registry
	bridgeHandler    *dispatch.Handler
	createOpts       vm.CreateOpts
	projectStore     project.Store
	credentialStore  credential.Store
	secretStore      secret.Store
	forgeConfigStore forgeconfig.Store
	forgeDefaults    forgeconfig.DefaultsStore // optional; nil means built-in fallbacks only
	forgeTypes       map[string]forge.Forge    // type registry ("github" -> impl)
	sessionMgr       session.Manager
	agent            agent.Agent
	transferer       transfer.Transferer
	workspaceDir     string                 // VM workspace path; defaults to "/home/kvarn/workspace"
	registryMirrors  []string               // Docker registry mirrors
	cacheProvider    cache.Provider         // optional cache provider
	cacheQuota       cache.Quota            // LRU sweep limits; zero fields = unbounded
	cacheNamespace   string                 // cache namespace; "" is the shared pool
	repoMirror       *mirror.Store          // optional; nil clones straight from the forge
	repoPolicy       RepoPolicy             // mirror maintenance knobs; zero fields disable each
	sandboxFactory   SandboxFactory         // optional; nil uses defaultSandboxFactory
	defaultsStore    modelcfg.DefaultsStore // optional; nil means built-in fallbacks only
	pricingManager   *llms.PricingManager   // optional; nil disables USD computation
	apiKeyStore      apikey.Store           // API keys for request authentication
	authEnabled      bool                   // when true, project-scoped RPCs require an authorized key
	scheduler        *scheduler.Scheduler   // resource admission; never nil (defaults to unbounded)
	staging          *staging               // bounds concurrent clones; nil is unbounded
	tenantLimits     TenantLimitDefaults    // host-wide per-project/per-key caps; zero means uncapped
	meter            metric.Meter           // never nil; no-op when metrics disabled
	instruments      *metrics.Instruments   // optional; nil-safe at all call sites

	// Job lifecycle. shutdownCtx is the parent of every runJob root context;
	// Shutdown cancels it to wind down in-flight jobs and waits on jobsWG so
	// each Sandbox.Close gets a chance to tear its VM down via the bounded
	// stop path.
	jobsWG         sync.WaitGroup
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc

	// running maps a session ID to its in-flight run. An entry exists from
	// before the job goroutine is spawned until after it has written the
	// session's terminal state, so a cancel arriving at any point in between
	// finds the job. It is also the dispatcher's census of the in-memory
	// pipeline — how much room is left, and who is holding it.
	runningMu sync.Mutex
	running   map[string]runningJob

	// dispatcher moves work from the durable backlog into that pipeline. It is
	// always present and its loop is started by the constructor: a service
	// whose submissions only reach a table nothing drains cannot run a job at
	// all, so this is not something a caller should be able to forget to wire.
	dispatcher *dispatcher

	// continuationMu serializes the per-PR single-flight check with the session
	// creation that follows it, so two concurrent submissions against the same
	// pull request cannot both find it idle.
	continuationMu sync.Mutex

	// drain is the host's admission stance: whether the dispatcher may move
	// work out of the backlog. See drain.go.
	drainMu sync.Mutex
	drain   drainStatus
}

type ServiceOpts struct {
	Provider            vm.Provider
	CreateOpts          vm.CreateOpts
	ProjectStore        project.Store
	CredentialStore     credential.Store
	SecretStore         secret.Store
	ForgeConfigStore    forgeconfig.Store
	ForgeDefaultsStore  forgeconfig.DefaultsStore // optional; nil means built-in fallbacks only
	ForgeTypes          map[string]forge.Forge
	SessionMgr          session.Manager
	Agent               agent.Agent
	Transferer          transfer.Transferer
	WorkspaceDir        string                 // VM workspace path; defaults to "/home/kvarn/workspace"
	RegistryMirrors     []string               // Docker registry mirrors (infrastructure config)
	CacheProvider       cache.Provider         // optional cache provider
	CacheQuota          cache.Quota            // LRU sweep limits; zero fields = unbounded
	Namespace           string                 // cache namespace; "" is the shared pool
	RepoMirror          *mirror.Store          // optional; nil clones straight from the forge
	RepoPolicy          RepoPolicy             // mirror maintenance knobs; ignored when RepoMirror is nil
	SandboxFactory      SandboxFactory         // optional; nil uses defaultSandboxFactory
	DefaultsStore       modelcfg.DefaultsStore // optional; nil means no user defaults (built-ins only)
	PricingManager      *llms.PricingManager   // optional; nil disables USD computation
	APIKeyStore         apikey.Store           // API keys for request authentication
	AuthEnabled         bool                   // when true, project-scoped RPCs require an authorized key
	Scheduler           *scheduler.Scheduler   // optional; nil means unbounded (no admission control)
	MaxConcurrentClones int                    // optional; 0 means unbounded
	TenantLimits        TenantLimitDefaults    // host-wide per-project/per-key caps a project or key overrides
	Meter               metric.Meter           // optional; nil uses an otel no-op meter
	Instruments         *metrics.Instruments   // optional; nil disables job/auth/scheduler instrumentation
	Dispatch            DispatchPolicy         // backlog dispatch bounds; zero value dispatches everything immediately
}

func NewService(p vm.Provider, createOpts vm.CreateOpts) *Service {
	reg := dispatch.NewRegistry()
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	s := &Service{
		provider:       p,
		registry:       reg,
		bridgeHandler:  dispatch.NewHandler(reg),
		createOpts:     createOpts,
		scheduler:      scheduler.NewUnbounded(),
		meter:          otelnoop.NewMeterProvider().Meter("kvarn"),
		shutdownCtx:    shutdownCtx,
		shutdownCancel: shutdownCancel,
		running:        make(map[string]runningJob),
	}
	s.startDispatcher(DispatchPolicy{})
	return s
}

func NewServiceWithOpts(opts ServiceOpts) *Service {
	wsDir := opts.WorkspaceDir
	if wsDir == "" {
		wsDir = "/home/kvarn/workspace"
	}
	sched := opts.Scheduler
	if sched == nil {
		sched = scheduler.NewUnbounded()
	}
	meter := opts.Meter
	if meter == nil {
		meter = otelnoop.NewMeterProvider().Meter("kvarn")
	}
	reg := dispatch.NewRegistry()
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	s := &Service{
		provider:         opts.Provider,
		registry:         reg,
		bridgeHandler:    dispatch.NewHandler(reg),
		createOpts:       opts.CreateOpts,
		projectStore:     opts.ProjectStore,
		credentialStore:  opts.CredentialStore,
		secretStore:      opts.SecretStore,
		forgeConfigStore: opts.ForgeConfigStore,
		forgeDefaults:    opts.ForgeDefaultsStore,
		forgeTypes:       opts.ForgeTypes,
		sessionMgr:       opts.SessionMgr,
		agent:            opts.Agent,
		transferer:       opts.Transferer,
		workspaceDir:     wsDir,
		registryMirrors:  opts.RegistryMirrors,
		cacheProvider:    opts.CacheProvider,
		cacheQuota:       opts.CacheQuota,
		cacheNamespace:   opts.Namespace,
		repoMirror:       opts.RepoMirror,
		repoPolicy:       opts.RepoPolicy,
		sandboxFactory:   opts.SandboxFactory,
		defaultsStore:    opts.DefaultsStore,
		pricingManager:   opts.PricingManager,
		apiKeyStore:      opts.APIKeyStore,
		authEnabled:      opts.AuthEnabled,
		scheduler:        sched,
		staging:          newStaging(opts.MaxConcurrentClones),
		tenantLimits:     opts.TenantLimits,
		meter:            meter,
		instruments:      opts.Instruments,
		shutdownCtx:      shutdownCtx,
		shutdownCancel:   shutdownCancel,
		running:          make(map[string]runningJob),
	}
	s.startDispatcher(opts.Dispatch)
	return s
}

// startDispatcher builds the backlog dispatcher and starts its loop on the
// service's shutdown context, so draining the service also stops new work being
// pulled out of the backlog.
func (s *Service) startDispatcher(policy DispatchPolicy) {
	s.dispatcher = newDispatcher(s, policy)
	s.dispatcher.start(s.shutdownCtx)
}

// dispatchedCount is how many runs occupy the in-memory pipeline.
func (s *Service) dispatchedCount() int {
	s.runningMu.Lock()
	defer s.runningMu.Unlock()
	return len(s.running)
}

// dispatchedPerProject counts in-flight runs by project, for the dispatcher's
// per-project share of the pipeline.
func (s *Service) dispatchedPerProject() map[string]int {
	s.runningMu.Lock()
	defer s.runningMu.Unlock()
	out := make(map[string]int, len(s.running))
	for _, job := range s.running {
		out[job.project]++
	}
	return out
}

// Shutdown signals every in-flight runJob to wind down and waits for them to
// return, bounded by ctx. The per-job `defer sandbox.Close()` chains run the
// bounded VM-stop path so VMs are torn down rather than orphaned. Callers
// typically pass a context with a deadline (see shutdownTimeout in run).
func (s *Service) Shutdown(ctx context.Context) {
	// Draining first is what keeps the teardown window from starting work it is
	// about to kill: the dispatcher can otherwise claim a backlog entry while
	// jobs are winding down, and that job would be failed seconds later having
	// spent an attempt. An operator who wants running jobs to *finish* drains
	// ahead of the signal and waits; this only stops the bleeding.
	s.setDrain(true, "orchestrator shutting down")
	s.shutdownCancel()

	done := make(chan struct{})
	go func() {
		s.jobsWG.Wait()
		close(done)
	}()

	select {
	case <-done:
		slog.Info("all jobs drained")
	case <-ctx.Done():
		slog.Warn("shutdown deadline reached; some jobs may still be running")
	}
}

// checkBacklogDepth refuses a submission the durable backlog cannot take. This
// is the only place a submission is turned away for depth: the in-memory queue
// behind it is fed by the dispatcher, which never pushes more into it than it
// holds, so its own bound is a guard rather than a limit callers meet.
//
// A backlog entry costs a row, which is why its bound can be set orders of
// magnitude above the pipeline's and why reaching it means something has gone
// badly wrong rather than that the host is merely busy.
func (s *Service) checkBacklogDepth(ctx context.Context, project string) error {
	maxBacklog := s.dispatcher.backlogBound()
	if maxBacklog <= 0 {
		return nil
	}
	depth, err := s.sessionMgr.CountPending(ctx)
	if err != nil {
		// A store that cannot be counted will not take the session either;
		// let the create call report the real problem.
		slog.WarnContext(ctx, "could not measure backlog depth", "error", err)
		return nil
	}
	if depth < maxBacklog {
		return nil
	}
	s.instruments.RecordAdmissionDenied(ctx, project, "backlog_full")
	return connect.NewError(connect.CodeResourceExhausted, errBacklogFull)
}

// callerKeyID returns the API key behind ctx, or "" when auth is disabled and
// no identity was injected. Unauthenticated jobs share the empty key, which is
// the honest reading: without auth there is no caller to tell apart.
func callerKeyID(ctx context.Context) string {
	if id, ok := auth.IdentityFrom(ctx); ok {
		return id.KeyID
	}
	return ""
}

// authorizeProject enforces that the authenticated caller is allowed to act on
// the given project. It is a no-op when auth is disabled (local dev). When auth
// is enabled the interceptor has already injected an Identity; a missing one is
// treated as unauthenticated, and a project the key does not cover is denied.
func (s *Service) authorizeProject(ctx context.Context, project, procedure string) error {
	if !s.authEnabled {
		return nil
	}
	id, ok := auth.IdentityFrom(ctx)
	if !ok {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("missing identity"))
	}
	if !id.AllowsProject(project) {
		slog.LogAttrs(ctx, slog.LevelWarn, "api_key_authz_denied",
			slog.Bool("audit", true),
			slog.String("key_name", id.KeyName),
			slog.String("key_id", id.KeyID),
			slog.String("project", project),
			slog.String("method", procedure),
		)
		return connect.NewError(connect.CodePermissionDenied,
			fmt.Errorf("key %q not allowed for project %q", id.KeyName, project))
	}
	return nil
}

// callerIsWildcard reports whether the caller's project scope is unbounded, so
// a request that names no project reaches the whole host rather than the
// caller's own corner of it. With auth disabled every caller is unbounded,
// since there is no scope to bound them by.
func (s *Service) callerIsWildcard(ctx context.Context) bool {
	if !s.authEnabled {
		return true
	}
	id, ok := auth.IdentityFrom(ctx)
	return ok && id.IsWildcard()
}

// authorizeHost enforces that the caller may act on the orchestrator itself
// rather than on one project. It is the check for requests that have no project
// to scope them to, and it is deliberately not satisfied by the project
// wildcard: `*` says a key may reach every project, which is what a CI bot
// needs, and says nothing about whether it speaks for the host.
//
// Like authorizeProject it is a no-op with auth disabled, so --no-auth stays
// one flag that opens everything rather than two half-open doors.
func (s *Service) authorizeHost(ctx context.Context, procedure string) error {
	if !s.authEnabled {
		return nil
	}
	id, ok := auth.IdentityFrom(ctx)
	if !ok {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("missing identity"))
	}
	if !id.Allows(apikey.CapabilityHost) {
		slog.LogAttrs(ctx, slog.LevelWarn, "capability_denied",
			slog.Bool("audit", true),
			slog.String("auth_source", id.Source),
			slog.String("key_name", id.KeyName),
			slog.String("key_id", id.KeyID),
			slog.String("peer", id.Peer),
			slog.String("capability", string(apikey.CapabilityHost)),
			slog.String("method", procedure),
		)
		return connect.NewError(connect.CodePermissionDenied,
			fmt.Errorf("%q lacks the %q capability, which this request needs because it acts on the orchestrator rather than on a project",
				id.KeyName, apikey.CapabilityHost))
	}
	return nil
}

// startJobParams is one submission, of either kind. It is the shared body of
// StartJob and of a retry that resubmits the same work.
type startJobParams struct {
	project string
	prompt  string
	// branch and prRef are the two starting points, and at most one is set.
	// branch clones and opens a pull request; prRef continues the pull request
	// it names. Both empty starts from the project's default branch.
	branch string
	prRef  string
	mode   string
	// modeSpec is a mode definition supplied with the request, for a run whose
	// shape the repository does not define. Nil for a submission that named a
	// mode instead of defining one.
	modeSpec *coding.Spec
	// idempotencyKey, when set, makes the submission replayable: a second
	// request carrying it returns the session the first one created. Empty for a
	// caller that did not ask for the guarantee.
	idempotencyKey string
	// metadata is the caller's own annotations on the submission, stored with
	// the session and never interpreted here. Nil when none were sent.
	metadata map[string]string
	// procedure names the RPC on whose behalf the submission is authorized, for
	// the audit log that records a denial.
	procedure string
}

// continues reports whether the submission names a pull request to work on
// rather than a branch to start from.
func (p startJobParams) continues() bool { return p.prRef != "" }

// maxIdempotencyKeyLen bounds a caller-supplied key. It is generous for the
// UUIDs and request ids callers actually use, and stops an unbounded string
// from being written to every session row.
const maxIdempotencyKeyLen = 255

// submissionResult is one accepted submission. Duplicate is true when the
// request matched an existing idempotency key, so the session it names was
// created by an earlier request and no new run was started.
type submissionResult struct {
	session   *session.Session
	duplicate bool
}

// startJob admits one submission of either kind. Everything a submission needs
// regardless of where it starts is settled here — who may submit it, which mode
// it runs in, which project it belongs to — and the starting point then decides
// which arm accepts it.
//
// The two arms differ in how much they check up front, and deliberately so. A
// fresh job resolves nothing beyond its branch, because the dispatcher reads
// the project, the forge and the credentials as they are when the run actually
// starts. A continuation has to read its pull request now, because a reference
// that names nothing is a bad request and should be answered as one rather than
// become a session that fails minutes later.
func (s *Service) startJob(ctx context.Context, p startJobParams) (*submissionResult, error) {
	if err := s.authorizeProject(ctx, p.project, p.procedure); err != nil {
		return nil, err
	}

	if s.projectStore == nil || s.sessionMgr == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("project-aware jobs not configured"))
	}

	if strings.TrimSpace(p.prompt) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("prompt is required"))
	}
	if len(p.idempotencyKey) > maxIdempotencyKeyLen {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("idempotency key is %d bytes; the limit is %d", len(p.idempotencyKey), maxIdempotencyKeyLen))
	}
	if err := validateMetadata(p.metadata); err != nil {
		return nil, invalidArgument(err)
	}

	// The mode is checked here but not necessarily settled: a project defines
	// its own modes in its kvarn.yml, which is not readable until the run's
	// clone. What can be checked without the repository is, and the rest
	// travels with the job. See mode.go.
	choice, err := resolveSubmittedMode(p)
	if err != nil {
		return nil, invalidArgument(err)
	}
	if err := checkModeFeasible(choice.resolved, p.continues()); err != nil {
		return nil, invalidArgument(err)
	}

	specJSON, err := choice.specJSON()
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	log := reqid.LoggerFrom(ctx).With("project", p.project, "mode", choice.name)
	if len(p.metadata) > 0 {
		// The annotations join the job's log line for the same reason they are
		// stored: an operator asking which jobs a given upstream request produced
		// usually asks it of the logs first. Bounded by validateMetadata.
		attrs := make([]any, 0, len(p.metadata))
		for _, k := range slices.Sorted(maps.Keys(p.metadata)) {
			attrs = append(attrs, slog.String(k, p.metadata[k]))
		}
		log = log.With(slog.Group("metadata", attrs...))
	}

	if p.continues() {
		// The pull request arm's own fast path, before the forge round trips
		// its checks need: a replay spelled the way the first request spelled it
		// settles here. Anything else — no key, no match yet, or a reference the
		// forge would canonicalise differently — falls through to the
		// authoritative check, which compares against the resolved ref.
		if p.idempotencyKey != "" {
			claimed, err := s.findClaimedSession(ctx, p.project, p.idempotencyKey)
			if err != nil {
				return nil, err
			}
			if claimed != nil && sameSubmission(claimed, p.prompt, choice.name, specJSON, p.prRef, claimed.PRRef) == nil {
				log.Info("idempotent replay", "session_id", claimed.ID)
				return &submissionResult{session: claimed, duplicate: true}, nil
			}
		}
	}

	proj, err := s.projectStore.Get(ctx, p.project)
	if err != nil {
		log.Error("project not found", "error", err)
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("project %q: %w", p.project, err))
	}

	log.Info("resolved project", "repo", proj.RepoURL, "forge", proj.Forge)

	if p.continues() {
		return s.startContinuation(ctx, p, proj, choice, specJSON, log)
	}
	return s.startFresh(ctx, p, proj, choice, specJSON, log)
}

// startFresh admits a submission that starts from a branch: clone it, and open
// a pull request against it when the run produces changes.
func (s *Service) startFresh(
	ctx context.Context,
	p startJobParams,
	proj *project.Project,
	choice modeChoice,
	specJSON string,
	log *slog.Logger,
) (*submissionResult, error) {
	branch := p.branch
	if branch == "" {
		branch = proj.DefaultBranch
	}

	log.Info("starting job", "branch", branch)

	// The key is resolved against the store before anything else is spent on the
	// request, so the common retry — the client that saw a timeout and sent the
	// same submission again — costs a lookup and does not touch the backlog
	// limit. The branch compared here is the resolved one: a caller that omitted
	// it and one that named the project default sent the same job.
	replay := func(claimed *session.Session) (*submissionResult, error) {
		if err := sameSubmission(claimed, p.prompt, choice.name, specJSON, branch, claimed.BaseBranch); err != nil {
			return nil, err
		}
		log.Info("idempotent replay", "session_id", claimed.ID)
		return &submissionResult{session: claimed, duplicate: true}, nil
	}
	if p.idempotencyKey != "" {
		claimed, err := s.findClaimedSession(ctx, p.project, p.idempotencyKey)
		if err != nil {
			return nil, err
		}
		if claimed != nil {
			return replay(claimed)
		}
	}

	if err := s.checkBacklogDepth(ctx, p.project); err != nil {
		return nil, err
	}

	// The session is the durable record that the job exists, so writing it is
	// what accepts the submission: once this returns, the run happens even if
	// the orchestrator dies on the next instruction.
	sess, err := s.sessionMgr.Create(ctx, session.CreateParams{
		ProjectName:    p.project,
		Prompt:         p.prompt,
		Mode:           choice.name,
		ModeSpecJSON:   specJSON,
		BaseBranch:     branch,
		KeyID:          callerKeyID(ctx),
		Priority:       jobPriority(proj, choice.name),
		IdempotencyKey: p.idempotencyKey,
		Metadata:       p.metadata,
	})
	// Two copies of one retried request can both get past the lookup above; the
	// store's uniqueness constraint is what decides between them, and the loser
	// answers with the session the winner created rather than with an error.
	if errors.Is(err, session.ErrIdempotencyConflict) {
		claimed, findErr := s.findClaimedSession(ctx, p.project, p.idempotencyKey)
		if findErr != nil {
			return nil, findErr
		}
		if claimed == nil {
			return nil, connect.NewError(connect.CodeInternal, errors.New("idempotency key claimed by a session that cannot be read back"))
		}
		return replay(claimed)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create session: %w", err))
	}

	log.Info("job queued", "session_id", sess.ID, "branch", branch)
	s.dispatcher.poke()

	return &submissionResult{session: sess}, nil
}

// findClaimedSession resolves an idempotency key to the session already holding
// it, or nil when the key is unclaimed. It exists so both arms report a store
// failure the same way.
func (s *Service) findClaimedSession(ctx context.Context, project, key string) (*session.Session, error) {
	claimed, err := s.sessionMgr.FindByIdempotencyKey(ctx, project, key)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("look up idempotency key: %w", err))
	}
	return claimed, nil
}

// sameSubmission checks that a replayed idempotency key describes the work it
// originally claimed. A key reused with different content is a client bug — two
// distinct submissions sharing one key — and silently returning the first one
// would drop the second without anyone noticing.
//
// startFrom is where the run begins: the branch a fresh job clones, or the pull
// request a continuation works on. claimedStartFrom is that same thing read off
// the claimed session, which is why the caller supplies both.
//
// modeSpecJSON is compared alongside the mode name because an inline definition
// is part of what was submitted: two requests naming the same inline mode but
// defining it differently are two different jobs, and collapsing them into one
// would silently drop the second.
//
// Metadata is deliberately not compared. It annotates the work rather than
// describing it, and a caller that regenerates a trace id between two sends of
// one retried request has still sent that request once. The first submission's
// annotations are the ones kept, and the duplicate flag on the response is what
// tells the caller its second send changed nothing.
func sameSubmission(claimed *session.Session, prompt, mode, modeSpecJSON, startFrom, claimedStartFrom string) error {
	if claimed.Prompt == prompt && claimed.Mode == mode &&
		claimed.ModeSpecJSON == modeSpecJSON && claimedStartFrom == startFrom {
		return nil
	}
	return connect.NewError(connect.CodeAlreadyExists,
		fmt.Errorf("idempotency key already used by session %s for a different submission", claimed.ID))
}

func (s *Service) StartJob(ctx context.Context, req *connect.Request[v1.StartJobRequest]) (*connect.Response[v1.StartJobResponse], error) {
	msg := req.Msg

	p := startJobParams{
		project:        msg.Project,
		prompt:         msg.Prompt,
		mode:           msg.Mode,
		modeSpec:       modeSpecFromProto(msg.GetModeSpec()),
		idempotencyKey: msg.IdempotencyKey,
		metadata:       msg.GetMetadata(),
		procedure:      req.Spec().Procedure,
	}
	// An unset start_from is the project's default branch. A set-but-empty one
	// is refused rather than folded into that default: a caller that meant to
	// name a pull request and computed an empty reference would otherwise get a
	// fresh job and a second pull request, which is the outcome naming the pull
	// request was meant to avoid.
	switch from := msg.GetStartFrom().(type) {
	case *v1.StartJobRequest_Branch:
		p.branch = strings.TrimSpace(from.Branch)
	case *v1.StartJobRequest_PrRef:
		p.prRef = strings.TrimSpace(from.PrRef)
		if p.prRef == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				errors.New("pr_ref is empty; omit start_from to run from the project's default branch"))
		}
	}

	res, err := s.startJob(ctx, p)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&v1.StartJobResponse{
		SessionId: res.session.ID,
		Duplicate: res.duplicate,
	}), nil
}

// prTarget identifies the pull request a continuation run pushes back to.
// HeadSHA is the tip observed when the run started; submission aborts if the
// branch has moved since.
type prTarget struct {
	ref        string
	headBranch string
	headSHA    string
	url        string
}

// jobSpec is everything runJob needs for one run. A nil pr means the fresh-job
// behavior: clone the base branch and open a new pull request. A non-nil pr
// means continuing on that PR's head branch.
type jobSpec struct {
	sessionID string
	// keyID is the API key that submitted the job, captured here because the
	// job's context is detached from the request's and so carries no identity.
	// Empty when auth is disabled.
	keyID string
	proj  *project.Project
	// modeName is what the submission asked for and modeSpec the definition it
	// supplied inline, if any. The runnable mode is resolved from these once the
	// repository has been cloned and its own `modes:` block read; see
	// resolveRunMode.
	modeName   string
	modeSpec   *coding.Spec
	baseBranch string
	// userPrompt is what the requester actually asked for, used for the PR
	// comment and as the last section of the agent's task message.
	userPrompt string
	// agentContext holds the pieces a context pack can be built from. Which of
	// them the agent actually sees is the mode's `context` axis, applied inside
	// the run.
	agentContext coding.ContextInput
	pr           *prTarget
}

// resolvedForge carries the forge wiring for a project: its config, the
// implementation, a credential source, and the expanded clone URL. impl and
// cfg are nil when the project has no forge configured, in which case cloneURL
// is the raw repo URL and plain git is used.
//
// creds is a source rather than a token because a job can outlive the
// credentials it started with; every operation resolves it afresh.
type resolvedForge struct {
	cfg      *forgeconfig.ForgeConfig
	impl     forge.Forge
	creds    scm.CredentialSource
	cloneURL string
}

// resolveForge loads the project's forge config, looks up the implementation,
// expands the clone URL, and resolves credentials.
func (s *Service) resolveForge(ctx context.Context, proj *project.Project) (*resolvedForge, error) {
	if proj.Forge == "" || s.forgeConfigStore == nil {
		// No forge configured — use plain git with the repo URL as-is.
		return &resolvedForge{cloneURL: proj.RepoURL}, nil
	}

	cfg, err := s.forgeConfigStore.Get(ctx, proj.Forge)
	if err != nil {
		return nil, fmt.Errorf("load forge config %q: %w", proj.Forge, err)
	}

	var impl forge.Forge
	if s.forgeTypes != nil {
		impl = s.forgeTypes[cfg.Type]
	}
	if impl == nil {
		return nil, fmt.Errorf("unknown forge type %q", cfg.Type)
	}

	cloneURL, err := impl.ResolveCloneURL(proj.RepoURL)
	if err != nil {
		return nil, fmt.Errorf("resolve clone URL: %w", err)
	}

	var creds scm.CredentialSource
	if cfg.Credential != "" && s.credentialStore != nil {
		cred, err := s.credentialStore.Get(ctx, cfg.Credential)
		if err != nil {
			return nil, fmt.Errorf("load credential %q: %w", cfg.Credential, err)
		}
		creds, err = impl.ResolveCredentials(ctx, cred.Config)
		if err != nil {
			return nil, fmt.Errorf("resolve credentials: %w", err)
		}
	}

	return &resolvedForge{cfg: cfg, impl: impl, creds: creds, cloneURL: cloneURL}, nil
}

// runJob executes one run. rootCtx and cancelJob come from beginJob, which
// registered the run for cancellation before this goroutine started; cancelJob
// is also handed to the cost tracker so a tripped budget stops the run the same
// way an operator does.
func (s *Service) runJob(rootCtx context.Context, cancelJob context.CancelCauseFunc, spec jobSpec) {
	defer s.jobsWG.Done()
	sessionID := spec.sessionID
	proj := spec.proj
	// The mode's name is all that is available until the repository is cloned;
	// the mode itself is resolved from it (and from the project's kvarn.yml)
	// after the clone, below.
	modeName := spec.modeName
	if modeName == "" {
		modeName = coding.ModeAuto.Name
	}
	var mode *coding.Mode
	defer s.endJob(sessionID)
	defer cancelJob(nil)
	// A run is no longer bounded by the request that submitted it — it may
	// start minutes later, after a restart, from a backlog row — so the session
	// ID rather than a request ID is what ties its log lines together.
	ctx := rootCtx
	// Writes that decide the session's final state have to outlive the job's
	// context. A cost-cap trip cancels rootCtx and shutdown cancels its parent,
	// so a Fail/Completed write made on ctx would never reach SQLite and the
	// session would sit non-terminal until the next restart's reconciliation
	// flipped it. Dropping only the cancellation keeps the request ID attached
	// for logging inside the session manager.
	termCtx := context.WithoutCancel(rootCtx)
	log := reqid.LoggerFrom(ctx).With("session_id", sessionID, "project", proj.Name, "mode", modeName)

	// The branch to clone and work on: a continuation run picks up the pull
	// request's head branch, a fresh run starts from the base branch.
	branch := spec.baseBranch
	if spec.pr != nil {
		branch = spec.pr.headBranch
		log = log.With("pr_ref", spec.pr.ref)
	}

	jobStart := time.Now()
	defer func() {
		outcome := "success"
		// Inspect the persisted state rather than threading a flag through every
		// failure return; session.Fail has already moved it to StateFailed by
		// the time this defer runs.
		if final, err := s.sessionMgr.Get(termCtx, sessionID); err == nil {
			switch final.State {
			case session.StateCompleted:
				outcome = "success"
			case session.StateFailed:
				outcome = "failed"
			case session.StateCancelled:
				outcome = "cancelled"
			default:
				// Non-terminal here means the goroutine returned without
				// recording an outcome, which shutdown does to in-flight jobs.
				outcome = "cancelled"
			}
		}
		s.instruments.RecordJobEnd(ctx, proj.Name, coding.MetricsModeLabel(modeName), outcome, time.Since(jobStart).Seconds())
	}()

	log.Info("job started", "repo", proj.RepoURL, "branch", branch)

	// Resolve cost limits for this (project, mode) pair. Built-in fallbacks
	// kick in when no user defaults or project overrides are configured, so
	// the tracker is always created with a sane MaxCostUSD.
	var userDefaults modelcfg.Defaults
	if s.defaultsStore != nil {
		d, err := s.defaultsStore.Defaults(ctx)
		if err != nil {
			log.Warn("failed to load user defaults; using built-in fallbacks", "error", err)
		} else {
			userDefaults = d
		}
	}
	costLimits := limits.Resolve(proj, userDefaults, modeName)
	tracker := cost.NewTracker(cost.TrackerOpts{
		Pricing: s.pricingManager,
		Limit:   cost.Limit{MaxUSD: costLimits.MaxCostUSD, WarnFraction: costLimits.WarnThreshold},
		Cancel:  cancelJob,
		OnWarning: func(report cost.Report) {
			log.Info("cost warning threshold crossed", "usd", report.TotalUSD, "limit_usd", costLimits.MaxCostUSD)
			s.sessionMgr.EmitEvent(ctx, sessionID, session.CostEvent{
				SessionID: sessionID,
				Kind:      session.CostUpdateWarning,
				Report:    report,
				Limit:     cost.Limit{MaxUSD: costLimits.MaxCostUSD, WarnFraction: costLimits.WarnThreshold},
			})
			s.sessionMgr.UpdateCost(ctx, sessionID, report)
		},
		OnOverBudget: func(report cost.Report) {
			log.Warn("cost limit exceeded; cancelling job", "usd", report.TotalUSD, "limit_usd", costLimits.MaxCostUSD)
			s.sessionMgr.EmitEvent(ctx, sessionID, session.CostEvent{
				SessionID: sessionID,
				Kind:      session.CostUpdateOverBudget,
				Report:    report,
				Limit:     cost.Limit{MaxUSD: costLimits.MaxCostUSD, WarnFraction: costLimits.WarnThreshold},
			})
			s.sessionMgr.UpdateCost(ctx, sessionID, report)
		},
	})

	fr, err := s.resolveForge(ctx, proj)
	if err != nil {
		log.Error("failed to resolve forge", "forge", proj.Forge, "error", err)
		s.failRun(termCtx, rootCtx, sessionID, err)
		return
	}
	forgeCfg, forgeImpl, creds, cloneURL := fr.cfg, fr.impl, fr.creds, fr.cloneURL

	// Clone repo first so we can read kvarn.yml before booting the VM.
	cloneDir, err := os.MkdirTemp("", "kvarn-clone-*")
	if err != nil {
		log.Error("failed to create temp dir", "error", err)
		s.failRun(termCtx, rootCtx, sessionID, fmt.Errorf("create temp dir: %w", err))
		return
	}
	defer os.RemoveAll(cloneDir)

	// The clone and the kvarn.yml read that follows it are bounded separately
	// from the capacity pool, which cannot account for them: the footprint
	// they resolve is the very thing the pool admits against.
	releaseStaging, err := s.enterStaging(ctx, sessionID, log)
	if err != nil {
		log.Error("staging failed", "error", err)
		s.failRun(termCtx, rootCtx, sessionID, err)
		return
	}
	defer releaseStaging()

	log.Info("cloning repository", "url", gitscm.RedactURL(cloneURL), "branch", branch, "destination", cloneDir)
	s.sessionMgr.UpdateState(ctx, sessionID, session.StateCloning, "Cloning repository")
	cloneStart := time.Now()

	// Pick the SCM to use for cloning.
	var scmImpl scm.SCM
	if forgeImpl != nil {
		scmImpl = forgeImpl.SCM()
	} else {
		scmImpl = &gitscm.Git{}
	}

	cloneDepth := scm.DefaultCloneDepth
	if proj.CloneDepth != nil {
		cloneDepth = *proj.CloneDepth
	}
	wantSHA := ""
	if spec.pr != nil {
		wantSHA = spec.pr.headSHA
	}
	if !s.cloneViaMirror(ctx, proj, cloneURL, branch, cloneDir, cloneDepth, creds, wantSHA, log) {
		cloneOpts := scm.CloneOpts{
			URL:         cloneURL,
			Branch:      branch,
			Destination: cloneDir,
			Credentials: creds,
			Depth:       cloneDepth,
		}
		if err := scmImpl.Clone(ctx, cloneOpts); err != nil {
			log.Error("clone failed", "error", err)
			s.failRun(termCtx, rootCtx, sessionID, fmt.Errorf("clone repository: %w", err))
			return
		}
	}
	log.Info("clone complete", "duration", logging.Elapsed(cloneStart))

	// Load project config from cloned repo.
	cfg, err := projconfig.Load(cloneDir)
	if err != nil {
		log.Error("failed to load project config", "error", err)
		s.failRun(termCtx, rootCtx, sessionID, fmt.Errorf("load project config: %w", err))
		return
	}

	// The repository is on disk, so its `modes:` block is finally readable and
	// the mode the submission asked for can be settled. Everything that reads a
	// mode's capabilities — the agent's toolset, whether validation runs, where
	// the result goes — happens below this point.
	mode, err = resolveRunMode(cfg, modeName, spec.modeSpec)
	if err != nil {
		log.Error("failed to resolve mode", "error", err)
		s.failRun(termCtx, rootCtx, sessionID, err)
		return
	}
	if err := checkModeFeasible(mode, spec.pr != nil); err != nil {
		log.Error("mode cannot run from here", "error", err)
		s.failRun(termCtx, rootCtx, sessionID, err)
		return
	}
	agentPrompt := mode.BuildPrompt(spec.agentContext)

	// The footprint is known, so the clone permit has done its job. Handing it
	// back here rather than at the end of the run is the whole point: held
	// across the wait for capacity, the number of permits would cap the queue
	// instead of the clones.
	releaseStaging()

	// Reserve capacity before booting the VM. The footprint matches what
	// sandbox.Start will request, so the scheduler accounts for what actually
	// runs. Release is deferred so any provisioning failure returns the slot.
	cpuCount := uint(0)
	memBytes := uint64(0)
	diskBytes := int64(0)
	if cfg != nil {
		cpuCount = cfg.CPUs()
		memBytes = cfg.MemoryBytes()
		diskBytes = cfg.DiskSizeBytes()
	}
	if cpuCount == 0 {
		cpuCount = projconfig.DefaultCPUs
	}
	if memBytes == 0 {
		memBytes = projconfig.DefaultMemory
	}
	if diskBytes == 0 {
		diskBytes = projconfig.DefaultDiskSize
	}
	// Caps are read here, per job, so an operator's edit applies to the next
	// job without a restart — the same hot-reload the stores give everything
	// else. They then travel with the request, keeping the scheduler's policy
	// free of config reads while it holds its lock.
	projLimits, keyLim, err := s.resolveJobLimits(ctx, proj, spec.keyID)
	if err != nil {
		log.Error("failed to resolve tenant limits", "error", err)
		s.failRun(termCtx, rootCtx, sessionID, err)
		return
	}
	admitReq := scheduler.Request{
		CPUMillis:     uint64(cpuCount) * 1000,
		MemBytes:      memBytes,
		DiskBytes:     uint64(diskBytes),
		Tenant:        scheduler.Tenant{Project: proj.Name, KeyID: spec.keyID},
		Priority:      jobPriority(proj, modeName),
		ProjectLimits: projLimits,
		KeyLimits:     keyLim,
		OnWait: func(e scheduler.WaitEvent) {
			need := fmt.Sprintf("need %d vCPU / %s memory / %s disk",
				cpuCount, formatBytes(memBytes), formatBytes(uint64(diskBytes)))
			if e.HostDiskLow {
				// Free capacity is not the reason here and reporting it would
				// send whoever reads this looking in the wrong place.
				s.sessionMgr.UpdateState(ctx, sessionID, session.StateQueued,
					fmt.Sprintf("Position %d in queue; host disk below reserve, admission paused (%s)",
						e.Position, need))
				return
			}
			s.sessionMgr.UpdateState(ctx, sessionID, session.StateQueued,
				fmt.Sprintf("Position %d in queue; %s", e.Position, need))
		},
	}
	admitStart := time.Now()
	lease, err := s.scheduler.Acquire(ctx, admitReq)
	if err != nil {
		if errors.Is(err, scheduler.ErrTooLarge) {
			log.Error("job exceeds scheduler capacity", "error", err)
			s.instruments.RecordAdmissionDenied(ctx, proj.Name, "too_large")
			s.failRun(termCtx, rootCtx, sessionID, fmt.Errorf("scheduler: job %d vCPU / %s memory / %s disk exceeds host capacity",
				cpuCount, formatBytes(memBytes), formatBytes(uint64(diskBytes))))
			return
		}
		if errors.Is(err, scheduler.ErrExceedsLimit) {
			// Queueing would hide a misconfiguration behind a job that never
			// starts, so this fails now and names the limit.
			log.Error("job exceeds a configured tenant limit", "error", err)
			s.instruments.RecordAdmissionDenied(ctx, proj.Name, "exceeds_limit")
			s.failRun(termCtx, rootCtx, sessionID, fmt.Errorf(
				"scheduler: job %d vCPU / %s memory / %s disk exceeds a configured limit: %w",
				cpuCount, formatBytes(memBytes), formatBytes(uint64(diskBytes)), err))
			return
		}
		if errors.Is(err, scheduler.ErrQueueFull) {
			// Backpressure, not a bad job: the same request would be taken
			// against a shorter queue, so the message says to retry.
			log.Warn("admission queue full", "error", err)
			s.instruments.RecordAdmissionDenied(ctx, proj.Name, "queue_full")
			s.failRun(termCtx, rootCtx, sessionID,
				errors.New("scheduler: admission queue is full; retry when the host has caught up"))
			return
		}
		log.Error("admission failed", "error", err)
		s.failRun(termCtx, rootCtx, sessionID, fmt.Errorf("admission: %w", err))
		return
	}
	defer lease.Release()

	waited := time.Since(admitStart)
	s.instruments.RecordAdmissionWait(ctx, proj.Name, coding.MetricsModeLabel(modeName), waited.Seconds())
	// The histogram gets every job; the log line is only for a wait an
	// operator would be looking into by hand — "why did this one take so long
	// to start" — which matches how every other wait here is logged.
	if waited > time.Second {
		log.Info("admitted after waiting for capacity", "duration", logging.Elapsed(admitStart))
	}

	// Load repo context (instructions + skills). Non-fatal on error.
	rc, err := repocontext.Load(cloneDir)
	if err != nil {
		log.Warn("failed to load repo context", "error", err)
		rc = &repocontext.RepoContext{}
	}

	// Resolve secrets declared in kvarn.yml. env-typed secrets become real
	// env vars in the VM; managed secrets are exposed as per-job placeholders
	// that the egress proxy substitutes for the real value (per scheme) just
	// before the request leaves the host.
	var secretEnv map[string]string
	var managed map[string]secret.Managed
	if cfg != nil && len(cfg.Secrets) > 0 {
		secretEnv, managed, err = secret.Resolve(ctx, s.secretStore, proj.Name, secretRefs(cfg.Secrets))
	}
	if err != nil {
		log.Error("failed to resolve secrets", "error", err)
		s.failRun(termCtx, rootCtx, sessionID, err)
		return
	}

	createOpts := s.createOpts
	if len(managed) > 0 {
		createOpts.Network.SecretInjector = egressproxy.NewPlaceholderInjector(managedSecrets(managed), log)
	}

	// Boot VM, transfer files, configure firewall/tools/container.
	create := s.sandboxFactory
	if create == nil {
		create = defaultSandboxFactory
	}
	sess, err := create(ctx, sandbox.Opts{
		Provider:   s.provider,
		CreateOpts: createOpts,
		Config:     cfg,
		Transferer: s.transferer,
		SourceDir:  cloneDir,
		// The clone is untouched at this point, so its worktree is exactly
		// HEAD: ship the repository alone and let the guest write the files.
		PristineClone:   true,
		WorkingDir:      s.workspaceDir,
		Registry:        s.registry,
		BridgeHandler:   s.bridgeHandler,
		RegistryMirrors: s.registryMirrors,
		CacheProvider:   s.cacheProvider,
		ProjectID:       cache.ProjectID(proj.RepoURL),
		Namespace:       s.cacheNamespace,
		Secrets:         secretEnv,
		OnEvent:         s.makeEventAdapter(ctx, sessionID),
	})
	if err != nil {
		log.Error("sandbox start failed", "error", err)
		s.failRun(termCtx, rootCtx, sessionID, err)
		return
	}
	defer sess.Close()

	// Run setup steps.
	if cfg != nil && (len(cfg.Setup.Steps) > 0 || len(cfg.Setup.HealthChecks) > 0) {
		log.Info("running setup steps")
		s.sessionMgr.UpdateState(ctx, sessionID, session.StateSetup, "Running setup")

		onStepDone := s.makeStepCallback(ctx, sessionID)
		onOutput := s.makeOutputCallback(ctx, sessionID)
		if _, err := sess.RunSetup(ctx, cfg, onStepDone, onOutput); err != nil {
			log.Error("setup failed", "error", err)
			s.failRun(termCtx, rootCtx, sessionID, fmt.Errorf("setup: %w", err))
			return
		}
		log.Info("setup complete")
	}

	// Hand off to agent.
	log.Info("handing off to agent")
	s.sessionMgr.UpdateState(ctx, sessionID, session.StateRunning, "Running agent")

	worklog := &worklogCollector{}

	agentCtx := &agent.Context{
		ProjectName: proj.Name,
		RepoURL:     proj.RepoURL,
		Branch:      branch,
		WorkingDir:  sess.GetWorkingDir(),
		SessionID:   sess.GetShellSessionID(),
		Prompt:      agentPrompt,
		Mode:        mode,
		Runner:      sess.GetRunner(),
		RepoContext: rc,
		Cost:        tracker,
		OnProgress: func(event agent.ProgressEvent) {
			switch e := event.(type) {
			case agent.ProgressToolUse:
				if e.AgentID == "" {
					s.sessionMgr.UpdateState(ctx, sessionID, session.StateRunning, e.ToolID)
				}
				worklog.appendToolUse(e.ToolID, shortArgs(e.ArgumentsJSON))
				s.sessionMgr.EmitEvent(ctx, sessionID, session.AgentToolUseEvent{
					SessionID:     sessionID,
					AgentID:       e.AgentID,
					ToolID:        e.ToolID,
					ArgumentsJSON: e.ArgumentsJSON,
				})
			case agent.ProgressToolResult:
				if e.IsError {
					worklog.appendToolError(e.ToolID, firstLine(e.Result))
				}
				s.sessionMgr.EmitEvent(ctx, sessionID, session.AgentToolResultEvent{
					SessionID: sessionID,
					AgentID:   e.AgentID,
					ToolID:    e.ToolID,
					Result:    e.Result,
					IsError:   e.IsError,
				})
			case agent.ProgressTextMessage:
				worklog.appendText(e.Text)
				s.sessionMgr.EmitEvent(ctx, sessionID, session.AgentMessageEvent{
					SessionID: sessionID,
					AgentID:   e.AgentID,
					Text:      e.Text,
					Final:     e.Final,
				})
			}
		},
	}

	// Run the agent; if it modifies files and the project has validation
	// steps, fold their pass/fail back into a single conversation so the
	// agent can fix what its own change broke. Each retry is gated by both
	// MaxValidationRetries and the shared cost budget.
	var conv agent.Conversation
	if s.agent != nil {
		conv, err = s.agent.Start(ctx, agentCtx)
		if err != nil {
			log.Error("agent start failed", "error", err)
			cause := context.Cause(rootCtx)
			if errors.Is(cause, cost.ErrBudgetExceeded) {
				s.failRun(termCtx, rootCtx, sessionID, fmt.Errorf("agent: %w", cause))
			} else {
				s.failRun(termCtx, rootCtx, sessionID, fmt.Errorf("agent: %w", err))
			}
			return
		}
		defer conv.Close()
	}

	// Whether the project's checks run is the mode's decision, not a
	// consequence of it writing files: a read-only mode may exist precisely to
	// run them against someone else's branch and report an honest verdict.
	validates := mode.Validation != coding.ValidationSkip && cfg != nil &&
		(len(cfg.Validation.Required) > 0 || len(cfg.Validation.Advisory) > 0)

	var valResult *sandbox.ValidationResult
	var agentText string
	// requiredFailed records a verdict the run has to deliver before it reports
	// it. Under `require` a red required step ends the run, but ending it here
	// would skip the delivery that is the entire output of a mode written to say
	// whether someone else's branch passes.
	requiredFailed := false
	for attempt := 0; ; attempt++ {
		followup := ""
		if attempt > 0 {
			followup = agent.BuildRetryPrompt(valResult, attempt, costLimits.MaxValidationRetries)
			s.sessionMgr.UpdateState(ctx, sessionID, session.StateRunning,
				fmt.Sprintf("Retrying after validation failure (attempt %d/%d)",
					attempt, costLimits.MaxValidationRetries))
		}

		if conv != nil {
			agentText, err = conv.Run(ctx, followup)
			// Persist partial cost regardless of success: spend up to a
			// failure is still interesting to users.
			s.sessionMgr.UpdateCost(termCtx, sessionID, tracker.Snapshot())
			if err != nil {
				log.Error("agent failed", "error", err)
				cause := context.Cause(rootCtx)
				if errors.Is(cause, cost.ErrBudgetExceeded) {
					s.failRun(termCtx, rootCtx, sessionID, fmt.Errorf("agent: %w", cause))
				} else {
					s.failRun(termCtx, rootCtx, sessionID, fmt.Errorf("agent: %w", err))
				}
				return
			}
		}

		if !validates {
			break
		}

		log.Info("running validation steps", "attempt", attempt+1)
		s.sessionMgr.UpdateState(ctx, sessionID, session.StateValidating, "Running validation")

		// Path-scoped steps are gated on the run's own diff, which only a mode
		// that writes one has. A read-only run leaves the list nil so every step
		// runs: gating it on an empty diff would skip each step that declares
		// `paths:` and report the pass those skips add up to.
		var changedFiles []string
		if mode.WritesChanges() {
			changedFiles, err = sess.ChangedFiles(ctx)
			if err != nil {
				log.Error("failed to get changed files", "error", err)
				s.failRun(termCtx, rootCtx, sessionID, fmt.Errorf("changed files: %w", err))
				return
			}
		}

		onStepDone := s.makeStepCallback(ctx, sessionID)
		onOutput := s.makeOutputCallback(ctx, sessionID)
		valResult, err = sess.RunValidation(ctx, cfg, changedFiles, onStepDone, onOutput)
		if err != nil {
			log.Error("validation failed", "error", err)
			s.failRun(termCtx, rootCtx, sessionID, fmt.Errorf("validation: %w", err))
			return
		}

		if valResult.RequiredPassed {
			log.Info("validation complete", "attempt", attempt+1)
			break
		}

		// `require` is a verdict, not a repair loop: it settles on the first red
		// required step, which is what lets a read-only mode report that someone
		// else's branch does not build. The run is already lost, so it breaks
		// out to deliver the verdict and fails once that has gone out.
		if mode.Validation == coding.ValidationRequire {
			log.Error("required validation steps failed")
			requiredFailed = true
			break
		}
		// A read-only run has nothing to fix and nothing to break, so under
		// `run` a red step is reported and the run continues to deliver
		// whatever the agent produced. A mode that wants the failure to count
		// asks for `require`.
		if mode.ReadOnly() {
			log.Warn("required validation steps failed in a read-only run; reporting without failing the job")
			break
		}

		if conv == nil || attempt >= costLimits.MaxValidationRetries {
			log.Error("required validation steps failed", "attempts", attempt+1)
			s.sessionMgr.Fail(termCtx, sessionID,
				fmt.Errorf("required validation steps failed after %d attempts", attempt+1))
			return
		}
		if tracker.OverBudget() {
			log.Error("required validation failed; cost budget exhausted")
			s.sessionMgr.Fail(termCtx, sessionID,
				fmt.Errorf("required validation steps failed; cost budget exhausted: %w",
					cost.ErrBudgetExceeded))
			return
		}
	}

	// Every run has a result, including a read-only one: for a mode that
	// commits it is the summary that becomes the commit message, and for one
	// that does not it is the agent's own final answer. Both are persisted, so
	// a mode that delivers nowhere is still readable afterwards.
	var agentResult *agent.Result
	switch {
	case conv == nil:
	case mode.WritesChanges():
		agentResult, err = conv.Summarize(ctx)
		if err != nil {
			log.Warn("failed to summarize agent run; using defaults", "error", err)
			agentResult = &agent.Result{
				Title:       "Apply agent changes",
				Description: "Automated changes by kvarn agent.",
			}
		}
	case strings.TrimSpace(agentText) != "":
		agentResult = &agent.Result{Description: agentText}
	}
	if agentResult != nil && agentResult.Description != "" {
		if err := s.sessionMgr.SetResult(termCtx, sessionID, agentResult.Description); err != nil {
			log.Warn("failed to record run result", "error", err)
		}
	}

	// Save cache (non-fatal on error).
	s.sessionMgr.UpdateState(ctx, sessionID, session.StateSetup, "Saving cache")
	if err := sess.SaveCache(ctx); err != nil {
		log.Warn("failed to save cache", "error", err)
	}
	// Opportunistic bounded LRU sweep after each save (non-fatal).
	if s.cacheProvider != nil && (s.cacheQuota.PerProjectBytes > 0 || s.cacheQuota.GlobalBytes > 0) {
		if report, err := s.cacheProvider.Evict(s.cacheQuota); err != nil {
			log.Warn("cache eviction failed", "error", err)
		} else if report.RemovedEntries > 0 {
			log.Info("cache evicted", "entries", report.RemovedEntries, "bytes_freed", report.BytesFreed)
		}
	}

	// Deliver the run's output wherever the mode says it goes.
	userPrompt := spec.userPrompt
	if userPrompt == "" {
		userPrompt = agentPrompt
	}
	submitErr := s.deliver(ctx, deliveryRequest{
		mode:        mode,
		sessionID:   sessionID,
		sandbox:     sess,
		forgeImpl:   forgeImpl,
		forgeCfg:    forgeCfg,
		proj:        proj,
		agentResult: agentResult,
		baseBranch:  branch,
		cloneURL:    cloneURL,
		cloneDir:    cloneDir,
		creds:       creds,
		pr:          spec.pr,
		userPrompt:  userPrompt,
		worklog:     worklog.snapshot(),

		valResult:        valResult,
		validationFailed: requiredFailed,
		cost:             tracker.Snapshot(),
		reportCost:       costLimits.ReportCostOnPR,
		log:              log,
	})

	// Final cost snapshot. The agent has already populated the session with
	// its partial snapshot above; this one captures any tail work, and the
	// CostEvent gives watchers a clear end-of-run summary. It is emitted before
	// the outcome is decided so a run that spent money and then failed to
	// submit still reports what it spent.
	finalReport := tracker.Snapshot()
	s.sessionMgr.UpdateCost(termCtx, sessionID, finalReport)
	s.sessionMgr.EmitEvent(termCtx, sessionID, session.CostEvent{
		SessionID: sessionID,
		Kind:      session.CostUpdateFinal,
		Report:    finalReport,
		Limit:     cost.Limit{MaxUSD: costLimits.MaxCostUSD, WarnFraction: costLimits.WarnThreshold},
	})

	// A run whose work never reached the forge is not a success: reporting it as
	// completed with an empty pull_request_url hides the failure from anyone
	// listing sessions.
	if submitErr != nil {
		log.Error("failed to submit changes", "error", submitErr)
		s.failRun(termCtx, rootCtx, sessionID, submitErr)
		return
	}

	// The verdict has been delivered, so the run can now report what it found.
	if requiredFailed {
		s.sessionMgr.Fail(termCtx, sessionID, errors.New("required validation steps failed"))
		return
	}

	log.Info("job completed successfully")
	s.sessionMgr.UpdateState(termCtx, sessionID, session.StateCompleted, "Completed")
}

// formatBytes renders a byte count using GiB/MiB units, matching the way kvarn.yml
// declares vm.memory/vm.disk so queue messages read consistently with the config.
func formatBytes(b uint64) string {
	const (
		mib = uint64(1024 * 1024)
		gib = uint64(1024) * mib
	)
	if b >= gib && b%gib == 0 {
		return fmt.Sprintf("%dG", b/gib)
	}
	if b >= gib {
		return fmt.Sprintf("%.1fG", float64(b)/float64(gib))
	}
	return fmt.Sprintf("%dM", b/mib)
}

// ccPrefix matches a leading Conventional Commit prefix: one of the recognized
// type words, an optional (scope), an optional ! breaking-change marker, and
// the colon. Restricting to known types avoids mangling ordinary titles that
// merely start with a "word:" (e.g. "Note: ...").
var ccPrefix = regexp.MustCompile(`(?i)^(?:feat|fix|docs|style|refactor|perf|test|build|ci|chore|revert)(\([^)]*\))?!?:\s*`)

// nonSlugChars matches runs of characters that aren't lowercase alphanumerics,
// so they can be collapsed into a single hyphen.
var nonSlugChars = regexp.MustCompile(`[^a-z0-9]+`)

const maxBranchSlugLen = 50

// branchSlug derives a human-readable, git-ref-safe branch component from a
// commit title. The Conventional Commit prefix is stripped because the branch
// already carries a namespace prefix, so the type/scope would only add noise.
// Returns "" when nothing usable remains, in which case the caller falls back
// to the session id alone.
func branchSlug(title string) string {
	s := ccPrefix.ReplaceAllString(title, "")
	s = strings.ToLower(s)
	s = nonSlugChars.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > maxBranchSlugLen {
		s = s[:maxBranchSlugLen]
		// Trim back to the last word boundary to avoid cutting mid-word,
		// unless the truncated slug is a single long word with no boundary.
		if i := strings.LastIndex(s, "-"); i > 0 {
			s = s[:i]
		}
		s = strings.Trim(s, "-")
	}
	return s
}

// submitChanges extracts changes from the VM, commits, pushes, and creates a PR.
// Everything up to and including PR creation is fatal to the run: without it
// the agent's work exists only inside a VM that is about to be torn down.
func (s *Service) submitChanges(
	ctx context.Context,
	sessionID string,
	sess Sandbox,
	forgeImpl forge.Forge,
	agentResult *agent.Result,
	proj *project.Project,
	forgeCfg *forgeconfig.ForgeConfig,
	branch string,
	cloneURL string,
	cloneDir string,
	creds scm.CredentialSource,
	prompt string,
	worklog []worklogEntry,
	costReport cost.Report,
	reportCostOnPR bool,
	log *slog.Logger,
) error {
	// Check if there are any changes.
	changedFiles, err := sess.ChangedFiles(ctx)
	if err != nil {
		return fmt.Errorf("check changed files for submission: %w", err)
	}
	if len(changedFiles) == 0 {
		log.Info("no changes to submit")
		return nil
	}

	title := agentResult.Title
	if title == "" {
		title = "Apply agent changes"
	}
	body := agentResult.Description

	s.sessionMgr.UpdateState(ctx, sessionID, session.StateSubmitting, "Submitting changes")

	// Extract changes from VM to host clone dir.
	if err := sess.ExtractChanges(ctx, cloneDir); err != nil {
		return fmt.Errorf("extract changes from VM: %w", err)
	}

	// Resolve behavioral settings by layering, highest precedence first:
	// per-project overrides, per-forge values, the global [defaults] block, and
	// the compiled-in constants.
	var forgeDefaults forgeconfig.Defaults
	if s.forgeDefaults != nil {
		if d, err := s.forgeDefaults.Defaults(ctx); err != nil {
			log.Warn("failed to load forge defaults; using built-ins", "error", err)
		} else {
			forgeDefaults = d
		}
	}
	behavior := forgeCfg.ResolveBehavior(forgeDefaults, forgeconfig.Overrides{
		BranchPrefix:      proj.BranchPrefix,
		CommitAuthorName:  proj.CommitAuthorName,
		CommitAuthorEmail: proj.CommitAuthorEmail,
		Labels:            proj.Labels,
	})
	prefix := behavior.BranchPrefix
	authorName := behavior.CommitAuthorName
	authorEmail := behavior.CommitAuthorEmail
	labels := behavior.Labels

	// Commit and push. The commit message and PR body are identical so the
	// PR shows the same content that lands as the merge commit.
	//
	// The branch name is derived from the commit title for readability, with a
	// short slice of the session id as a suffix to keep it unique and git-ref
	// safe. If the title yields no usable slug, fall back to the session id.
	prBranch := fmt.Sprintf("%s/%s", prefix, sessionID)
	if slug := branchSlug(title); slug != "" {
		suffix := sessionID
		if len(suffix) > 8 {
			suffix = suffix[:8]
		}
		prBranch = fmt.Sprintf("%s/%s-%s", prefix, slug, suffix)
	}
	commitMsg := title
	if body != "" {
		commitMsg = title + "\n\n" + body
	}

	if err := forgeImpl.SCM().CommitAndPush(ctx, scm.CommitAndPushOpts{
		RepoDir:     cloneDir,
		RemoteURL:   cloneURL,
		Branch:      prBranch,
		Message:     commitMsg,
		AuthorName:  authorName,
		AuthorEmail: authorEmail,
		Credentials: creds,
	}); err != nil {
		return fmt.Errorf("commit and push to %s: %w", prBranch, err)
	}
	s.recordMirrorPush(ctx, proj, cloneURL, cloneDir, prBranch, log)

	pr, err := forgeImpl.CreatePullRequest(ctx, forge.CreatePROpts{
		RepoURL:     cloneURL,
		BaseBranch:  branch,
		HeadBranch:  prBranch,
		Title:       title,
		Body:        body,
		Labels:      labels,
		Credentials: creds,
	})
	if err != nil {
		return fmt.Errorf("create pull request for %s: %w", prBranch, err)
	}

	log.Info("pull request created", "url", pr.URL, "ref", pr.Ref)
	// Persist the PR identity on the session (and broadcast a
	// PullRequestEvent) so GetSession returns it after the run, not just live
	// watchers. The ref is what a later feedback run is looked up by. The
	// pull request already exists on the forge at this point, so this write
	// runs uncancellable: losing the ref to a shutdown would leave the run
	// with no record of what it opened.
	s.sessionMgr.SetPullRequest(context.WithoutCancel(ctx), sessionID, pr.URL, pr.Ref, prBranch)

	// Post task + work log as a PR comment so it stays out of any
	// squash-merge commit message.
	commentBody := formatWorklogComment(prompt, worklog, reportCostOnPR, costReport)
	if err := forgeImpl.PostComment(ctx, forge.PostCommentOpts{
		RepoURL:     cloneURL,
		PRRef:       pr.Ref,
		Body:        commentBody,
		Credentials: creds,
	}); err != nil {
		// The PR exists and carries the change; a missing comment is cosmetic.
		log.Warn("failed to post task/work-log comment", "error", err)
	}
	return nil
}

// submitFollowup extracts changes from the VM and pushes them as a follow-up
// commit onto the pull request's existing head branch. No new PR is opened and
// the PR's title and body are left alone; what was addressed goes into a
// comment instead.
func (s *Service) submitFollowup(
	ctx context.Context,
	sessionID string,
	sess Sandbox,
	forgeImpl forge.Forge,
	agentResult *agent.Result,
	proj *project.Project,
	forgeCfg *forgeconfig.ForgeConfig,
	pr *prTarget,
	cloneURL string,
	cloneDir string,
	creds scm.CredentialSource,
	feedback string,
	worklog []worklogEntry,
	costReport cost.Report,
	reportCostOnPR bool,
	log *slog.Logger,
) error {
	// The VM cloned the PR head, so the changed set is the follow-up delta.
	changedFiles, err := sess.ChangedFiles(ctx)
	if err != nil {
		return fmt.Errorf("check changed files for submission: %w", err)
	}
	if len(changedFiles) == 0 {
		log.Info("no changes to submit")
		return nil
	}

	s.sessionMgr.UpdateState(ctx, sessionID, session.StateSubmitting, "Submitting changes")

	// Re-check the head before doing any work: the run started from a snapshot
	// of the branch, and pushing on top of someone else's newer commit would
	// silently revert it.
	current, err := forgeImpl.GetPullRequest(ctx, forge.GetPROpts{
		RepoURL:     cloneURL,
		PRRef:       pr.ref,
		Credentials: creds,
	})
	if err != nil {
		return fmt.Errorf("re-read pull request %s: %w", pr.ref, err)
	}
	if current.HeadSHA != pr.headSHA {
		return fmt.Errorf("pull request %s moved during the run (head %s -> %s); not pushing",
			pr.ref, pr.headSHA, current.HeadSHA)
	}

	if err := sess.ExtractChanges(ctx, cloneDir); err != nil {
		return fmt.Errorf("extract changes from VM: %w", err)
	}

	title := agentResult.Title
	if title == "" {
		title = "Address review feedback"
	}
	commitMsg := title
	if agentResult.Description != "" {
		commitMsg = title + "\n\n" + agentResult.Description
	}

	var forgeDefaults forgeconfig.Defaults
	if s.forgeDefaults != nil {
		if d, err := s.forgeDefaults.Defaults(ctx); err != nil {
			log.Warn("failed to load forge defaults; using built-ins", "error", err)
		} else {
			forgeDefaults = d
		}
	}
	behavior := forgeCfg.ResolveBehavior(forgeDefaults, forgeconfig.Overrides{
		BranchPrefix:      proj.BranchPrefix,
		CommitAuthorName:  proj.CommitAuthorName,
		CommitAuthorEmail: proj.CommitAuthorEmail,
		Labels:            proj.Labels,
	})

	// Pushing the PR's own branch name is a fast-forward of one new commit.
	if err := forgeImpl.SCM().CommitAndPush(ctx, scm.CommitAndPushOpts{
		RepoDir:     cloneDir,
		RemoteURL:   cloneURL,
		Branch:      pr.headBranch,
		Message:     commitMsg,
		AuthorName:  behavior.CommitAuthorName,
		AuthorEmail: behavior.CommitAuthorEmail,
		Credentials: creds,
	}); err != nil {
		return fmt.Errorf("commit and push to %s: %w", pr.headBranch, err)
	}
	s.recordMirrorPush(ctx, proj, cloneURL, cloneDir, pr.headBranch, log)

	log.Info("follow-up commit pushed", "branch", pr.headBranch, "url", pr.url)

	commentBody := formatFollowupComment(feedback, agentResult.Description, worklog, reportCostOnPR, costReport)
	if err := forgeImpl.PostComment(ctx, forge.PostCommentOpts{
		RepoURL:     cloneURL,
		PRRef:       pr.ref,
		Body:        commentBody,
		Credentials: creds,
	}); err != nil {
		log.Warn("failed to post follow-up comment", "error", err)
	}
	return nil
}

// makeEventAdapter translates sandbox Events to session state updates.
func (s *Service) makeEventAdapter(ctx context.Context, sessionID string) func(sandbox.Event) {
	return func(e sandbox.Event) {
		var state session.State
		var message string
		switch ev := e.(type) {
		case sandbox.ProvisioningEvent:
			state = session.StateProvisioning
			message = "Provisioning VM"
		case sandbox.ProvisionedEvent:
			if ev.VmInfo != nil {
				s.sessionMgr.EmitEvent(ctx, sessionID, session.VmInfoEvent{
					SessionID:   sessionID,
					CpuCount:    ev.VmInfo.CpuCount,
					CpuModel:    ev.VmInfo.CpuModel,
					MemTotalMB:  ev.VmInfo.MemTotalMb,
					MemAvailMB:  ev.VmInfo.MemAvailableMb,
					DiskUsedMB:  ev.VmInfo.DiskUsedMb,
					DiskTotalMB: ev.VmInfo.DiskTotalMb,
				})
			}
			return
		case sandbox.TransferringEvent:
			state = session.StateTransferring
			message = "Transferring files"
		case sandbox.TransferProgressEvent:
			s.sessionMgr.EmitEvent(ctx, sessionID, session.TransferProgressEvent{
				SessionID:  sessionID,
				BytesSent:  ev.BytesSent,
				TotalBytes: ev.TotalBytes,
			})
			return
		case sandbox.DependenciesInstallingEvent:
			state = session.StateInstallingDependencies
			message = "Installing dependencies"
		case sandbox.DependenciesInstalledEvent:
			return
		case sandbox.DependencyOutputEvent:
			s.sessionMgr.EmitEvent(ctx, sessionID, session.DependencyOutputEvent{
				SessionID: sessionID,
				Stdout:    ev.Stdout,
				Stderr:    ev.Stderr,
			})
			return
		case sandbox.ImagePullingEvent:
			state = session.StatePullingImage
			message = fmt.Sprintf("Pulling image %s", ev.Image)
		case sandbox.ContainerStartingEvent:
			state = session.StatePullingImage
			message = "Starting container"
		case sandbox.ContainerStartedEvent:
			state = session.StatePullingImage
			message = "Container started"
		case sandbox.CacheRestoringEvent:
			state = session.StateSetup
			message = "Restoring cache"
		case sandbox.CacheProgressEvent:
			s.sessionMgr.EmitEvent(ctx, sessionID, session.CacheProgressEvent{
				SessionID: sessionID,
				Path:      ev.Path,
				Index:     ev.Index,
				Total:     ev.Total,
				Restoring: ev.Restoring,
			})
			return
		case sandbox.CacheRestoredEvent:
			state = session.StateSetup
			message = "Cache restored"
		case sandbox.CacheSavingEvent:
			state = session.StateSetup
			message = "Saving cache"
		case sandbox.CacheSavedEvent:
			state = session.StateSetup
			message = "Cache saved"
		case sandbox.SessionCreatingEvent:
			state = session.StateSetup
			message = "Creating shell session"
		case sandbox.SessionCreatedEvent:
			state = session.StateSetup
			message = "Shell session created"
		case sandbox.ConsoleOutputEvent:
			s.sessionMgr.EmitEvent(ctx, sessionID, session.ConsoleOutputEvent{
				SessionID: sessionID,
				Output:    ev.Output,
			})
			return
		default:
			return
		}
		s.sessionMgr.UpdateState(ctx, sessionID, state, message)
	}
}

func (s *Service) GetSession(ctx context.Context, req *connect.Request[v1.GetSessionRequest]) (*connect.Response[v1.GetSessionResponse], error) {
	if s.sessionMgr == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("sessions not configured"))
	}

	sess, err := s.sessionMgr.Get(ctx, req.Msg.SessionId)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}

	if err := s.authorizeProject(ctx, sess.ProjectName, req.Spec().Procedure); err != nil {
		return nil, err
	}

	return connect.NewResponse(sessionToProto(sess)), nil
}

// GetSessionResult returns what a run produced in writing. It is how a mode
// that delivers nowhere is read: the answer a research run wrote, or the review
// a read-only audit produced, without replaying the whole event log to find the
// final assistant message.
func (s *Service) GetSessionResult(ctx context.Context, req *connect.Request[v1.GetSessionResultRequest]) (*connect.Response[v1.GetSessionResultResponse], error) {
	if s.sessionMgr == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("sessions not configured"))
	}

	sess, err := s.sessionMgr.Get(ctx, req.Msg.SessionId)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}

	if err := s.authorizeProject(ctx, sess.ProjectName, req.Spec().Procedure); err != nil {
		return nil, err
	}

	return connect.NewResponse(&v1.GetSessionResultResponse{
		SessionId: sess.ID,
		State:     string(sess.State),
		Result:    sess.Result,
	}), nil
}

// sessionToProto renders a session as the wire message shared by GetSession
// and ListSessions.
func sessionToProto(sess *session.Session) *v1.GetSessionResponse {
	return &v1.GetSessionResponse{
		SessionId:       sess.ID,
		Project:         sess.ProjectName,
		State:           string(sess.State),
		Message:         sess.Message,
		Error:           sess.Error,
		Prompt:          sess.Prompt,
		PullRequestUrl:  sess.PullRequestURL,
		Mode:            sess.Mode,
		Cost:            costReportToProto(sess.Cost),
		PrRef:           sess.PRRef,
		HeadBranch:      sess.HeadBranch,
		BaseBranch:      sess.BaseBranch,
		ParentSessionId: sess.ParentSessionID,
		Continuation:    sess.Continuation,
		CreatedAt:       timestamppb.New(sess.CreatedAt),
		UpdatedAt:       timestamppb.New(sess.UpdatedAt),
		QueuedAt:        timestamppb.New(sess.QueuedAt),
		Priority:        int32(sess.Priority),
		Attempts:        int32(sess.Attempts),
		Metadata:        sess.Metadata,
	}
}

// parseStates resolves the state names on a filtering request. An unknown name
// is refused rather than matched against nothing, so a misspelled state is a
// visible error instead of an empty listing.
func parseStates(names []string) ([]session.State, error) {
	if len(names) == 0 {
		return nil, nil
	}
	states := make([]session.State, 0, len(names))
	for _, name := range names {
		st, err := session.ParseState(name)
		if err != nil {
			return nil, err
		}
		states = append(states, st)
	}
	return states, nil
}

// Server-side bounds for ListSessions paging.
const (
	defaultSessionsLimit = 100
	maxSessionsLimit     = 500
)

func (s *Service) ListSessions(ctx context.Context, req *connect.Request[v1.ListSessionsRequest]) (*connect.Response[v1.ListSessionsResponse], error) {
	if s.sessionMgr == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("sessions not configured"))
	}

	limit := int(req.Msg.Limit)
	if limit <= 0 {
		limit = defaultSessionsLimit
	}
	if limit > maxSessionsLimit {
		limit = maxSessionsLimit
	}

	cursorTime, cursorID, err := decodePageCursor(req.Msg.PageCursor)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("page_cursor: %w", err))
	}

	states, err := parseStates(req.Msg.States)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	var createdAfter time.Time
	if ts := req.Msg.CreatedAfter; ts != nil {
		createdAfter = ts.AsTime()
	}

	// When auth is enabled, restrict the listing to the projects the key
	// covers. A missing identity (unreachable behind the interceptor) yields
	// an empty list rather than an error.
	id, hasIdentity := auth.IdentityFrom(ctx)
	allowed := func(projectName string) bool {
		if !s.authEnabled {
			return true
		}
		return hasIdentity && id.AllowsProject(projectName)
	}

	// Over-fetch and re-page: the identity post-filter can drop rows from the
	// middle of a store page, so keep pulling batches (advancing the keyset
	// cursor) until we have a full page of authorized rows or the store is
	// exhausted. next_page_cursor points at the last *included* row.
	var (
		included []*session.Session
		lastRow  *session.Session
	)
	for len(included) < limit {
		batch, err := s.sessionMgr.List(ctx, session.SessionFilter{
			Project:        req.Msg.Project,
			PRRef:          req.Msg.PrRef,
			Mode:           req.Msg.Mode,
			States:         states,
			ActiveOnly:     req.Msg.ActiveOnly,
			CreatedAfter:   createdAfter,
			Metadata:       req.Msg.GetMetadata(),
			Limit:          limit,
			AfterCreatedAt: cursorTime,
			AfterID:        cursorID,
		})
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		if len(batch) == 0 {
			break
		}
		for _, sess := range batch {
			cursorTime, cursorID = sess.CreatedAt, sess.ID
			if !allowed(sess.ProjectName) {
				continue
			}
			included = append(included, sess)
			lastRow = sess
			if len(included) == limit {
				break
			}
		}
		if len(batch) < limit {
			// Store returned a short page: no more rows to fetch.
			break
		}
	}

	resp := make([]*v1.GetSessionResponse, 0, len(included))
	for _, sess := range included {
		resp = append(resp, sessionToProto(sess))
	}

	nextCursor := ""
	if len(included) == limit && lastRow != nil {
		nextCursor = encodePageCursor(lastRow.CreatedAt, lastRow.ID)
	}

	return connect.NewResponse(&v1.ListSessionsResponse{
		Sessions:       resp,
		NextPageCursor: nextCursor,
	}), nil
}

// encodePageCursor / decodePageCursor serialize the (created_at, id) keyset
// cursor as an opaque base64 token. created_at is carried as unix micros UTC to
// match the store ordering exactly.
func encodePageCursor(createdAt time.Time, id string) string {
	raw := fmt.Sprintf("%d|%s", createdAt.UTC().UnixMicro(), id)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodePageCursor(cursor string) (time.Time, string, error) {
	if cursor == "" {
		return time.Time{}, "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("invalid cursor encoding")
	}
	sep := strings.IndexByte(string(raw), '|')
	if sep < 0 {
		return time.Time{}, "", fmt.Errorf("malformed cursor")
	}
	micros, err := strconv.ParseInt(string(raw[:sep]), 10, 64)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("malformed cursor timestamp")
	}
	return time.UnixMicro(micros).UTC(), string(raw[sep+1:]), nil
}

func (s *Service) WatchSession(ctx context.Context, req *connect.Request[v1.WatchSessionRequest], stream *connect.ServerStream[v1.SessionUpdate]) error {
	if s.sessionMgr == nil {
		return connect.NewError(connect.CodeUnimplemented, errors.New("sessions not configured"))
	}

	// Resolve the session first so we can authorize against its project before
	// streaming any events.
	sess, err := s.sessionMgr.Get(ctx, req.Msg.SessionId)
	if err != nil {
		return connect.NewError(connect.CodeNotFound, err)
	}
	if err := s.authorizeProject(ctx, sess.ProjectName, req.Spec().Procedure); err != nil {
		return err
	}

	ch, err := s.sessionMgr.Watch(ctx, req.Msg.SessionId, req.Msg.FromSequence)
	if err != nil {
		return connect.NewError(connect.CodeNotFound, err)
	}

	for we := range ch {
		update := sessionEventToUpdate(we.Seq, we.Event)
		if update != nil {
			if err := stream.Send(update); err != nil {
				return err
			}
		}
	}

	return nil
}

func (s *Service) ListSessionEvents(ctx context.Context, req *connect.Request[v1.ListSessionEventsRequest]) (*connect.Response[v1.ListSessionEventsResponse], error) {
	if s.sessionMgr == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("sessions not configured"))
	}

	sess, err := s.sessionMgr.Get(ctx, req.Msg.SessionId)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if err := s.authorizeProject(ctx, sess.ProjectName, req.Spec().Procedure); err != nil {
		return nil, err
	}

	limit := int(req.Msg.Limit)
	if limit <= 0 {
		limit = defaultSessionEventsLimit
	}
	if limit > maxSessionEventsLimit {
		limit = maxSessionEventsLimit
	}

	events, err := s.sessionMgr.ListEvents(ctx, req.Msg.SessionId, req.Msg.AfterSequence, limit)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	var (
		updates []*v1.SessionUpdate
		lastSeq int64
	)
	for _, we := range events {
		update := sessionEventToUpdate(we.Seq, we.Event)
		if update == nil {
			continue
		}
		updates = append(updates, update)
		lastSeq = we.Seq
	}

	return connect.NewResponse(&v1.ListSessionEventsResponse{
		Events:       updates,
		LastSequence: lastSeq,
	}), nil
}

// Server-side bounds for ListSessionEvents polling.
const (
	defaultSessionEventsLimit = 500
	maxSessionEventsLimit     = 2000
)

// sessionEventToUpdate converts an internal session Event (with its durable
// sequence; 0 for ephemeral) into the proto SessionUpdate. Returns nil for
// events that have no wire representation. Shared by WatchSession streaming and
// ListSessionEvents polling.
func sessionEventToUpdate(seq int64, event session.Event) *v1.SessionUpdate {
	var update *v1.SessionUpdate
	switch e := event.(type) {
	case session.StateChangeEvent:
		update = &v1.SessionUpdate{
			SessionId: e.Session.ID,
			Project:   e.Session.ProjectName,
			Event: &v1.SessionUpdate_StateChange{
				StateChange: &v1.StateChange{
					State:   string(e.Session.State),
					Message: e.Session.Message,
					Error:   e.Session.Error,
				},
			},
		}
	case session.AgentMessageEvent:
		update = &v1.SessionUpdate{
			SessionId: e.SessionID,
			Event: &v1.SessionUpdate_AgentMessage{
				AgentMessage: &v1.AgentMessage{
					Text:    e.Text,
					Final:   e.Final,
					AgentId: e.AgentID,
				},
			},
		}
	case session.AgentToolUseEvent:
		update = &v1.SessionUpdate{
			SessionId: e.SessionID,
			Event: &v1.SessionUpdate_AgentToolUse{
				AgentToolUse: &v1.AgentToolUse{
					ToolId:        e.ToolID,
					ArgumentsJson: e.ArgumentsJSON,
					AgentId:       e.AgentID,
				},
			},
		}
	case session.AgentToolResultEvent:
		update = &v1.SessionUpdate{
			SessionId: e.SessionID,
			Event: &v1.SessionUpdate_AgentToolResult{
				AgentToolResult: &v1.AgentToolResult{
					ToolId:  e.ToolID,
					Result:  e.Result,
					IsError: e.IsError,
					AgentId: e.AgentID,
				},
			},
		}
	case session.StepResultEvent:
		update = &v1.SessionUpdate{
			SessionId: e.SessionID,
			Event: &v1.SessionUpdate_StepResult{
				StepResult: &v1.StepResult{
					Name:     e.Name,
					Phase:    stepPhaseToProto(e.Phase),
					ExitCode: e.ExitCode,
					Stdout:   e.Stdout,
					Stderr:   e.Stderr,
					Passed:   e.Passed,
					Skipped:  e.Skipped,
				},
			},
		}
	case session.StepOutputEvent:
		update = &v1.SessionUpdate{
			SessionId: e.SessionID,
			Event: &v1.SessionUpdate_StepOutput{
				StepOutput: &v1.StepOutput{
					Name:   e.Name,
					Phase:  stepPhaseToProto(e.Phase),
					Stdout: e.Stdout,
					Stderr: e.Stderr,
				},
			},
		}
	case session.VmInfoEvent:
		update = &v1.SessionUpdate{
			SessionId: e.SessionID,
			Event: &v1.SessionUpdate_VmInfo{
				VmInfo: &v1.VmInfo{
					CpuCount:       e.CpuCount,
					CpuModel:       e.CpuModel,
					MemTotalMb:     e.MemTotalMB,
					MemAvailableMb: e.MemAvailMB,
					DiskUsedMb:     e.DiskUsedMB,
					DiskTotalMb:    e.DiskTotalMB,
				},
			},
		}
	case session.TransferProgressEvent:
		update = &v1.SessionUpdate{
			SessionId: e.SessionID,
			Event: &v1.SessionUpdate_TransferProgress{
				TransferProgress: &v1.TransferProgress{
					BytesSent:  e.BytesSent,
					TotalBytes: e.TotalBytes,
				},
			},
		}
	case session.DependencyOutputEvent:
		update = &v1.SessionUpdate{
			SessionId: e.SessionID,
			Event: &v1.SessionUpdate_DependencyOutput{
				DependencyOutput: &v1.DependencyOutput{
					Stdout: e.Stdout,
					Stderr: e.Stderr,
				},
			},
		}
	case session.CacheProgressEvent:
		update = &v1.SessionUpdate{
			SessionId: e.SessionID,
			Event: &v1.SessionUpdate_CacheProgress{
				CacheProgress: &v1.CacheProgress{
					Path:      e.Path,
					Index:     int32(e.Index),
					Total:     int32(e.Total),
					Restoring: e.Restoring,
				},
			},
		}
	case session.PullRequestEvent:
		update = &v1.SessionUpdate{
			SessionId: e.SessionID,
			Event: &v1.SessionUpdate_PullRequestCreated{
				PullRequestCreated: &v1.PullRequestCreated{
					Url:    e.URL,
					Ref:    e.Ref,
					Branch: e.Branch,
				},
			},
		}
	case session.CostEvent:
		update = &v1.SessionUpdate{
			SessionId: e.SessionID,
			Event: &v1.SessionUpdate_CostUpdate{
				CostUpdate: &v1.CostUpdate{
					Kind:         costKindToProto(e.Kind),
					Report:       costReportToProto(e.Report),
					LimitUsd:     e.Limit.MaxUSD,
					WarnFraction: e.Limit.WarnFraction,
				},
			},
		}
	}
	if update != nil {
		update.Sequence = seq
	}
	return update
}

// BridgeHandler returns the dispatch.Handler for this service, which implements
// kvarnv1connect.BridgeServiceHandler.
func (s *Service) BridgeHandler() *dispatch.Handler {
	return s.bridgeHandler
}

// makeOutputCallback creates an OnOutput callback that emits StepOutputEvents.
func (s *Service) makeOutputCallback(ctx context.Context, sessionID string) sandbox.OnOutput {
	return func(stepName string, phase string, stdout string, stderr string) {
		var sp session.StepPhase
		switch phase {
		case "setup":
			sp = session.StepPhaseSetup
		case "health_check":
			sp = session.StepPhaseHealthCheck
		case "validation_required":
			sp = session.StepPhaseValidationRequired
		case "validation_advisory":
			sp = session.StepPhaseValidationAdvisory
		}

		s.sessionMgr.EmitEvent(ctx, sessionID, session.StepOutputEvent{
			SessionID: sessionID,
			Name:      stepName,
			Phase:     sp,
			Stdout:    stdout,
			Stderr:    stderr,
		})
	}
}

// makeStepCallback creates an OnStepDone callback that emits StepResultEvents.
func (s *Service) makeStepCallback(ctx context.Context, sessionID string) sandbox.OnStepDone {
	return func(result sandbox.StepResult, phase string) {
		var sp session.StepPhase
		switch phase {
		case "setup":
			sp = session.StepPhaseSetup
		case "health_check":
			sp = session.StepPhaseHealthCheck
		case "validation_required":
			sp = session.StepPhaseValidationRequired
		case "validation_advisory":
			sp = session.StepPhaseValidationAdvisory
		}

		passed := result.ExitCode == 0 && result.Err == nil
		s.sessionMgr.EmitEvent(ctx, sessionID, session.StepResultEvent{
			SessionID: sessionID,
			Name:      result.Name,
			Phase:     sp,
			ExitCode:  result.ExitCode,
			Stdout:    result.Stdout,
			Stderr:    result.Stderr,
			Passed:    passed,
			Skipped:   result.Skipped,
		})
	}
}

// costReportToProto converts an internal cost.Report into its proto shape.
// Returns nil for the zero report so the wire format stays unset when no
// spend was recorded.
func costReportToProto(r cost.Report) *v1.CostReport {
	if r.InputTokens == 0 && r.OutputTokens == 0 && r.CachedTokens == 0 && r.TotalUSD == 0 && len(r.PerModel) == 0 {
		return nil
	}
	ids := make([]string, 0, len(r.PerModel))
	for id := range r.PerModel {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	perModel := make([]*v1.ModelCost, 0, len(ids))
	for _, id := range ids {
		m := r.PerModel[id]
		perModel = append(perModel, &v1.ModelCost{
			ModelId:      m.ModelID,
			InputTokens:  m.InputTokens,
			OutputTokens: m.OutputTokens,
			CachedTokens: m.CachedTokens,
			TotalUsd:     m.TotalUSD,
		})
	}
	return &v1.CostReport{
		InputTokens:  r.InputTokens,
		OutputTokens: r.OutputTokens,
		CachedTokens: r.CachedTokens,
		TotalUsd:     r.TotalUSD,
		PerModel:     perModel,
	}
}

func costKindToProto(k session.CostUpdateKind) v1.CostUpdateKind {
	switch k {
	case session.CostUpdateWarning:
		return v1.CostUpdateKind_COST_UPDATE_KIND_WARNING
	case session.CostUpdateOverBudget:
		return v1.CostUpdateKind_COST_UPDATE_KIND_OVER_BUDGET
	case session.CostUpdateFinal:
		return v1.CostUpdateKind_COST_UPDATE_KIND_FINAL
	default:
		return v1.CostUpdateKind_COST_UPDATE_KIND_UNSPECIFIED
	}
}

// secretRefs maps kvarn.yml secret declarations to resolution refs. The two
// types are deliberately decoupled (project knows nothing about resolution);
// the mapping lives at the call site.
func secretRefs(refs []projconfig.SecretRef) []secret.Ref {
	out := make([]secret.Ref, len(refs))
	for i, r := range refs {
		out[i] = secret.Ref{Name: r.Name, Scheme: r.Scheme, Hosts: r.Hosts}
	}
	return out
}

// managedSecrets translates resolved managed secrets into the proxy's
// injector input.
func managedSecrets(m map[string]secret.Managed) map[string]egressproxy.ManagedSecret {
	out := make(map[string]egressproxy.ManagedSecret, len(m))
	for ph, ms := range m {
		out[ph] = egressproxy.ManagedSecret{
			Value:  ms.Value,
			Scheme: egressproxy.Scheme(ms.Scheme),
			Hosts:  ms.Hosts,
		}
	}
	return out
}

func stepPhaseToProto(sp session.StepPhase) v1.StepPhase {
	switch sp {
	case session.StepPhaseSetup:
		return v1.StepPhase_STEP_PHASE_SETUP
	case session.StepPhaseHealthCheck:
		return v1.StepPhase_STEP_PHASE_HEALTH_CHECK
	case session.StepPhaseValidationRequired:
		return v1.StepPhase_STEP_PHASE_VALIDATION_REQUIRED
	case session.StepPhaseValidationAdvisory:
		return v1.StepPhase_STEP_PHASE_VALIDATION_ADVISORY
	default:
		return v1.StepPhase_STEP_PHASE_UNSPECIFIED
	}
}
