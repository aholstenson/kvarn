package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"

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
	"github.com/aholstenson/kvarn/internal/orchestrator/auth"
	"github.com/aholstenson/kvarn/internal/orchestrator/scheduler"
	projconfig "github.com/aholstenson/kvarn/internal/project"
	"github.com/aholstenson/kvarn/internal/sandbox"
	"github.com/aholstenson/kvarn/internal/sandbox/cache"
	"github.com/aholstenson/kvarn/internal/sandbox/transfer"
	"github.com/aholstenson/kvarn/internal/scm"
	gitscm "github.com/aholstenson/kvarn/internal/scm/git"
	"github.com/aholstenson/kvarn/internal/session"
	"github.com/aholstenson/kvarn/internal/vm"
	llms "github.com/aholstenson/llms-go"
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
	if len(entries) > 0 {
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
	if includeCost && (report.InputTokens > 0 || report.OutputTokens > 0 || report.TotalUSD > 0) {
		sb.WriteString("\n\n")
		sb.WriteString(formatCostSection(report))
	}
	return sb.String()
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
	sandboxFactory   SandboxFactory         // optional; nil uses defaultSandboxFactory
	defaultsStore    modelcfg.DefaultsStore // optional; nil means built-in fallbacks only
	pricingManager   *llms.PricingManager   // optional; nil disables USD computation
	apiKeyStore      apikey.Store           // API keys for request authentication
	authEnabled      bool                   // when true, project-scoped RPCs require an authorized key
	scheduler        *scheduler.Scheduler   // resource admission; never nil (defaults to unbounded)

	// Job lifecycle. shutdownCtx is the parent of every runJob root context;
	// Shutdown cancels it to wind down in-flight jobs and waits on jobsWG so
	// each Sandbox.Close gets a chance to tear its VM down via the bounded
	// stop path.
	jobsWG         sync.WaitGroup
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc
}

type ServiceOpts struct {
	Provider           vm.Provider
	CreateOpts         vm.CreateOpts
	ProjectStore       project.Store
	CredentialStore    credential.Store
	SecretStore        secret.Store
	ForgeConfigStore   forgeconfig.Store
	ForgeDefaultsStore forgeconfig.DefaultsStore // optional; nil means built-in fallbacks only
	ForgeTypes         map[string]forge.Forge
	SessionMgr         session.Manager
	Agent              agent.Agent
	Transferer         transfer.Transferer
	WorkspaceDir       string                 // VM workspace path; defaults to "/home/kvarn/workspace"
	RegistryMirrors    []string               // Docker registry mirrors (infrastructure config)
	CacheProvider      cache.Provider         // optional cache provider
	CacheQuota         cache.Quota            // LRU sweep limits; zero fields = unbounded
	Namespace          string                 // cache namespace; "" is the shared pool
	SandboxFactory     SandboxFactory         // optional; nil uses defaultSandboxFactory
	DefaultsStore      modelcfg.DefaultsStore // optional; nil means no user defaults (built-ins only)
	PricingManager     *llms.PricingManager   // optional; nil disables USD computation
	APIKeyStore        apikey.Store           // API keys for request authentication
	AuthEnabled        bool                   // when true, project-scoped RPCs require an authorized key
	Scheduler          *scheduler.Scheduler   // optional; nil means unbounded (no admission control)
}

func NewService(p vm.Provider, createOpts vm.CreateOpts) *Service {
	reg := dispatch.NewRegistry()
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	return &Service{
		provider:       p,
		registry:       reg,
		bridgeHandler:  dispatch.NewHandler(reg),
		createOpts:     createOpts,
		scheduler:      scheduler.NewUnbounded(),
		shutdownCtx:    shutdownCtx,
		shutdownCancel: shutdownCancel,
	}
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
	reg := dispatch.NewRegistry()
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	return &Service{
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
		sandboxFactory:   opts.SandboxFactory,
		defaultsStore:    opts.DefaultsStore,
		pricingManager:   opts.PricingManager,
		apiKeyStore:      opts.APIKeyStore,
		authEnabled:      opts.AuthEnabled,
		scheduler:        sched,
		shutdownCtx:      shutdownCtx,
		shutdownCancel:   shutdownCancel,
	}
}

// Shutdown signals every in-flight runJob to wind down and waits for them to
// return, bounded by ctx. The per-job `defer sandbox.Close()` chains run the
// bounded VM-stop path so VMs are torn down rather than orphaned. Callers
// typically pass a context with a deadline (see shutdownTimeout in run).
func (s *Service) Shutdown(ctx context.Context) {
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

// authorizeProject enforces that the authenticated caller is allowed to act on
// the given project. It is a no-op when auth is disabled (local dev). When auth
// is enabled the interceptor has already injected an Identity; a missing one is
// treated as unauthenticated, and a project the key does not cover is denied.
func (s *Service) authorizeProject(ctx context.Context, project string) error {
	if !s.authEnabled {
		return nil
	}
	id, ok := auth.IdentityFrom(ctx)
	if !ok {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("missing identity"))
	}
	if !id.AllowsProject(project) {
		return connect.NewError(connect.CodePermissionDenied,
			fmt.Errorf("key %q not allowed for project %q", id.KeyName, project))
	}
	return nil
}

func (s *Service) StartJob(ctx context.Context, req *connect.Request[v1.StartJobRequest]) (*connect.Response[v1.StartJobResponse], error) {
	msg := req.Msg

	if err := s.authorizeProject(ctx, msg.Project); err != nil {
		return nil, err
	}

	if s.projectStore == nil || s.sessionMgr == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("project-aware jobs not configured"))
	}

	slog.Info("starting job", "project", msg.Project, "branch", msg.Branch, "mode", msg.Mode)

	mode, err := coding.ModeByName(msg.Mode)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	proj, err := s.projectStore.Get(ctx, msg.Project)
	if err != nil {
		slog.Error("project not found", "project", msg.Project, "error", err)
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("project %q: %w", msg.Project, err))
	}

	slog.Info("resolved project", "project", proj.Name, "repo", proj.RepoURL, "forge", proj.Forge)

	sess, err := s.sessionMgr.Create(ctx, msg.Project, msg.Prompt, mode.ModeName())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create session: %w", err))
	}

	branch := msg.Branch
	if branch == "" {
		branch = proj.DefaultBranch
	}

	slog.Info("session created", "session_id", sess.ID, "branch", branch, "mode", mode.ModeName())

	s.jobsWG.Add(1)
	go s.runJob(sess.ID, proj, branch, msg.Prompt, mode)

	return connect.NewResponse(&v1.StartJobResponse{
		SessionId: sess.ID,
	}), nil
}

func (s *Service) runJob(sessionID string, proj *project.Project, branch string, prompt string, mode *coding.Mode) {
	defer s.jobsWG.Done()
	rootCtx, cancelJob := context.WithCancelCause(s.shutdownCtx)
	defer cancelJob(nil)
	ctx := rootCtx
	log := slog.With("session_id", sessionID, "project", proj.Name, "mode", mode.ModeName())

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
	costLimits := limits.Resolve(proj, userDefaults, mode.ModeName())
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

	// Resolve forge config, forge impl, and credentials.
	var forgeCfg *forgeconfig.ForgeConfig
	var forgeImpl forge.Forge
	var creds *scm.Credentials
	var cloneURL string

	if proj.Forge != "" && s.forgeConfigStore != nil {
		var err error
		forgeCfg, err = s.forgeConfigStore.Get(ctx, proj.Forge)
		if err != nil {
			log.Error("failed to load forge config", "forge", proj.Forge, "error", err)
			s.sessionMgr.Fail(ctx, sessionID, fmt.Errorf("load forge config %q: %w", proj.Forge, err))
			return
		}

		if s.forgeTypes != nil {
			forgeImpl = s.forgeTypes[forgeCfg.Type]
		}
		if forgeImpl == nil {
			log.Error("unknown forge type", "type", forgeCfg.Type)
			s.sessionMgr.Fail(ctx, sessionID, fmt.Errorf("unknown forge type %q", forgeCfg.Type))
			return
		}

		// Resolve clone URL via forge.
		cloneURL, err = forgeImpl.ResolveCloneURL(proj.RepoURL)
		if err != nil {
			log.Error("failed to resolve clone URL", "repo", proj.RepoURL, "error", err)
			s.sessionMgr.Fail(ctx, sessionID, fmt.Errorf("resolve clone URL: %w", err))
			return
		}

		// Resolve credentials.
		if forgeCfg.Credential != "" && s.credentialStore != nil {
			cred, err := s.credentialStore.Get(ctx, forgeCfg.Credential)
			if err != nil {
				log.Error("failed to load credentials", "credential", forgeCfg.Credential, "error", err)
				s.sessionMgr.Fail(ctx, sessionID, fmt.Errorf("load credential %q: %w", forgeCfg.Credential, err))
				return
			}

			creds, err = forgeImpl.ResolveCredentials(ctx, cred.Config)
			if err != nil {
				log.Error("failed to resolve credentials", "error", err)
				s.sessionMgr.Fail(ctx, sessionID, fmt.Errorf("resolve credentials: %w", err))
				return
			}
		}
	} else {
		// No forge configured — use plain git with the repo URL as-is.
		cloneURL = proj.RepoURL
	}

	// Clone repo first so we can read kvarn.yml before booting the VM.
	cloneDir, err := os.MkdirTemp("", "kvarn-clone-*")
	if err != nil {
		log.Error("failed to create temp dir", "error", err)
		s.sessionMgr.Fail(ctx, sessionID, fmt.Errorf("create temp dir: %w", err))
		return
	}
	defer os.RemoveAll(cloneDir)

	log.Info("cloning repository", "url", cloneURL, "branch", branch, "destination", cloneDir)
	s.sessionMgr.UpdateState(ctx, sessionID, session.StateCloning, "Cloning repository")

	// Pick the SCM to use for cloning.
	var scmImpl scm.SCM
	if forgeImpl != nil {
		scmImpl = forgeImpl.SCM()
	} else {
		scmImpl = &gitscm.Git{}
	}

	cloneOpts := scm.CloneOpts{
		URL:         cloneURL,
		Branch:      branch,
		Destination: cloneDir,
		Credentials: creds,
	}

	if err := scmImpl.Clone(ctx, cloneOpts); err != nil {
		log.Error("clone failed", "error", err)
		s.sessionMgr.Fail(ctx, sessionID, fmt.Errorf("clone repository: %w", err))
		return
	}
	log.Info("clone complete")

	// Load project config from cloned repo.
	cfg, err := projconfig.Load(cloneDir)
	if err != nil {
		log.Error("failed to load project config", "error", err)
		s.sessionMgr.Fail(ctx, sessionID, fmt.Errorf("load project config: %w", err))
		return
	}

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
	admitReq := scheduler.Request{
		CPUMillis: uint64(cpuCount) * 1000,
		MemBytes:  memBytes,
		DiskBytes: uint64(diskBytes),
		OnWait: func(e scheduler.WaitEvent) {
			s.sessionMgr.UpdateState(ctx, sessionID, session.StateQueued,
				fmt.Sprintf("Position %d in queue; need %d vCPU / %s memory / %s disk",
					e.Position, cpuCount, formatBytes(memBytes), formatBytes(uint64(diskBytes))))
		},
	}
	lease, err := s.scheduler.Acquire(ctx, admitReq)
	if err != nil {
		if errors.Is(err, scheduler.ErrTooLarge) {
			log.Error("job exceeds scheduler capacity", "error", err)
			s.sessionMgr.Fail(ctx, sessionID, fmt.Errorf("scheduler: job %d vCPU / %s memory / %s disk exceeds host capacity",
				cpuCount, formatBytes(memBytes), formatBytes(uint64(diskBytes))))
			return
		}
		log.Error("admission failed", "error", err)
		s.sessionMgr.Fail(ctx, sessionID, fmt.Errorf("admission: %w", err))
		return
	}
	defer lease.Release()

	// Load repo context (instructions + skills). Non-fatal on error.
	rc, err := repocontext.Load(cloneDir)
	if err != nil {
		log.Warn("failed to load repo context", "error", err)
		rc = &repocontext.RepoContext{}
	}

	// Resolve secrets declared in kvarn.yml. env-typed secrets become
	// real env vars in the VM; bearer-typed secrets are exposed as
	// per-job placeholders that the egress proxy substitutes for the
	// real value just before the request leaves the host.
	var secretEnv, bearerPlaceholders map[string]string
	if cfg != nil && len(cfg.Secrets) > 0 {
		secretEnv, bearerPlaceholders, err = secret.Resolve(ctx, s.secretStore, proj.Name, cfg.Secrets)
	}
	if err != nil {
		log.Error("failed to resolve secrets", "error", err)
		s.sessionMgr.Fail(ctx, sessionID, err)
		return
	}

	createOpts := s.createOpts
	if len(bearerPlaceholders) > 0 {
		createOpts.Network.SecretInjector = egressproxy.NewPlaceholderInjector(bearerPlaceholders, log)
	}

	// Boot VM, transfer files, configure firewall/tools/container.
	create := s.sandboxFactory
	if create == nil {
		create = defaultSandboxFactory
	}
	sess, err := create(ctx, sandbox.Opts{
		Provider:        s.provider,
		CreateOpts:      createOpts,
		Config:          cfg,
		Transferer:      s.transferer,
		SourceDir:       cloneDir,
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
		s.sessionMgr.Fail(ctx, sessionID, err)
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
			s.sessionMgr.Fail(ctx, sessionID, fmt.Errorf("setup: %w", err))
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
		Prompt:      prompt,
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

	var agentResult *agent.Result
	if s.agent != nil {
		agentResult, err = s.agent.Run(ctx, agentCtx)
		// Persist partial cost regardless of success: spend up to a failure
		// is still interesting to users.
		s.sessionMgr.UpdateCost(context.Background(), sessionID, tracker.Snapshot())
		if err != nil {
			log.Error("agent failed", "error", err)
			// Surface a clearer error message when the agent ctx was cancelled
			// because the cost cap was hit.
			cause := context.Cause(rootCtx)
			if errors.Is(cause, cost.ErrBudgetExceeded) {
				s.sessionMgr.Fail(context.Background(), sessionID, fmt.Errorf("agent: %w", cause))
			} else {
				s.sessionMgr.Fail(context.Background(), sessionID, fmt.Errorf("agent: %w", err))
			}
			return
		}
	}

	// Run validation steps.
	if !mode.WritesChanges() {
		log.Info("skipping validation: read-only mode")
	} else if cfg != nil && (len(cfg.Validation.Required) > 0 || len(cfg.Validation.Advisory) > 0) {
		log.Info("running validation steps")
		s.sessionMgr.UpdateState(ctx, sessionID, session.StateValidating, "Running validation")

		changedFiles, err := sess.ChangedFiles(ctx)
		if err != nil {
			log.Error("failed to get changed files", "error", err)
			s.sessionMgr.Fail(ctx, sessionID, fmt.Errorf("changed files: %w", err))
			return
		}

		onStepDone := s.makeStepCallback(ctx, sessionID)
		onOutput := s.makeOutputCallback(ctx, sessionID)
		valResult, err := sess.RunValidation(ctx, cfg, changedFiles, onStepDone, onOutput)
		if err != nil {
			log.Error("validation failed", "error", err)
			s.sessionMgr.Fail(ctx, sessionID, fmt.Errorf("validation: %w", err))
			return
		}

		if !valResult.RequiredPassed {
			log.Error("required validation steps failed")
			s.sessionMgr.Fail(ctx, sessionID, errors.New("required validation steps failed"))
			return
		}
		log.Info("validation complete")
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

	// Submit changes as PR if forge is available, agent produced a result,
	// and there are changes.
	if !mode.WritesChanges() {
		log.Info("skipping PR submission: read-only mode")
	} else if forgeImpl == nil {
		log.Info("skipping PR submission: no forge configured")
	} else if agentResult == nil {
		log.Info("skipping PR submission: no agent result")
	} else if creds == nil || creds.APIToken() == "" {
		log.Info("skipping PR submission: no token in credentials")
	} else {
		s.submitChanges(ctx, sessionID, sess, forgeImpl, agentResult, proj, forgeCfg, branch, cloneURL, cloneDir, creds, prompt, worklog.snapshot(), tracker.Snapshot(), costLimits.ReportCostOnPR, log)
	}

	// Final cost snapshot. The agent has already populated the session with
	// its partial snapshot above; this one captures any tail work, and the
	// CostEvent gives watchers a clear end-of-run summary.
	finalReport := tracker.Snapshot()
	s.sessionMgr.UpdateCost(ctx, sessionID, finalReport)
	s.sessionMgr.EmitEvent(ctx, sessionID, session.CostEvent{
		SessionID: sessionID,
		Kind:      session.CostUpdateFinal,
		Report:    finalReport,
		Limit:     cost.Limit{MaxUSD: costLimits.MaxCostUSD, WarnFraction: costLimits.WarnThreshold},
	})

	log.Info("job completed successfully")
	s.sessionMgr.UpdateState(ctx, sessionID, session.StateCompleted, "Completed")
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
	creds *scm.Credentials,
	prompt string,
	worklog []worklogEntry,
	costReport cost.Report,
	reportCostOnPR bool,
	log *slog.Logger,
) {
	// Check if there are any changes.
	changedFiles, err := sess.ChangedFiles(ctx)
	if err != nil {
		log.Warn("failed to check changed files for submission", "error", err)
		return
	}
	if len(changedFiles) == 0 {
		log.Info("no changes to submit")
		return
	}

	title := agentResult.Title
	if title == "" {
		title = "Apply agent changes"
	}
	body := agentResult.Description

	s.sessionMgr.UpdateState(ctx, sessionID, session.StateSubmitting, "Submitting changes")

	// Extract changes from VM to host clone dir.
	if err := sess.ExtractChanges(ctx, cloneDir); err != nil {
		log.Error("failed to extract changes from VM", "error", err)
		return
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
		Branch:      prBranch,
		Message:     commitMsg,
		AuthorName:  authorName,
		AuthorEmail: authorEmail,
		Credentials: creds,
	}); err != nil {
		log.Error("failed to commit and push", "error", err)
		return
	}

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
		log.Error("failed to create pull request", "error", err)
		return
	}

	log.Info("pull request created", "url", pr.URL, "number", pr.Number)
	s.sessionMgr.EmitEvent(ctx, sessionID, session.PullRequestEvent{
		SessionID: sessionID,
		URL:       pr.URL,
		Number:    pr.Number,
		Branch:    prBranch,
	})

	// Post task + work log as a PR comment so it stays out of any
	// squash-merge commit message.
	commentBody := formatWorklogComment(prompt, worklog, reportCostOnPR, costReport)
	if err := forgeImpl.PostComment(ctx, forge.PostCommentOpts{
		RepoURL:     cloneURL,
		Number:      pr.Number,
		Body:        commentBody,
		Credentials: creds,
	}); err != nil {
		log.Warn("failed to post task/work-log comment", "error", err)
	}
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

	if err := s.authorizeProject(ctx, sess.ProjectName); err != nil {
		return nil, err
	}

	return connect.NewResponse(&v1.GetSessionResponse{
		SessionId:      sess.ID,
		Project:        sess.ProjectName,
		State:          string(sess.State),
		Message:        sess.Message,
		Error:          sess.Error,
		Prompt:         sess.Prompt,
		PullRequestUrl: sess.PullRequestURL,
		Mode:           sess.Mode,
		Cost:           costReportToProto(sess.Cost),
	}), nil
}

func (s *Service) ListSessions(ctx context.Context, _ *connect.Request[v1.ListSessionsRequest]) (*connect.Response[v1.ListSessionsResponse], error) {
	if s.sessionMgr == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("sessions not configured"))
	}

	sessions, err := s.sessionMgr.List(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// When auth is enabled, restrict the listing to the projects the key
	// covers. A missing identity (unreachable behind the interceptor) yields
	// an empty list rather than an error.
	id, hasIdentity := auth.IdentityFrom(ctx)

	var resp []*v1.GetSessionResponse
	for _, sess := range sessions {
		if s.authEnabled && (!hasIdentity || !id.AllowsProject(sess.ProjectName)) {
			continue
		}
		resp = append(resp, &v1.GetSessionResponse{
			SessionId:      sess.ID,
			Project:        sess.ProjectName,
			State:          string(sess.State),
			Message:        sess.Message,
			Error:          sess.Error,
			Prompt:         sess.Prompt,
			PullRequestUrl: sess.PullRequestURL,
			Mode:           sess.Mode,
			Cost:           costReportToProto(sess.Cost),
		})
	}

	return connect.NewResponse(&v1.ListSessionsResponse{
		Sessions: resp,
	}), nil
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
	if err := s.authorizeProject(ctx, sess.ProjectName); err != nil {
		return err
	}

	ch, err := s.sessionMgr.Watch(ctx, req.Msg.SessionId)
	if err != nil {
		return connect.NewError(connect.CodeNotFound, err)
	}

	for event := range ch {
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
						Number: int32(e.Number),
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
			if err := stream.Send(update); err != nil {
				return err
			}
		}
	}

	return nil
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
