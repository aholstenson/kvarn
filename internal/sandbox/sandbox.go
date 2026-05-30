package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/dispatch"
	"github.com/aholstenson/kvarn/internal/project"
	"github.com/aholstenson/kvarn/internal/sandbox/cache"
	"github.com/aholstenson/kvarn/internal/sandbox/transfer"
	"github.com/aholstenson/kvarn/internal/vm"
)

// Event is emitted during sandbox startup to report progress.
type Event interface {
	isEvent()
}

type ProvisioningEvent struct{}

func (ProvisioningEvent) isEvent() {}

type TransferringEvent struct{}

func (TransferringEvent) isEvent() {}

type DependenciesInstallingEvent struct{}

func (DependenciesInstallingEvent) isEvent() {}

type DependenciesInstalledEvent struct{}

func (DependenciesInstalledEvent) isEvent() {}

type ImagePullingEvent struct {
	Image string
}

func (ImagePullingEvent) isEvent() {}

type ContainerStartingEvent struct{}

func (ContainerStartingEvent) isEvent() {}

type SessionCreatingEvent struct{}

func (SessionCreatingEvent) isEvent() {}

type CacheRestoringEvent struct{}

func (CacheRestoringEvent) isEvent() {}

type CacheSavingEvent struct{}

func (CacheSavingEvent) isEvent() {}

type ProvisionedEvent struct {
	VmInfo *v1.VmInfo
}

func (ProvisionedEvent) isEvent() {}

type TransferProgressEvent struct {
	BytesSent  int64
	TotalBytes int64
}

func (TransferProgressEvent) isEvent() {}

type DependencyOutputEvent struct {
	Stdout string
	Stderr string
}

func (DependencyOutputEvent) isEvent() {}

type ContainerStartedEvent struct{}

func (ContainerStartedEvent) isEvent() {}

type SessionCreatedEvent struct{}

func (SessionCreatedEvent) isEvent() {}

type CacheProgressEvent struct {
	Path      string
	Index     int
	Total     int
	Restoring bool
}

func (CacheProgressEvent) isEvent() {}

type CacheRestoredEvent struct{}

func (CacheRestoredEvent) isEvent() {}

type CacheSavedEvent struct{}

func (CacheSavedEvent) isEvent() {}

type ConsoleOutputEvent struct {
	Output string
}

func (ConsoleOutputEvent) isEvent() {}

// Opts configures sandbox creation.
type Opts struct {
	Provider   vm.Provider
	CreateOpts vm.CreateOpts
	Config     *project.Config
	Transferer transfer.Transferer
	SourceDir  string // local directory to upload
	WorkingDir string // VM path; defaults to "/home/kvarn/workspace"

	// Registry and BridgeHandler allow sharing dispatch infrastructure with
	// an existing service (e.g. the orchestrator). When nil, the sandbox
	// creates its own.
	Registry      *dispatch.Registry
	BridgeHandler *dispatch.Handler

	RegistryMirrors []string

	CacheProvider cache.Provider
	ProjectID     string
	// Namespace partitions the cache pool; "" is the shared pool. A future
	// "pr-<n>" isolates untrusted fork PRs.
	Namespace string

	// Secrets are env-var-name → final-string pairs to expose inside the
	// VM. For env-typed secrets the value is the real secret; for bearer
	// secrets the orchestrator has already substituted the unguessable
	// placeholder so the VM never sees the real value.
	Secrets map[string]string

	OnEvent func(Event)
}

// Session represents a running sandbox with a booted VM, transferred files,
// configured firewall/tools/container, and a persistent shell session.
type Session struct {
	Runner         RunnerProxy
	ShellSessionID string
	WorkingDir     string
	VmInfo         *v1.VmInfo

	// bareProxy is the underlying BridgeProxy (not wrapped by container).
	// Needed for operations like git diff that must run on the host VM.
	bareProxy *BridgeProxy

	cacheProvider cache.Provider
	projectID     string
	cacheLayers   []cache.Layer
	onEvent       func(Event)

	closers   []func()
	closersMu sync.Mutex
	closeOnce sync.Once
}

// BareProxy returns the underlying BridgeProxy that talks directly to the VM,
// bypassing any container wrapper. Use this for host-level operations like
// git diff.
func (s *Session) BareProxy() *BridgeProxy {
	return s.bareProxy
}

// ExtractChanges copies changed files from the VM workspace back to a host
// directory. It identifies modified/added/deleted files via git commands,
// reads each changed file, writes them to destDir, and removes deleted files.
func (s *Session) ExtractChanges(ctx context.Context, destDir string) error {
	return ExtractChanges(ctx, s.bareProxy, s.WorkingDir, destDir)
}

// GetRunner returns the session's RunnerProxy.
func (s *Session) GetRunner() RunnerProxy { return s.Runner }

// GetShellSessionID returns the persistent shell session ID.
func (s *Session) GetShellSessionID() string { return s.ShellSessionID }

// GetWorkingDir returns the workspace directory path.
func (s *Session) GetWorkingDir() string { return s.WorkingDir }

// RunSetup executes setup steps and health checks from the config.
func (s *Session) RunSetup(ctx context.Context, cfg *project.Config, onDone OnStepDone, onOutput OnOutput) (*SetupResult, error) {
	if cfg == nil {
		return &SetupResult{}, nil
	}
	return RunSetup(ctx, s.Runner, cfg, s.ShellSessionID, onDone, onOutput)
}

// RunValidation runs validation steps from the config.
// If changedFiles is nil, all steps run; otherwise path-filtered steps are
// skipped when no changed files match.
func (s *Session) RunValidation(ctx context.Context, cfg *project.Config, changedFiles []string, onDone OnStepDone, onOutput OnOutput) (*ValidationResult, error) {
	if cfg == nil {
		return &ValidationResult{RequiredPassed: true}, nil
	}
	return RunValidation(ctx, s.Runner, cfg, s.ShellSessionID, changedFiles, onDone, onOutput)
}

// ChangedFiles runs `git diff --name-only HEAD` on the VM and returns the list of changed file paths.
func (s *Session) ChangedFiles(ctx context.Context) ([]string, error) {
	return ChangedFiles(ctx, s.bareProxy, s.WorkingDir)
}

// SaveCache creates tarballs from cached guest paths and stores them via the
// cache provider. Should be called explicitly by the caller after job
// completion (even on job failure), but not on infrastructure failures.
func (s *Session) SaveCache(ctx context.Context) error {
	if s.cacheProvider == nil || len(s.cacheLayers) == 0 {
		return nil
	}
	return SaveCache(ctx, s.bareProxy, s.cacheProvider, s.cacheLayers, s.onEvent)
}

// Close tears down all resources in reverse order. Idempotent.
func (s *Session) Close() {
	s.closeOnce.Do(func() {
		s.closersMu.Lock()
		closers := s.closers
		s.closers = nil
		s.closersMu.Unlock()

		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	})
}

func (s *Session) addCloser(fn func()) {
	s.closersMu.Lock()
	defer s.closersMu.Unlock()
	s.closers = append(s.closers, fn)
}

// Start boots a VM, transfers files, configures firewall/tools/container,
// runs setup steps, and returns a ready Session.
func Start(ctx context.Context, opts Opts) (_ *Session, retErr error) {
	workingDir := opts.WorkingDir
	if workingDir == "" {
		workingDir = "/home/kvarn/workspace"
	}

	sess := &Session{
		WorkingDir: workingDir,
	}

	// On error, clean up everything accumulated so far.
	defer func() {
		if retErr != nil {
			sess.Close()
		}
	}()

	// Set up dispatch registry for runner communication.
	token, err := generateToken()
	if err != nil {
		return nil, fmt.Errorf("generate token: %w", err)
	}

	registry := opts.Registry
	handler := opts.BridgeHandler
	if registry == nil {
		registry = dispatch.NewRegistry()
		handler = dispatch.NewHandler(registry)
	}

	pr, err := registry.Register(token)
	if err != nil {
		return nil, fmt.Errorf("register token: %w", err)
	}
	sess.addCloser(func() { registry.Remove(token) })

	// Resolve dependencies once and reuse for firewall hosts and installation.
	var deps []project.ResolvedDep
	if opts.Config != nil && len(opts.Config.Dependencies) > 0 {
		var err error
		deps, err = opts.Config.Dependencies.Resolve()
		if err != nil {
			return nil, fmt.Errorf("resolve dependencies: %w", err)
		}
	}

	// Curation runs against the VM filesystem and profile.d, so it is
	// skipped in image mode (container exec doesn't source the VM's
	// /etc/profile.d).
	var aug augmentations
	if opts.Config != nil && opts.Config.Image == "" {
		aug = computeAugmentations(deps)
	}

	// Derive the content-addressed cache layers from the resolved deps and
	// user config. Tool layers come from registered nixpkgs deps (empty in
	// image mode, where deps are disallowed), so image jobs get only the
	// user-configured layers.
	var cacheLayers []cache.Layer
	if opts.CacheProvider != nil && opts.ProjectID != "" && opts.Config != nil {
		cacheLayers, err = cache.DeriveLayers(
			opts.SourceDir, deps, cacheToolLookup,
			opts.Config.Cache, opts.ProjectID, opts.Namespace,
		)
		if err != nil {
			return nil, fmt.Errorf("derive cache layers: %w", err)
		}
	}

	// Boot VM.
	emit(opts, ProvisioningEvent{})

	createOpts := opts.CreateOpts
	createOpts.Token = token
	createOpts.OnConsoleOutput = func(output string) {
		emit(opts, ConsoleOutputEvent{Output: output})
	}
	if opts.Config != nil {
		createOpts.DiskSizeBytes = opts.Config.DiskSizeBytes()
		createOpts.CPUs = opts.Config.CPUs()
		createOpts.MemoryBytes = opts.Config.MemoryBytes()
	}

	// Build the egress proxy's allowlist before VM creation so the
	// netstack and proxy come up with the right hosts permitted.
	var allowedHosts []string
	if opts.Config != nil {
		allowedHosts = append(allowedHosts, opts.Config.Network.AllowedHosts...)
	}
	if len(deps) > 0 {
		// Substituters Nix talks to for any flake evaluation.
		allowedHosts = append(allowedHosts,
			"github.com", "codeload.github.com",
			"cache.nixos.org", "channels.nixos.org",
		)
		for _, d := range deps {
			if d.Host != "" {
				allowedHosts = append(allowedHosts, d.Host)
			}
		}
	}
	allowedHosts = append(allowedHosts, aug.Hosts...)
	for _, mirror := range opts.RegistryMirrors {
		// Extract the bare hostname from each mirror URL.
		if u, err := url.Parse(mirror); err == nil && u.Hostname() != "" {
			allowedHosts = append(allowedHosts, u.Hostname())
		} else if mirror != "" {
			allowedHosts = append(allowedHosts, mirror)
		}
	}
	createOpts.Network.AllowedHosts = append(createOpts.Network.AllowedHosts, allowedHosts...)
	// CreateOpts.Network.SecretInjector is whatever the caller set; the
	// orchestrator builds a PlaceholderInjector for bearer-typed secrets
	// before invoking sandbox.Start, leaving it nil when no bearer secrets
	// are configured.

	instance, runnerConn, err := opts.Provider.Create(ctx, createOpts)
	if err != nil {
		return nil, fmt.Errorf("create VM: %w", err)
	}
	sess.addCloser(func() {
		slog.Info("destroying VM", "vm_id", instance.ID)
		destroyCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if destroyErr := opts.Provider.Destroy(destroyCtx, instance.ID); destroyErr != nil {
			slog.Error("failed to destroy VM", "vm_id", instance.ID, "error", destroyErr)
		}
	})

	// Bridge is reachable only over the provider-supplied transport so that
	// nothing outside the guest can talk to it. Each provider must hand us a
	// listener (vsock today; future remote providers must design an
	// authenticated transport rather than fall back to an open TCP socket).
	if runnerConn == nil || runnerConn.Listener == nil {
		return nil, errors.New("provider did not supply a runner listener; bridge cannot be served safely")
	}
	bridgeListener := dispatch.WrapListener(runnerConn.Listener, runnerConn.ExpectedPeerCID)
	go dispatch.Serve(bridgeListener, handler)
	sess.addCloser(func() { bridgeListener.Close() })

	// Wait for runner to register.
	registerCtx, registerCancel := context.WithTimeout(ctx, 30*time.Second)
	defer registerCancel()

	select {
	case <-pr.DoneCh:
	case <-registerCtx.Done():
		return nil, errors.New("timed out waiting for runner to connect")
	}

	sess.VmInfo = pr.VmInfo
	emit(opts, ProvisionedEvent{VmInfo: pr.VmInfo})

	// Build proxy for sending commands to the runner.
	proxy := NewBridgeProxy(pr.CommandCh, pr.ResultCh, pr.OutputCh, pr)
	sess.bareProxy = proxy

	// Transfer files.
	if opts.Transferer != nil && opts.SourceDir != "" {
		emit(opts, TransferringEvent{})
		switch t := opts.Transferer.(type) {
		case *transfer.BatchTransferer:
			t.OnProgress = func(sent, total int64) {
				emit(opts, TransferProgressEvent{BytesSent: sent, TotalBytes: total})
			}
		case *transfer.StreamingTransferer:
			t.OnProgress = func(sent, total int64) {
				emit(opts, TransferProgressEvent{BytesSent: sent, TotalBytes: total})
			}
		}
		if err := opts.Transferer.Upload(ctx, proxy, opts.SourceDir, workingDir); err != nil {
			return nil, fmt.Errorf("transfer files: %w", err)
		}
	}

	// Restore cache before installing dependencies so that Nix's eval and
	// fetcher state in ~/.cache/nix is in place before `nix profile add`
	// runs. Restoring after would overlay a stale tarball onto open sqlite
	// state and corrupt the install.
	if len(cacheLayers) > 0 && opts.CacheProvider != nil {
		emit(opts, CacheRestoringEvent{})
		emitFn := func(e Event) { emit(opts, e) }
		if err := RestoreCache(ctx, proxy, opts.CacheProvider, cacheLayers, emitFn); err != nil {
			return nil, fmt.Errorf("restore cache: %w", err)
		}
		sess.cacheProvider = opts.CacheProvider
		sess.projectID = opts.ProjectID
		sess.cacheLayers = cacheLayers
		sess.onEvent = opts.OnEvent
	}

	// Install dependencies if configured.
	if len(deps) > 0 {
		emit(opts, DependenciesInstallingEvent{})
		if err := InstallDependencies(ctx, proxy, deps, func(stdout, stderr string) {
			emit(opts, DependencyOutputEvent{Stdout: stdout, Stderr: stderr})
		}); err != nil {
			return nil, fmt.Errorf("install dependencies: %w", err)
		}
		emit(opts, DependenciesInstalledEvent{})
	}

	// Write curated, user, and secret environment to /etc/profile.d so
	// privileged `su -l` execs pick them up. Skipped in image mode
	// (container exec doesn't source the VM's profile.d).
	if opts.Config != nil && opts.Config.Image == "" {
		if err := writeProfileScripts(ctx, proxy, aug, opts.Config.Environment, opts.Secrets); err != nil {
			return nil, fmt.Errorf("write profile.d scripts: %w", err)
		}
	}

	// Configure registry mirrors and pull image if configured.
	cfg := opts.Config
	if cfg != nil && strings.TrimSpace(cfg.Image) != "" {
		if len(opts.RegistryMirrors) > 0 {
			if err := configureRegistryMirrors(ctx, proxy, opts.RegistryMirrors); err != nil {
				return nil, fmt.Errorf("configure registry mirrors: %w", err)
			}
		}

		emit(opts, ImagePullingEvent{Image: cfg.Image})
		if err := PullImage(ctx, proxy, cfg.Image); err != nil {
			return nil, fmt.Errorf("pull image: %w", err)
		}
	}

	// Start container if image is configured.
	var runner RunnerProxy = proxy
	if cfg != nil && strings.TrimSpace(cfg.Image) != "" {
		emit(opts, ContainerStartingEvent{})
		containerProxy := NewContainerProxy(proxy, "kvarn-workspace")
		if err := containerProxy.Start(ctx, cfg.Image, workingDir); err != nil {
			return nil, fmt.Errorf("start container: %w", err)
		}
		emit(opts, ContainerStartedEvent{})
		sess.addCloser(func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			containerProxy.Stop(stopCtx)
		})
		runner = containerProxy
	}
	sess.Runner = runner

	// Create persistent shell session.
	emit(opts, SessionCreatingEvent{})
	sessionResp, err := runner.CreateSession(ctx, &v1.CreateSessionRequest{
		WorkingDir: workingDir,
	})
	if err != nil {
		return nil, fmt.Errorf("create shell session: %w", err)
	}
	sess.ShellSessionID = sessionResp.SessionId
	emit(opts, SessionCreatedEvent{})
	sess.addCloser(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		runner.CloseSession(closeCtx, &v1.CloseSessionRequest{SessionId: sess.ShellSessionID})
	})

	return sess, nil
}

func emit(opts Opts, e Event) {
	if opts.OnEvent != nil {
		opts.OnEvent(e)
	}
}

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func appendUnique(base []string, extra []string) []string {
	seen := make(map[string]bool, len(base))
	for _, s := range base {
		seen[s] = true
	}
	for _, s := range extra {
		if !seen[s] {
			seen[s] = true
			base = append(base, s)
		}
	}
	return base
}

// configureRegistryMirrors uploads a Podman registries.conf with mirrors.
func configureRegistryMirrors(ctx context.Context, proxy RunnerProxy, mirrors []string) error {
	var toml strings.Builder
	for _, mirror := range mirrors {
		fmt.Fprintf(&toml, "[[registry]]\nlocation = \"docker.io\"\n\n[[registry.mirror]]\nlocation = \"%s\"\n\n", mirror)
	}

	_, err := proxy.UploadFiles(ctx, &v1.UploadFilesRequest{
		WorkingDir: "/etc/containers",
		Files: []*v1.FileContent{
			{Path: "registries.conf", Content: []byte(toml.String()), Mode: 0o644},
		},
	})
	if err != nil {
		return fmt.Errorf("upload registries.conf: %w", err)
	}

	return nil
}
