package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	projcfg "github.com/aholstenson/kvarn/internal/config/project"
	"github.com/aholstenson/kvarn/internal/config/secret"
	egressproxy "github.com/aholstenson/kvarn/internal/egress/proxy"
	"github.com/aholstenson/kvarn/internal/logging"
	"github.com/aholstenson/kvarn/internal/orchestrator/scheduler"
	"github.com/aholstenson/kvarn/internal/preview"
	projconfig "github.com/aholstenson/kvarn/internal/project"
	"github.com/aholstenson/kvarn/internal/sandbox"
	"github.com/aholstenson/kvarn/internal/sandbox/cache"
	"github.com/aholstenson/kvarn/internal/scm"
	gitscm "github.com/aholstenson/kvarn/internal/scm/git"
	"github.com/aholstenson/kvarn/internal/session"
	"github.com/aholstenson/kvarn/internal/vm"
)

// Booting a preview is the job path with the agent taken out and servers put
// in. It clones through the same mirror, reads the same kvarn.yml, resolves the
// same secrets and boots the same sandbox — a preview that behaved differently
// from a job on the same commit would be worth very little.
//
// What it does not reuse is the session: a preview outlives any one boot, so
// the durable preview record is the thing that persists, and a session is
// borrowed per boot purely so the cloning, dependency-install and setup phases
// report through the state machine and event stream that already exist. When
// the boot ends the session ends with it, successfully or not.

// previewSessionMarker is set in a borrowed session's metadata so the backlog
// dispatcher leaves it alone. Without it, a session created for a preview boot
// would be picked up on the next dispatch tick and run as if it were a job.
const previewSessionMarker = "kvarn.preview"

// isSessionMarkedPreview reports whether a backlog entry belongs to a preview
// boot rather than to a job.
func isSessionMarkedPreview(sess *session.Session) bool {
	if sess == nil {
		return false
	}
	_, ok := sess.Metadata[previewSessionMarker]
	return ok
}

// bootPreview provisions one preview environment. It satisfies previewBooter.
func (s *Service) bootPreview(ctx context.Context, p *preview.Preview, logs *preview.LogBuffer) (_ *previewBoot, retErr error) {
	if s.projectStore == nil || s.sessionMgr == nil {
		return nil, errors.New("preview boots need a project store and a session store")
	}

	proj, err := s.projectStore.Get(ctx, p.Project)
	if err != nil {
		return nil, fmt.Errorf("load project %q: %w", p.Project, err)
	}

	domain, err := s.previewDomain(proj)
	if err != nil {
		return nil, err
	}

	sess, err := s.sessionMgr.Create(ctx, session.CreateParams{
		ProjectName: proj.Name,
		Prompt:      fmt.Sprintf("Preview environment for %s", p.Ref),
		Mode:        "auto",
		BaseBranch:  p.Ref,
		Metadata:    map[string]string{previewSessionMarker: p.ID},
	})
	if err != nil {
		return nil, fmt.Errorf("create preview boot session: %w", err)
	}
	sessionID := sess.ID
	log := slog.With("component", "preview", "preview", p.ID, "project", proj.Name, "ref", p.Ref, "session_id", sessionID)

	// Take the session straight out of the backlog. The metadata marker keeps
	// the dispatcher from claiming it, and this makes the boot's own progress
	// the only thing the session reports.
	if _, err := s.sessionMgr.TransitionPending(ctx, sessionID, session.PendingTransition{
		State:   session.StateCloning,
		Message: "Cloning repository",
	}); err != nil {
		return nil, fmt.Errorf("claim preview boot session: %w", err)
	}

	// Every path out of here that has not produced a running preview has to
	// close the session, or a failed boot leaves a session running forever.
	succeeded := false
	defer func() {
		if succeeded {
			return
		}
		termCtx := context.WithoutCancel(ctx)
		cause := retErr
		if cause == nil {
			cause = errors.New("preview boot did not complete")
		}
		if err := s.sessionMgr.Fail(termCtx, sessionID, cause); err != nil {
			log.Warn("could not record preview boot failure on its session", "error", err)
		}
	}()

	// Record the session on the preview before anything slow happens: the
	// holding page reads the boot's progress from it, and a minute-long clone
	// with nothing to show is exactly when somebody is looking.
	p.SessionID = sessionID
	p.UpdatedAt = time.Now().UTC()
	if err := s.putPreview(ctx, p); err != nil {
		log.Warn("could not record the preview's boot session", "error", err)
	}

	fr, err := s.resolveForge(ctx, proj)
	if err != nil {
		return nil, fmt.Errorf("resolve forge: %w", err)
	}

	cloneDir, err := os.MkdirTemp("", "kvarn-preview-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	// The clone is only needed to build the VM's contents; the guest holds its
	// own copy for the preview's whole life.
	defer os.RemoveAll(cloneDir)

	releaseStaging, err := s.enterStaging(ctx, sessionID, log)
	if err != nil {
		return nil, err
	}
	defer releaseStaging()

	cloneStart := time.Now()
	log.Info("cloning for preview", "url", gitscm.RedactURL(fr.cloneURL), "ref", p.Ref)
	cloneDepth := resolveCloneDepth(proj)
	if !s.cloneViaMirror(ctx, proj, fr.cloneURL, p.Ref, cloneDir, cloneDepth, fr.creds, "", log) {
		var scmImpl scm.SCM = &gitscm.Git{}
		if fr.impl != nil {
			scmImpl = fr.impl.SCM()
		}
		if err := scmImpl.Clone(ctx, scm.CloneOpts{
			URL:         fr.cloneURL,
			Branch:      p.Ref,
			Destination: cloneDir,
			Credentials: fr.creds,
			Depth:       cloneDepth,
		}); err != nil {
			return nil, fmt.Errorf("clone %s: %w", p.Ref, err)
		}
	}
	log.Info("clone complete", "duration", logging.Elapsed(cloneStart))

	cfg, err := projconfig.Load(cloneDir)
	if err != nil {
		return nil, fmt.Errorf("load kvarn.yml: %w", err)
	}
	if cfg == nil || !cfg.Preview.Enabled() {
		return nil, fmt.Errorf("%s declares no preview: add a `preview:` block with at least one site to kvarn.yml", p.Ref)
	}

	sites, err := cfg.Preview.Resolve(p.Ref, domain)
	if err != nil {
		return nil, err
	}
	resolved := make([]preview.Site, 0, len(sites))
	for _, s := range sites {
		resolved = append(resolved, preview.Site{Name: s.Name, Host: s.Host, Port: s.Port})
	}

	// Claim the hostnames before booting anything. A name another preview holds
	// is a configuration error, and finding it out after a VM is up wastes a
	// boot and leaves the operator with two previews that disagree.
	p.Sites = resolved
	p.UpdatedAt = time.Now().UTC()
	if err := s.putPreview(ctx, p); err != nil {
		return nil, fmt.Errorf("claim preview hostnames: %w", err)
	}

	releaseStaging()

	lease, err := s.acquirePreviewCapacity(ctx, proj.Name, cfg)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			lease.Release()
		}
	}()

	secretEnv, managed, err := s.resolvePreviewSecrets(ctx, proj.Name, cfg)
	if err != nil {
		return nil, err
	}

	createOpts := s.createOpts
	if len(managed) > 0 {
		createOpts.Network.SecretInjector = egressproxy.NewPlaceholderInjector(managedSecrets(managed), log)
	}
	s.applyPreviewLimits(&createOpts, cfg)

	// The site URLs have to be in the environment before the serve commands run:
	// a server that cannot emit correct absolute URLs for its own assets is the
	// most common way a preview ends up half-broken.
	for _, site := range sites {
		if secretEnv == nil {
			secretEnv = map[string]string{}
		}
		secretEnv[projconfig.EnvVarName(site.Name)] = site.URL()
	}

	create := s.previewSandboxFactory
	if create == nil {
		create = defaultPreviewSandboxFactory
	}
	sandboxSession, err := create(ctx, sandbox.Opts{
		Provider:      s.provider,
		CreateOpts:    createOpts,
		Config:        cfg,
		Transferer:    s.transferer,
		SourceDir:     cloneDir,
		PristineClone: true,
		WorkingDir:    s.workspaceDir,
		Registry:      s.registry,
		BridgeHandler: s.bridgeHandler,
		CacheProvider: s.cacheProvider,
		ProjectID:     cache.ProjectID(proj.RepoURL),
		Namespace:     s.cacheNamespace,
		Secrets:       secretEnv,
		OnEvent:       s.makeEventAdapter(ctx, sessionID),
	})
	if err != nil {
		return nil, fmt.Errorf("boot preview VM: %w", err)
	}
	defer func() {
		if retErr != nil {
			sandboxSession.Close()
		}
	}()

	if !sandboxSession.CanDialGuest() {
		return nil, fmt.Errorf("the %s VM provider cannot serve preview traffic: %w",
			s.provider.Name(), errors.ErrUnsupported)
	}

	if len(cfg.Setup.Steps) > 0 || len(cfg.Setup.HealthChecks) > 0 {
		s.sessionMgr.UpdateState(ctx, sessionID, session.StateSetup, "Running setup")
		if _, err := sandboxSession.RunSetup(ctx, cfg,
			s.makeStepCallback(ctx, sessionID), s.makeOutputCallback(ctx, sessionID)); err != nil {
			return nil, fmt.Errorf("setup: %w", err)
		}
	}

	if err := s.runPreviewSetup(ctx, sandboxSession, cfg, sites, sessionID, logs); err != nil {
		return nil, err
	}

	if len(cfg.Preview.Serve) > 0 {
		s.sessionMgr.UpdateState(ctx, sessionID, session.StateRunning, "Starting services")
		if err := s.startPreviewServices(ctx, sandboxSession, cfg, sites, p.ID, logs); err != nil {
			return nil, err
		}
	}

	if err := s.waitPreviewReady(ctx, sandboxSession, cfg, sessionID); err != nil {
		return nil, err
	}

	// The boot is done and the preview takes over. Completing the session
	// rather than leaving it running is what says "there is nothing more to
	// watch here"; the preview's own state is where its life is reported from.
	if err := s.sessionMgr.UpdateState(ctx, sessionID, session.StateCompleted,
		fmt.Sprintf("Preview environment ready at %s", strings.Join(hostsOf(resolved), ", "))); err != nil {
		log.Warn("could not close the preview's boot session", "error", err)
	}

	succeeded = true
	return &previewBoot{
		Sandbox:   sandboxSession,
		Sites:     resolved,
		SessionID: sessionID,
		Lease:     lease,
	}, nil
}

// hostsOf reduces resolved sites to their hostnames.
func hostsOf(sites []preview.Site) []string {
	out := make([]string, 0, len(sites))
	for _, s := range sites {
		out = append(out, s.Host)
	}
	return out
}

// putPreview persists a preview record. A thin helper so the boot's several
// intermediate saves read as one line each.
func (s *Service) putPreview(ctx context.Context, p *preview.Preview) error {
	return s.previews.store.Put(ctx, p)
}

// previewDomain resolves which base domain this project's preview hostnames are
// formed under: the project's override, else the operator's.
func (s *Service) previewDomain(proj *projcfg.Project) (string, error) {
	if proj.Preview.Enabled != nil && !*proj.Preview.Enabled {
		return "", fmt.Errorf("previews are disabled for project %q", proj.Name)
	}
	if proj.Preview.Domain != "" {
		return proj.Preview.Domain, nil
	}
	if s.previews == nil || s.previews.policy.Domain == "" {
		return "", ErrPreviewsDisabled
	}
	return s.previews.policy.Domain, nil
}

// acquirePreviewCapacity reserves the VM's footprint without queueing. The
// ErrWouldBlock it returns is what the manager turns into an eviction attempt.
func (s *Service) acquirePreviewCapacity(ctx context.Context, projectName string, cfg *projconfig.Config) (scheduler.Lease, error) {
	cpuCount := cfg.CPUs()
	if cpuCount == 0 {
		cpuCount = projconfig.DefaultCPUs
	}
	memBytes := cfg.MemoryBytes()
	if memBytes == 0 {
		memBytes = projconfig.DefaultMemory
	}
	diskBytes := cfg.DiskSizeBytes()
	if diskBytes == 0 {
		diskBytes = projconfig.DefaultDiskSize
	}
	if s.previews != nil {
		if capMem := s.previews.policy.MaxMemoryBytes; capMem > 0 && memBytes > capMem {
			memBytes = capMem
		}
		if capDisk := s.previews.policy.MaxDiskBytes; capDisk > 0 && diskBytes > capDisk {
			diskBytes = capDisk
		}
	}

	lease, err := s.scheduler.TryAcquire(scheduler.Request{
		CPUMillis: uint64(cpuCount) * 1000,
		MemBytes:  memBytes,
		DiskBytes: uint64(diskBytes),
		Tenant:    scheduler.Tenant{Project: projectName},
	})
	if err != nil {
		if errors.Is(err, scheduler.ErrTooLarge) {
			return nil, fmt.Errorf("preview needs %d vCPU / %s memory / %s disk, which exceeds host capacity",
				cpuCount, formatBytes(memBytes), formatBytes(uint64(diskBytes)))
		}
		return nil, err
	}
	return lease, nil
}

// applyPreviewLimits clamps the VM request to the operator's preview ceilings.
// A preview runs for hours rather than minutes, so the memory and disk that are
// reasonable for a job are not automatically reasonable here.
func (s *Service) applyPreviewLimits(opts *vm.CreateOpts, cfg *projconfig.Config) {
	if s.previews == nil {
		return
	}
	if capMem := s.previews.policy.MaxMemoryBytes; capMem > 0 {
		want := cfg.MemoryBytes()
		if want == 0 || want > capMem {
			opts.MemoryBytes = capMem
		}
	}
	if capDisk := s.previews.policy.MaxDiskBytes; capDisk > 0 {
		want := cfg.DiskSizeBytes()
		if want == 0 || want > capDisk {
			opts.DiskSizeBytes = capDisk
		}
	}
}

// resolvePreviewSecrets resolves the project's declared secrets exactly as a
// job's are.
//
// This is deliberate and it is also the sharpest edge previews have: the code
// running in a preview came off a branch, and it can drive the egress proxy
// into attaching real managed credentials to its outbound requests. Nothing
// here mitigates that — what mitigates it is restricting who can reach a
// preview at all, which is why the how-to leads with access control.
func (s *Service) resolvePreviewSecrets(ctx context.Context, projectName string, cfg *projconfig.Config) (map[string]string, map[string]secret.Managed, error) {
	if len(cfg.Secrets) == 0 {
		return nil, nil, nil
	}
	env, managed, err := secret.Resolve(ctx, s.secretStore, projectName, secretRefs(cfg.Secrets))
	if err != nil {
		return nil, nil, fmt.Errorf("resolve secrets: %w", err)
	}
	return env, managed, nil
}

// runPreviewSetup publishes the site URLs into the boot's shell and runs the
// preview's one-shot setup steps there, reporting each onto the boot's session
// so a slow domain-configuration step is visible on the holding page.
func (s *Service) runPreviewSetup(
	ctx context.Context,
	sess previewBootSandbox,
	cfg *projconfig.Config,
	sites []projconfig.ResolvedSite,
	sessionID string,
	logs *preview.LogBuffer,
) error {
	if err := preview.ExportURLs(ctx, sess.GetRunner(), sess.GetShellSessionID(), previewURLs(sites)); err != nil {
		return err
	}
	if len(cfg.Preview.Setup) == 0 {
		return nil
	}

	s.sessionMgr.UpdateState(ctx, sessionID, session.StateSetup, "Running preview setup")
	return preview.RunSetup(ctx, sess.GetRunner(), sess.GetShellSessionID(), cfg.Preview.Setup,
		func(name string) {
			logs.Append(fmt.Sprintf("==> %s\n", name))
		},
		func(_ string, stdout, stderr string) {
			logs.Append(stdout)
			logs.Append(stderr)
		},
		func(name string) {
			s.sessionMgr.UpdateState(ctx, sessionID, session.StateSetup,
				fmt.Sprintf("Preview setup step %q completed", name))
		},
	)
}

// previewURLs maps site name to the address that site answers on.
func previewURLs(sites []projconfig.ResolvedSite) map[string]string {
	urls := make(map[string]string, len(sites))
	for _, site := range sites {
		urls[site.Name] = site.URL()
	}
	return urls
}

// startPreviewServices starts each declared serve command as a long-lived
// process in the guest, wiring its output into the preview's ring buffer.
func (s *Service) startPreviewServices(
	ctx context.Context,
	sess previewBootSandbox,
	cfg *projconfig.Config,
	sites []projconfig.ResolvedSite,
	previewID string,
	logs *preview.LogBuffer,
) error {
	urls := previewURLs(sites)

	return preview.StartServices(ctx, sess.Processes(), cfg, preview.ServeOpts{
		WorkspaceDir: s.workspaceDir,
		URLs:         urls,
		IDPrefix:     previewID,
		OnStarting: func(name string) {
			logs.Append(fmt.Sprintf("==> starting %s\n", name))
		},
		OnOutput: func(_ string, stdout, stderr string) {
			logs.Append(stdout)
			logs.Append(stderr)
		},
		OnExit: func(name string, exitCode int32, exitErr error) {
			// An exit goes to the ring buffer and the host log rather than to
			// the session: by the time a server dies, hours in, the boot's
			// session has long since completed and nobody is watching it.
			// `kvarn preview logs` is where this is read.
			msg := fmt.Sprintf("==> %s exited with status %d\n", name, exitCode)
			if exitErr != nil {
				msg = fmt.Sprintf("==> %s exited: %v\n", name, exitErr)
			}
			logs.Append(msg)
			slog.Warn("preview service exited",
				"preview", previewID, "service", name, "exit_code", exitCode, "error", exitErr)
		},
	})
}

// waitPreviewReady runs the ready checks until they pass, reporting each one
// that goes green onto the boot's session.
func (s *Service) waitPreviewReady(ctx context.Context, sess previewBootSandbox, cfg *projconfig.Config, sessionID string) error {
	return preview.WaitReady(ctx, sess.GetRunner(), sess.GetShellSessionID(), cfg.Preview.Ready, func(name string) {
		s.sessionMgr.UpdateState(ctx, sessionID, session.StateRunning,
			fmt.Sprintf("Ready check %q passed", name))
	})
}
