package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
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

// ToolProvisioningEvent reports that a registered tool is running the command
// that makes it usable — a version manager installing the toolchain it pins,
// for instance. It is a distinct phase from dependency installation: the
// dependency is already there, and this is the tool acting on the repository.
type ToolProvisioningEvent struct {
	Tool    string
	Command string
}

func (ToolProvisioningEvent) isEvent() {}

type ToolProvisionedEvent struct {
	Tool string
}

func (ToolProvisionedEvent) isEvent() {}

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

type ToolProvisionOutputEvent struct {
	Stdout string
	Stderr string
}

func (ToolProvisionOutputEvent) isEvent() {}

// EgressDeniedEvent reports a connection the allowlist refused. It is a
// warning, not a failure: plenty of jobs pass with a blocked telemetry ping.
// It matters when something else fails right afterwards, which is why the
// hosts are also recorded on the session and named in that failure.
type EgressDeniedEvent struct {
	Host string
}

func (EgressDeniedEvent) isEvent() {}

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

	// SkipFile filters the upload walk; see transfer.Options. Ignored when
	// PristineClone is set, which installs its own filter.
	SkipFile func(relPath string, isDir bool) bool

	// PristineClone declares that SourceDir is a clone whose worktree is
	// exactly HEAD, with nothing uncommitted. Only the repository is then
	// shipped and the worktree is checked out in the guest, which halves the
	// bytes crossing the transport. Set it only when that holds: for a dirty
	// or partially-ignored tree (what `kvarn run`/`kvarn test` send) the
	// guest checkout would discard the very files the run is about.
	PristineClone bool

	// Registry and BridgeHandler allow sharing dispatch infrastructure with
	// an existing service (e.g. the orchestrator). When nil, the sandbox
	// creates its own.
	Registry      *dispatch.Registry
	BridgeHandler *dispatch.Handler

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

	// BaseCommit is the commit the workspace sat at once it was prepared, and
	// the revision every later change detection compares against. Empty when
	// the workspace is not a git repository or has no commits.
	BaseCommit string

	// dialGuest is the provider's ingress path into the VM, carried here
	// because callers that want to reach a server inside the guest hold a
	// Session rather than the *vm.VM it was booted from. Nil when the provider
	// has no ingress path.
	dialGuest func(ctx context.Context, port uint16) (net.Conn, error)

	// bareProxy is the underlying BridgeProxy (not wrapped by container).
	// Needed for operations like git diff that must run on the host VM.
	bareProxy *BridgeProxy

	cacheProvider cache.Provider
	projectID     string
	cacheLayers   []cache.Layer
	onEvent       func(Event)

	deniedMu    sync.Mutex
	deniedHosts []string

	closers   []func()
	closersMu sync.Mutex
	closeOnce sync.Once
}

// maxRecordedDeniedHosts bounds the set kept for error messages. A job that
// retries a blocked download in a loop reports the same host every time; the
// list exists to name the problem, not to log it.
const maxRecordedDeniedHosts = 20

// recordEgressDenied notes a host the proxy refused. Called from proxy
// goroutines.
func (s *Session) recordEgressDenied(host string) {
	s.deniedMu.Lock()
	defer s.deniedMu.Unlock()
	for _, h := range s.deniedHosts {
		if h == host {
			return
		}
	}
	if len(s.deniedHosts) >= maxRecordedDeniedHosts {
		return
	}
	s.deniedHosts = append(s.deniedHosts, host)
}

// DeniedHosts returns the hosts the egress proxy has refused so far, in the
// order they were first seen.
func (s *Session) DeniedHosts() []string {
	s.deniedMu.Lock()
	defer s.deniedMu.Unlock()
	return append([]string(nil), s.deniedHosts...)
}

// annotateEgress adds the hosts the proxy refused to a failure. A refused
// connection reaches the program inside the VM as a socket that closed
// mid-handshake, so the error it reports — "unexpected EOF", "connection
// reset" — describes the symptom and cannot name the cause. The proxy knows
// the hostname; this is where the two halves meet.
func (s *Session) annotateEgress(err error) error {
	if err == nil {
		return nil
	}
	denied := s.DeniedHosts()
	if len(denied) == 0 {
		return err
	}
	return fmt.Errorf("%w (egress denied: %s — add the host to network.allowed_hosts in kvarn.yml if the job needs it)",
		err, strings.Join(denied, ", "))
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
	return ExtractChanges(ctx, s.bareProxy, s.WorkingDir, destDir, s.BaseCommit)
}

// DialGuest opens a TCP connection to a port inside the sandbox's VM. It
// returns errors.ErrUnsupported when the provider that booted this session has
// no ingress path, matching the convention the non-darwin provider stubs use.
func (s *Session) DialGuest(ctx context.Context, port uint16) (net.Conn, error) {
	if s.dialGuest == nil {
		return nil, fmt.Errorf("dial guest port %d: %w", port, errors.ErrUnsupported)
	}
	return s.dialGuest(ctx, port)
}

// CanDialGuest reports whether this session's provider supports ingress, so a
// caller can refuse work up front rather than discovering it on the first
// request.
func (s *Session) CanDialGuest() bool { return s.dialGuest != nil }

// Processes returns the session's manager for long-lived guest processes. It
// is the bare proxy deliberately: a server has to run on the VM itself, not
// inside whatever container a step wrapped itself in.
func (s *Session) Processes() ProcessRunner { return s.bareProxy }

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
	result, err := RunSetup(ctx, s.Runner, cfg, s.ShellSessionID, onDone, onOutput)
	return result, s.annotateEgress(err)
}

// RunValidation runs validation steps from the config.
// If changedFiles is nil, all steps run; otherwise path-filtered steps are
// skipped when no changed files match.
func (s *Session) RunValidation(ctx context.Context, cfg *project.Config, changedFiles []string, onDone OnStepDone, onOutput OnOutput) (*ValidationResult, error) {
	if cfg == nil {
		return &ValidationResult{RequiredPassed: true}, nil
	}
	result, err := RunValidation(ctx, s.Runner, cfg, s.ShellSessionID, changedFiles, onDone, onOutput)
	return result, s.annotateEgress(err)
}

// ChangedFiles returns the paths the workspace has changed since the session's
// base commit.
func (s *Session) ChangedFiles(ctx context.Context) ([]string, error) {
	return ChangedFiles(ctx, s.bareProxy, s.WorkingDir, s.BaseCommit)
}

// GetBaseCommit returns the commit the workspace started at, or "" when it
// could not be resolved.
func (s *Session) GetBaseCommit() string { return s.BaseCommit }

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
			runCloser(closers[i])
		}
	})
}

// runCloser invokes a closer and recovers from any panic so that one
// misbehaving closer cannot skip the remaining teardown (VM destroy,
// listener close, etc.).
func runCloser(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic in sandbox session closer", "panic", r)
		}
	}()
	fn()
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

	aug := computeAugmentations(deps)

	// Derive the content-addressed cache layers from the resolved deps and
	// user config.
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
	createOpts.Network.AllowedHosts = append(createOpts.Network.AllowedHosts, allowedHosts...)
	if opts.Config != nil {
		// Every alias goes to the VM's DNS forwarder, wildcard or not, so the
		// two resolution paths in the guest agree on the names they both
		// cover. Only the exact ones can also become /etc/hosts lines.
		createOpts.Network.HostAliases = opts.Config.Network.HostAliases
	}
	createOpts.Network.OnEgressDenied = func(host string) {
		sess.recordEgressDenied(host)
		emit(opts, EgressDeniedEvent{Host: host})
	}
	// CreateOpts.Network.SecretInjector is whatever the caller set; the
	// orchestrator builds a PlaceholderInjector for bearer-typed secrets
	// before invoking sandbox.Start, leaving it nil when no bearer secrets
	// are configured.

	instance, runnerConn, err := opts.Provider.Create(ctx, createOpts)
	if err != nil {
		return nil, fmt.Errorf("create VM: %w", err)
	}
	sess.dialGuest = instance.DialGuest
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

	// Establish trust in the egress proxy before anything below it opens a
	// TLS connection from inside the VM. Everything that follows — the file
	// transfer's git operations, the cache restore, `nix profile add`, and every
	// pull a step makes — reaches the network only through the MITM proxy, so
	// this has to be the first command the guest runs.
	if err := InstallProxyCA(ctx, proxy, instance.ProxyCAPEM); err != nil {
		return nil, fmt.Errorf("install proxy CA: %w", err)
	}

	// Map the project's development hostnames before anything in the guest can
	// resolve a name: a container a step starts seeds its own hosts file from
	// this one when it is created, and every step runs after this point.
	// Wildcards are not expressible here and are served by the DNS forwarder
	// instead.
	if opts.Config != nil {
		if err := ConfigureHostAliases(ctx, proxy, opts.Config.Network.ExactHostAliases()); err != nil {
			return nil, fmt.Errorf("configure host aliases: %w", err)
		}
	}

	// Transfer files.
	if opts.Transferer != nil && opts.SourceDir != "" {
		emit(opts, TransferringEvent{})
		uploadOpts := transfer.Options{
			SkipFile: opts.SkipFile,
			OnProgress: func(sent, total int64) {
				emit(opts, TransferProgressEvent{BytesSent: sent, TotalBytes: total})
			},
		}
		if opts.PristineClone {
			uploadOpts.SkipFile = transfer.GitDirOnlyFilter()
		}
		if err := opts.Transferer.Upload(ctx, proxy, opts.SourceDir, workingDir, uploadOpts); err != nil {
			return nil, fmt.Errorf("transfer files: %w", err)
		}

		// Materialize the worktree before anything else in this function
		// touches the workspace: the cache restore and dependency install below
		// both assume a populated directory.
		if opts.PristineClone {
			if err := CheckoutWorktree(ctx, proxy, workingDir); err != nil {
				return nil, fmt.Errorf("check out worktree in guest: %w", err)
			}
		}
	}

	// Pin the base commit while the workspace is still exactly what was
	// uploaded. Everything downstream measures change against this commit, so
	// it has to be read before any step or agent can move HEAD.
	sess.BaseCommit = ResolveBaseCommit(ctx, proxy, workingDir)

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
			return nil, sess.annotateEgress(fmt.Errorf("install dependencies: %w", err))
		}
		emit(opts, DependenciesInstalledEvent{})
	}

	// Write curated, user, and secret environment to /etc/profile.d so
	// privileged `su -l` execs pick them up.
	if opts.Config != nil {
		if err := writeProfileScripts(ctx, proxy, aug, opts.Config.Environment, opts.Secrets); err != nil {
			return nil, fmt.Errorf("write profile.d scripts: %w", err)
		}
	}

	sess.Runner = proxy

	// Create persistent shell session.
	emit(opts, SessionCreatingEvent{})
	sessionResp, err := proxy.CreateSession(ctx, &v1.CreateSessionRequest{
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
		proxy.CloseSession(closeCtx, &v1.CloseSessionRequest{SessionId: sess.ShellSessionID})
	})

	// Provision registered tools last: the command runs in the shell session so
	// it inherits the curated environment, and it needs the workspace it is
	// about to read, which the transfer and checkout above have put in place.
	for _, step := range aug.Provision {
		emit(opts, ToolProvisioningEvent{Tool: step.Tool, Command: step.Command})
		if err := provisionTool(ctx, proxy, sess.ShellSessionID, step, func(stdout, stderr string) {
			emit(opts, ToolProvisionOutputEvent{Stdout: stdout, Stderr: stderr})
		}); err != nil {
			return nil, sess.annotateEgress(err)
		}
		emit(opts, ToolProvisionedEvent{Tool: step.Tool})
	}

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
