package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aholstenson/kvarn/internal/agent/coding"
	"github.com/aholstenson/kvarn/internal/buildinfo"
	apikeytoml "github.com/aholstenson/kvarn/internal/config/apikey/tomlstore"
	credtoml "github.com/aholstenson/kvarn/internal/config/credential/tomlstore"
	forgetoml "github.com/aholstenson/kvarn/internal/config/forge/tomlstore"
	modeltoml "github.com/aholstenson/kvarn/internal/config/model/tomlstore"
	orchcfg "github.com/aholstenson/kvarn/internal/config/orchestrator"
	projtoml "github.com/aholstenson/kvarn/internal/config/project/tomlstore"
	secrettoml "github.com/aholstenson/kvarn/internal/config/secret/tomlstore"
	"github.com/aholstenson/kvarn/internal/forge"
	forgegit "github.com/aholstenson/kvarn/internal/forge/git"
	forgegithub "github.com/aholstenson/kvarn/internal/forge/github"
	imageproxy "github.com/aholstenson/kvarn/internal/imagecache/proxy"
	imagestore "github.com/aholstenson/kvarn/internal/imagecache/store"
	"github.com/aholstenson/kvarn/internal/llmauth"
	"github.com/aholstenson/kvarn/internal/localsock"
	"github.com/aholstenson/kvarn/internal/observability/metrics"
	"github.com/aholstenson/kvarn/internal/orchestrator/scheduler"
	"github.com/aholstenson/kvarn/internal/preview"
	"github.com/aholstenson/kvarn/internal/preview/snapshot"
	previewsqlite "github.com/aholstenson/kvarn/internal/preview/sqlite"
	projconfig "github.com/aholstenson/kvarn/internal/project"
	"github.com/aholstenson/kvarn/internal/sandbox/cache"
	"github.com/aholstenson/kvarn/internal/sandbox/transfer"
	gitscm "github.com/aholstenson/kvarn/internal/scm/git"
	"github.com/aholstenson/kvarn/internal/scm/mirror"
	"github.com/aholstenson/kvarn/internal/session"
	sessionsqlite "github.com/aholstenson/kvarn/internal/session/sqlite"
	"github.com/aholstenson/kvarn/internal/vm"
	"github.com/aholstenson/kvarn/internal/vm/local"
	llms "github.com/aholstenson/llms-go"
)

type Cmd struct {
	Addr             string `help:"Address to listen on." default:":8080"`
	DiskImagePath    string `help:"Path to VM disk image. Auto-detected if not set."`
	ProjectsFile     string `help:"Path to projects TOML file." default:""`
	CredentialsFile  string `help:"Path to credentials TOML file." default:""`
	SecretsFile      string `help:"Path to per-project secrets TOML file." default:""`
	ForgesFile       string `help:"Path to forges TOML file." default:""`
	AgentsFile       string `help:"Path to agents config TOML file." default:""`
	APIKeysFile      string `help:"Path to API keys TOML file." default:""`
	SessionsDB       string `help:"Path to the persistent sessions SQLite database. Defaults to ~/.config/kvarn/sessions.db." default:""`
	PreviewsDB       string `help:"Path to the persistent previews SQLite database. Defaults to ~/.config/kvarn/previews.db." default:""`
	NoAuth           bool   `help:"Disable API-key auth (local dev only)." env:"KVARN_NO_AUTH"`
	LocalSocket      string `help:"Path to the host-local control socket. Empty = ~/.config/kvarn/orchestrator.sock." env:"KVARN_LOCAL_SOCKET" default:""`
	NoLocalSocket    bool   `help:"Do not serve the host-local control socket." env:"KVARN_NO_LOCAL_SOCKET"`
	Model            string `help:"LLM model alias for the coding agent." default:"coding-agent"`
	OrchestratorFile string `help:"Path to orchestrator TOML file (host-level settings, e.g. scheduler pool)." default:""`

	SchedCPUs           uint    `name:"sched-cpus" help:"Total vCPUs in the admission pool. 0 = file / runtime.NumCPU()." env:"KVARN_SCHED_CPUS" default:"0"`
	SchedMemory         string  `help:"Total admission-pool memory (e.g. 32G). Empty = file / 75% of host total." env:"KVARN_SCHED_MEMORY" default:""`
	SchedDisk           string  `help:"Total admission-pool disk (e.g. 200G). Empty = file / 75% of free space on the image cache filesystem." env:"KVARN_SCHED_DISK" default:""`
	SchedCPUOvercommit  float64 `help:"CPU overcommit multiplier (>=1.0). 0 = file / built-in default." env:"KVARN_SCHED_CPU_OVERCOMMIT" default:"0"`
	SchedDiskOvercommit float64 `help:"Disk overcommit multiplier (>=1.0); VM disks are thin. 0 = file / built-in default." env:"KVARN_SCHED_DISK_OVERCOMMIT" default:"0"`
	SchedDiskFloor      string  `help:"Real free space the VM disk filesystem must keep (e.g. 20G); admission pauses below it. 0 disables. Empty = file / 10% of the pool." env:"KVARN_SCHED_DISK_FLOOR" default:""`
	SchedBackfillGrace  string  `help:"How long a queued job may be skipped by ones that fit before it holds the line (e.g. 1m). 0 = strict FIFO. Empty = file / built-in default." env:"KVARN_SCHED_BACKFILL_GRACE" default:""`
	SchedPriorityAge    string  `help:"How long a queued job waits to gain one level of priority (e.g. 5m). 0 disables aging. Empty = file / built-in default." env:"KVARN_SCHED_PRIORITY_AGE_STEP" default:""`
	SchedMaxQueue       int     `help:"Max jobs in the in-memory pipeline (cloning, queued, running); the rest wait in the backlog. -1 = unbounded. 0 = file / built-in default." env:"KVARN_SCHED_MAX_QUEUE" default:"0"`
	SchedMaxBacklog     int     `help:"Max jobs accepted into the durable backlog; further submissions are refused. -1 = unbounded. 0 = file / built-in default." env:"KVARN_SCHED_MAX_BACKLOG" default:"0"`
	SchedMaxQueueWait   string  `help:"Fail a backlog entry that has waited this long undispatched (e.g. 24h). 0 = never. Empty = file / built-in default." env:"KVARN_SCHED_MAX_QUEUE_WAIT" default:""`
	SchedMaxAttempts    int     `help:"Max dispatches per job; a restart-interrupted run spends one. -1 = no cap. 0 = file / built-in default." env:"KVARN_SCHED_MAX_ATTEMPTS" default:"0"`
	SchedMaxClones      int     `help:"Max jobs cloning at once before admission. -1 = unbounded. 0 = file / built-in default." env:"KVARN_SCHED_MAX_CLONES" default:"0"`
	SchedMaxVMLifetime  string  `help:"Host-wide per-VM wall-time failsafe (e.g. 4h). Empty = file / built-in default." env:"KVARN_SCHED_MAX_VM_LIFETIME" default:""`

	OtelMetricsEnabled   bool   `help:"Enable OpenTelemetry metrics export." env:"KVARN_OTEL_METRICS_ENABLED"`
	OtelExporterEndpoint string `help:"OTLP metrics endpoint (host:port). Empty = honor OTEL_EXPORTER_OTLP_ENDPOINT." env:"KVARN_OTEL_EXPORTER_OTLP_ENDPOINT"`
	OtelServiceName      string `help:"service.name resource attribute." env:"KVARN_OTEL_SERVICE_NAME" default:"kvarn-orchestrator"`
}

// defaultCPUOvercommit is the built-in CPU overcommit multiplier used when
// neither the CLI flag nor the orchestrator.toml file set one.
const defaultCPUOvercommit = 1.5

// defaultDiskOvercommit is the built-in disk overcommit multiplier. A job is
// charged its VM's virtual disk (16 GiB by default) while the backing image is
// thin and a typical job writes a few gigabytes into it, so charging the full
// request idles most of the pool and stalls jobs behind a number nothing on the
// host is actually using. 3.0 stays well inside that gap, and the disk floor
// guard — not this multiplier — is what keeps the host from filling.
const defaultDiskOvercommit = 3.0

// defaultDiskFloorFraction is the share of the pre-overcommit disk pool kept
// free as the guard's floor when the operator sets none. It scales with the
// host instead of being an absolute size, so the same default is sane on a
// laptop and on a build server.
const defaultDiskFloorFraction = 0.10

// defaultBackfillGrace is the built-in window in which a queued job may be
// passed over by later ones that fit. It is short relative to a job's own
// runtime — a clone, a VM boot and an agent run — so letting a burst of small
// jobs through costs a large job little, while a job stuck behind an
// indefinite stream of small ones is the failure this bounds.
const defaultBackfillGrace = time.Minute

// defaultPriorityAgeStep is the built-in aging rate. It is long enough that a
// priority difference still decides the common case, and short enough that a
// job passed over by higher-priority work catches up within a few jobs'
// runtime rather than a shift.
const defaultPriorityAgeStep = 5 * time.Minute

// defaultMaxQueue bounds the in-memory pipeline when the operator sets none.
// Its job is to stop an unbounded accumulation of clones, not to shape traffic:
// work beyond it is not refused, it waits in the durable backlog.
const defaultMaxQueue = 64

// defaultMaxBacklog bounds the durable backlog. It is two orders of magnitude
// above the pipeline because a backlog entry is a row rather than a clone and a
// goroutine: the bound exists to catch a runaway submitter, not to express how
// much work the host can hold.
const defaultMaxBacklog = 1000

// defaultMaxQueueWait is how long a job may sit in the backlog before it is
// failed. A day is far longer than any healthy wait, which is the point — it
// only catches work whose requester has long since stopped waiting for it,
// typically after the host was down.
const defaultMaxQueueWait = 24 * time.Hour

// defaultMaxAttempts caps dispatches per job. A restart mid-clone should not
// cost a job its run, but a job that takes the orchestrator down with it must
// not be retried indefinitely; three attempts distinguishes the two.
const defaultMaxAttempts = 3

// defaultMaxConcurrentClones bounds the work that happens before admission.
// Clones are I/O bound and mostly served from local mirrors, so a handful
// saturates the disk; the point is that queue depth must not translate into
// that many simultaneous clones.
const defaultMaxConcurrentClones = 4

// defaultMaxVMLifetime is the built-in failsafe applied when no operator
// override is configured. 24h is well above any expected job runtime but
// guarantees a hung VM is reaped within a day.
const defaultMaxVMLifetime = 24 * time.Hour

// localSocketPath resolves where the host-local control socket should live, or
// "" when the operator has turned it off. It is on by default because the
// alternative is that stopping your own orchestrator requires minting a key
// first — the socket is what keeps the host's operator from needing a
// credential to operate the host they already own.
func (c *Cmd) localSocketPath() string {
	if c.NoLocalSocket {
		return ""
	}
	if c.LocalSocket != "" {
		return c.LocalSocket
	}
	return localsock.DefaultPath()
}

func (c *Cmd) Run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Every clone, fetch and push shells out to git, so a host without one
	// fails every job. Say so at boot rather than one job at a time.
	if err := gitscm.CheckAvailable(); err != nil {
		return fmt.Errorf("git is required to run the orchestrator: %w", err)
	}

	downloadLogged := false
	diskImagePath, err := vm.EnsureDiskImage(ctx, vm.DownloadOpts{
		Path: c.DiskImagePath,
		Progress: func(_, total int64) {
			if !downloadLogged {
				downloadLogged = true
				slog.Info("downloading VM disk image", "total_bytes", total)
			}
		},
	})
	if err != nil {
		return fmt.Errorf("find disk image: %w", err)
	}

	p := local.NewProvider()
	base := vm.BaseImage{
		DiskImagePath: diskImagePath,
	}

	image, err := p.PrepareImage(ctx, base)
	if err != nil {
		return fmt.Errorf("prepare image: %w", err)
	}

	projectsPath := c.ProjectsFile
	if projectsPath == "" {
		projectsPath = projtoml.DefaultPath()
	}
	// One store serves both the named forge credentials and the [llm] block
	// of provider API keys.
	credStore := credtoml.OpenDefault(c.CredentialsFile)
	secretsPath := c.SecretsFile
	if secretsPath == "" {
		secretsPath = secrettoml.DefaultPath()
	}
	forgesPath := c.ForgesFile
	if forgesPath == "" {
		forgesPath = forgetoml.DefaultPath()
	}
	apiKeysPath := c.APIKeysFile
	if apiKeysPath == "" {
		apiKeysPath = apikeytoml.DefaultPath()
	}
	apiKeyStore := apikeytoml.New(apiKeysPath)

	if c.NoAuth {
		slog.Warn("API-key auth disabled — do not expose the orchestrator to untrusted networks")
	} else if keys, err := apiKeyStore.List(ctx); err != nil {
		slog.Warn("failed to read API key store; requests will be rejected until it is readable", "path", apiKeysPath, "error", err)
	} else if len(keys) == 0 {
		slog.Warn("API-key auth enabled but no keys configured; all requests will be rejected until `kvarn key create`", "path", apiKeysPath)
	}

	logger := slog.Default()
	mgr, err := llms.NewManager(
		llms.WithManagerLogger(logger),
		llms.WithManagerCredentials(llmauth.NewSource(credStore.LLM())),
	)
	if err != nil {
		return fmt.Errorf("create llms manager: %w", err)
	}

	// One store instance serves both the named forge configs and the global
	// [defaults] block.
	forgeStore := forgetoml.New(forgesPath)

	agentsStore := modeltoml.OpenDefault(c.AgentsFile)
	modelResolver := coding.NewResolver(mgr, agentsStore, c.Model)

	// Resolve once here so a bad model alias or a missing credential is
	// reported at startup, by one clear message, rather than by whichever job
	// happens to run first. Jobs resolve again as they start, so an edit made
	// afterwards still applies without a restart.
	if _, err := modelResolver.Resolve(ctx); err != nil {
		return err
	}

	orchPath := c.OrchestratorFile
	if orchPath == "" {
		orchPath = orchcfg.DefaultPath()
	}
	orchFile, err := orchcfg.Load(orchPath)
	if err != nil {
		return fmt.Errorf("load orchestrator config: %w", err)
	}

	sched, dispatchPolicy, err := c.buildScheduler(orchFile.Scheduler)
	if err != nil {
		return fmt.Errorf("scheduler: %w", err)
	}

	maxLifetime, err := c.resolveMaxVMLifetime(orchFile.Scheduler)
	if err != nil {
		return fmt.Errorf("scheduler: %w", err)
	}

	tenantLimits, err := resolveTenantLimits(orchFile.Scheduler)
	if err != nil {
		return fmt.Errorf("scheduler: %w", err)
	}

	cacheProvider, err := cache.DefaultFileCache()
	if err != nil {
		return fmt.Errorf("set up cache: %w", err)
	}
	cacheQuota, err := resolveCacheQuota(orchFile.Cache)
	if err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	slog.Info("cache quota",
		"per_project_bytes", cacheQuota.PerProjectBytes,
		"global_bytes", cacheQuota.GlobalBytes,
		"dir", cacheProvider.BaseDir,
	)

	reposCfg, err := resolveReposConfig(orchFile.Repos)
	if err != nil {
		return fmt.Errorf("repos: %w", err)
	}
	var repoMirror *mirror.Store
	if reposCfg.Enabled {
		reposDir := reposCfg.Dir
		if reposDir == "" {
			if reposDir, err = mirror.DefaultDir(); err != nil {
				return fmt.Errorf("repos dir: %w", err)
			}
		}
		repoMirror = mirror.New(reposDir)
		// The mirrors share a filesystem with the image cache the scheduler
		// sizes its VM disk pool from, so an operator reading this line can see
		// what is competing for that space.
		slog.Info("repository mirrors enabled",
			"dir", reposDir,
			"prefetch", reposCfg.Policy.Prefetch,
			"prefetch_interval", reposCfg.Policy.PrefetchInterval,
			"mirror_depth", reposCfg.Policy.MirrorDepth,
			"branch_retention", reposCfg.Policy.BranchRetention,
			"global_bytes", reposCfg.Policy.GlobalBytes,
		)
	} else {
		slog.Info("repository mirrors disabled; every job clones from its forge")
	}

	imageCacheCfg, err := resolveImageCacheConfig(orchFile.ImageCache)
	if err != nil {
		return fmt.Errorf("image-cache: %w", err)
	}
	var imageCacheNet vm.NetworkConfig
	if imageCacheCfg.Enabled {
		dir, err := imagestore.DefaultDir()
		if err != nil {
			return fmt.Errorf("image-cache dir: %w", err)
		}
		st := imagestore.New(dir)
		handler := imageproxy.New(imageproxy.Config{
			Store:            st,
			Upstreams:        imageCacheCfg.Upstreams,
			ManifestTagTTL:   imageCacheCfg.ManifestTagTTL,
			GlobalQuotaBytes: imageCacheCfg.GlobalBytes,
		})
		imageCacheNet = vm.NetworkConfig{
			ImageCacheHandler:   handler,
			ImageCachePort:      imageCacheCfg.Port,
			ImageCacheUpstreams: imageCacheCfg.Upstreams,
		}
		slog.Info("image cache enabled",
			"dir", dir,
			"listen", fmt.Sprintf("%s:%d", imageCacheCfg.GatewayHost, imageCacheCfg.Port),
			"upstreams", imageCacheCfg.Upstreams,
			"global_bytes", imageCacheCfg.GlobalBytes,
			"manifest_tag_ttl", imageCacheCfg.ManifestTagTTL,
		)
	}

	meter, shutdownMeter, err := metrics.Setup(ctx, metrics.Config{
		Enabled:     c.OtelMetricsEnabled,
		Endpoint:    c.OtelExporterEndpoint,
		ServiceName: c.OtelServiceName,
		Version:     buildinfo.Version,
	})
	if err != nil {
		metrics.LogStartupError(err)
		meter, shutdownMeter, _ = metrics.Setup(ctx, metrics.Config{}) // no-op fallback
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownMeter(shutdownCtx); err != nil {
			slog.Warn("metrics shutdown error", "error", err)
		}
	}()

	sessionsDBPath := c.SessionsDB
	if sessionsDBPath == "" {
		sessionsDBPath = sessionsqlite.DefaultPath()
	}
	sessionStore, err := sessionsqlite.New(sessionsDBPath)
	if err != nil {
		return fmt.Errorf("open sessions database: %w", err)
	}
	defer sessionStore.Close()
	slog.Info("sessions database", "path", sessionsDBPath)

	// VMs do not survive an orchestrator restart, so no session left mid-run in
	// the store can continue where it left off. What differs is whether
	// starting it over is safe: a run that had only cloned, queued or booted a
	// VM goes back to the backlog and runs again, while one that had already
	// spent budget or pushed a branch is failed. This happens before the
	// listener opens, so the dispatcher never races a session it is about to
	// requeue. See session.RestartableStates for where the line falls and why.
	maxAttempts := resolveCount(c.SchedMaxAttempts, orchFile.Scheduler.MaxAttempts, defaultMaxAttempts)
	reconciled, err := sessionStore.ReconcileStartup(ctx, session.ReconcileOpts{
		MaxAttempts:    maxAttempts,
		RequeueMessage: "Requeued after the orchestrator restarted",
		FailError:      "orchestrator restarted",
	})
	if err != nil {
		return fmt.Errorf("reconcile sessions: %w", err)
	}
	if n := len(reconciled.Requeued); n > 0 {
		slog.Info("requeued interrupted jobs", "count", n, "max_attempts", maxAttempts)
	}
	if n := len(reconciled.Failed); n > 0 {
		slog.Warn("failed interrupted jobs that could not be safely restarted", "count", n)
	}

	retention, err := resolveSessionRetention(orchFile.Sessions)
	if err != nil {
		return fmt.Errorf("sessions: %w", err)
	}
	startSessionRetention(ctx, sessionStore, retention)

	// Previews are opt-in: without a [preview] section the store is never
	// opened, no ingress listener is bound, and every preview RPC reports the
	// feature as unimplemented.
	previewPolicy, err := resolvePreviewPolicy(orchFile.Preview)
	if err != nil {
		return fmt.Errorf("preview: %w", err)
	}
	var previewStore preview.Store
	var previewSnapshots snapshot.Store
	if previewPolicy.Enabled() {
		dbPath := c.PreviewsDB
		if dbPath == "" {
			dbPath = previewsqlite.DefaultPath()
		}
		store, err := previewsqlite.New(dbPath)
		if err != nil {
			return fmt.Errorf("open previews database: %w", err)
		}
		defer store.Close()
		previewStore = store
		slog.Info("previews database", "path", dbPath, "domain", previewPolicy.Domain)

		// Preview state sits beside the caches rather than inside them: same
		// root, its own directory, and swept by age alone, because nothing here
		// can be rebuilt by re-running a job.
		stateDir, err := snapshot.DefaultDir()
		if err != nil {
			return fmt.Errorf("preview state directory: %w", err)
		}
		previewSnapshots = snapshot.NewFileStore(stateDir)
		slog.Info("preview state", "path", stateDir, "retention", previewPolicy.StateRetention)
	}

	// Disk overcommit is only safe while something watches the real
	// filesystem, so the guard runs for the orchestrator's whole life.
	go sched.WatchHostDisk(ctx, 0)

	sessionMgr := session.NewManager(sessionStore)
	instruments, err := metrics.NewInstruments(meter,
		func(ctx context.Context) (int64, error) {
			ss, err := sessionMgr.List(ctx, session.SessionFilter{ActiveOnly: true})
			if err != nil {
				return 0, err
			}
			return int64(len(ss)), nil
		},
		func() metrics.SchedulerSample {
			used, free, queued := sched.Snapshot()
			hostFree, _, measured, open := sched.DiskGuard()
			// Backlog depth answers a question queue depth cannot: a full
			// pipeline with an empty backlog is a busy host, while a deep
			// backlog is work the host is not keeping up with at all.
			backlog, err := sessionStore.CountPending(ctx)
			if err != nil {
				backlog = -1
			}
			return metrics.SchedulerSample{
				BacklogDepth:      int64(backlog),
				CPUMillisUsed:     int64(used.CPUMillis),
				CPUMillisTotal:    int64(used.CPUMillis + free.CPUMillis),
				MemBytesUsed:      int64(used.MemBytes),
				MemBytesTotal:     int64(used.MemBytes + free.MemBytes),
				DiskBytesUsed:     int64(used.DiskBytes),
				DiskBytesTotal:    int64(used.DiskBytes + free.DiskBytes),
				QueueDepth:        int64(queued),
				HostDiskFreeBytes: int64(hostFree),
				HostDiskMeasured:  measured,
				AdmissionPaused:   !open,
			}
		},
	)
	if err != nil {
		metrics.LogStartupError(err)
		instruments = nil
	}
	defer instruments.Close()

	return run(ctx, serveOpts{
		Addr:           c.Addr,
		LocalSocket:    c.localSocketPath(),
		PreviewIngress: orchFile.Preview.Listen,
	}, ServiceOpts{
		Provider:           p,
		CreateOpts:         vm.CreateOpts{Image: image, MaxLifetime: maxLifetime, Network: imageCacheNet},
		ProjectStore:       projtoml.New(projectsPath),
		CredentialStore:    credStore,
		SecretStore:        secrettoml.New(secretsPath),
		ForgeConfigStore:   forgeStore,
		ForgeDefaultsStore: forgeStore,
		ForgeTypes: map[string]forge.Forge{
			"github": forgegithub.New(),
			"git":    forgegit.New(),
		},
		SessionMgr:          sessionMgr,
		Agent:               coding.NewCodingAgent(modelResolver),
		Transferer:          &transfer.StreamingTransferer{},
		DefaultsStore:       agentsStore,
		PricingManager:      llms.NewPricingManager(logger),
		APIKeyStore:         apiKeyStore,
		AuthEnabled:         !c.NoAuth,
		Scheduler:           sched,
		Dispatch:            dispatchPolicy,
		TenantLimits:        tenantLimits,
		MaxConcurrentClones: resolveCount(c.SchedMaxClones, orchFile.Scheduler.MaxConcurrentClones, defaultMaxConcurrentClones),
		CacheProvider:       cacheProvider,
		CacheQuota:          cacheQuota,
		RepoMirror:          repoMirror,
		RepoPolicy:          reposCfg.Policy,
		Meter:               meter,
		Instruments:         instruments,
		PreviewStore:        previewStore,
		PreviewPolicy:       previewPolicy,
		PreviewSnapshots:    previewSnapshots,
	})
}

// Preview defaults applied when [preview] leaves a field unset. They are sized
// for a preview somebody is looking at rather than for a job: half an hour of
// silence is a person who has closed the tab, and eight hours is a working day
// after which the branch is worth re-deriving from scratch.
const (
	defaultPreviewIdleTimeout   = 30 * time.Minute
	defaultPreviewMaxLifetime   = 8 * time.Hour
	defaultPreviewMaxConcurrent = 3
	// A month of retention is long enough that coming back to a pull request
	// after a holiday still finds the preview as it was left, and short enough
	// that a merged branch nobody deleted does not keep a database on the host
	// forever. Restoring a preview restarts the clock.
	defaultPreviewStateRetention = 30 * 24 * time.Hour
)

// resolvePreviewPolicy turns the [preview] table into the resolved policy. An
// absent section yields a zero policy, which disables previews.
func resolvePreviewPolicy(cfg orchcfg.Preview) (PreviewPolicy, error) {
	if cfg.Domain == "" {
		if cfg.Listen != "" {
			return PreviewPolicy{}, errors.New("listen is set but domain is not; a preview with no name is unaddressable")
		}
		return PreviewPolicy{}, nil
	}
	if cfg.Listen == "" {
		return PreviewPolicy{}, errors.New("domain is set but listen is not; a preview with no listener is unreachable")
	}

	policy := PreviewPolicy{
		Domain:        strings.Trim(cfg.Domain, "."),
		IdleTimeout:   defaultPreviewIdleTimeout,
		MaxLifetime:   defaultPreviewMaxLifetime,
		MaxConcurrent: defaultPreviewMaxConcurrent,
	}

	var err error
	if policy.IdleTimeout, err = resolvePreviewDuration(cfg.IdleTimeout, "idle_timeout", defaultPreviewIdleTimeout); err != nil {
		return PreviewPolicy{}, err
	}
	if policy.MaxLifetime, err = resolvePreviewDuration(cfg.MaxLifetime, "max_lifetime", defaultPreviewMaxLifetime); err != nil {
		return PreviewPolicy{}, err
	}
	if cfg.MaxConcurrent != nil {
		if *cfg.MaxConcurrent < 0 {
			return PreviewPolicy{}, errors.New("max_concurrent must not be negative")
		}
		policy.MaxConcurrent = *cfg.MaxConcurrent
	}
	if cfg.MaxMemory != "" {
		size, err := projconfig.ParseSize(cfg.MaxMemory)
		if err != nil {
			return PreviewPolicy{}, fmt.Errorf("max_memory: %w", err)
		}
		policy.MaxMemoryBytes = uint64(size)
	}
	if cfg.MaxDisk != "" {
		size, err := projconfig.ParseSize(cfg.MaxDisk)
		if err != nil {
			return PreviewPolicy{}, fmt.Errorf("max_disk: %w", err)
		}
		policy.MaxDiskBytes = size
	}

	if policy.StateTimeout, err = resolvePreviewDuration(
		cfg.StateTimeout, "state_timeout", defaultPreviewStateTimeout); err != nil {
		return PreviewPolicy{}, err
	}
	if policy.StateTimeout == 0 {
		// Unlike the reaping timeouts, "0" here is not a way to disable
		// something: it is a capture with no budget, which is a drain that never
		// returns.
		return PreviewPolicy{}, errors.New("state_timeout must be greater than zero")
	}
	if policy.StateRetention, err = resolvePreviewDuration(
		cfg.StateRetention, "state_retention", defaultPreviewStateRetention); err != nil {
		return PreviewPolicy{}, err
	}
	if cfg.MaxStateSize != "" {
		size, err := projconfig.ParseSize(cfg.MaxStateSize)
		if err != nil {
			return PreviewPolicy{}, fmt.Errorf("max_state_size: %w", err)
		}
		policy.MaxStateBytes = size
	}
	return policy, nil
}

// resolvePreviewDuration parses one of the preview timeouts. Empty takes the
// default; an explicit "0" disables that cap.
func resolvePreviewDuration(value, field string, fallback time.Duration) (time.Duration, error) {
	if value == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", field, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s: must be non-negative", field)
	}
	return d, nil
}

// defaultCachePerProjectBytes / defaultCacheGlobalBytes are the built-in tool
// cache quotas applied when orchestrator.toml leaves them unset.
const (
	defaultCachePerProjectBytes = int64(10) * 1024 * 1024 * 1024  // 10 GiB
	defaultCacheGlobalBytes     = int64(100) * 1024 * 1024 * 1024 // 100 GiB
)

// imageCacheResolved is the resolved image-cache configuration applied by
// the orchestrator at startup.
type imageCacheResolved struct {
	Enabled        bool
	GatewayHost    string
	Port           uint16
	Upstreams      []string
	ManifestTagTTL time.Duration
	GlobalBytes    int64
}

const (
	defaultImageCacheGlobalBytes    = int64(50) * 1024 * 1024 * 1024 // 50 GiB
	defaultImageCacheManifestTagTTL = 5 * time.Minute
	defaultImageCacheListenAddr     = "10.0.2.1:5000"
)

var defaultImageCacheUpstreams = []string{"docker.io", "ghcr.io", "quay.io", "gcr.io"}

func resolveImageCacheConfig(c orchcfg.ImageCache) (imageCacheResolved, error) {
	out := imageCacheResolved{
		Enabled:        true,
		Upstreams:      defaultImageCacheUpstreams,
		ManifestTagTTL: defaultImageCacheManifestTagTTL,
		GlobalBytes:    defaultImageCacheGlobalBytes,
	}
	if c.Enabled != nil {
		out.Enabled = *c.Enabled
	}
	listen := c.ListenAddr
	if listen == "" {
		listen = defaultImageCacheListenAddr
	}
	host, port, err := splitHostPort(listen)
	if err != nil {
		return imageCacheResolved{}, fmt.Errorf("listen_addr: %w", err)
	}
	out.GatewayHost = host
	out.Port = port
	if len(c.Upstreams) > 0 {
		out.Upstreams = append([]string(nil), c.Upstreams...)
	}
	if c.ManifestTagTTL != "" {
		d, err := time.ParseDuration(c.ManifestTagTTL)
		if err != nil {
			return imageCacheResolved{}, fmt.Errorf("manifest_tag_ttl: %w", err)
		}
		if d <= 0 {
			return imageCacheResolved{}, fmt.Errorf("manifest_tag_ttl must be positive")
		}
		out.ManifestTagTTL = d
	}
	if c.GlobalBytes != "" {
		n, err := projconfig.ParseSize(c.GlobalBytes)
		if err != nil {
			return imageCacheResolved{}, fmt.Errorf("global_bytes: %w", err)
		}
		out.GlobalBytes = n
	}
	return out, nil
}

// splitHostPort parses a "host:port" pair, requiring port to be a positive
// uint16. The host portion is returned verbatim so IPv4/IPv6/hostnames all
// work; this layer doesn't validate it further because the gvisor listener
// will reject an invalid bind on startup.
func splitHostPort(s string) (string, uint16, error) {
	i := -1
	for j := len(s) - 1; j >= 0; j-- {
		if s[j] == ':' {
			i = j
			break
		}
	}
	if i < 0 || i == len(s)-1 {
		return "", 0, fmt.Errorf("expected host:port, got %q", s)
	}
	host := s[:i]
	portStr := s[i+1:]
	p, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil || p == 0 {
		return "", 0, fmt.Errorf("invalid port %q", portStr)
	}
	return host, uint16(p), nil
}

// Built-in [repos] defaults, applied when orchestrator.toml leaves a field
// unset. Prefetch is on because the alternative is that the first job on every
// project pays for the initial mirror clone on the critical path.
const (
	defaultRepoPrefetchInterval = 5 * time.Minute
	defaultRepoBranchRetention  = 720 * time.Hour // 30 days
)

// reposResolved is the resolved repository-mirror configuration.
type reposResolved struct {
	Enabled bool
	Dir     string
	Policy  RepoPolicy
}

// resolveReposConfig parses the [repos] table. Durations use time.ParseDuration
// units; an explicit "0" means "never" for branch_retention rather than falling
// back to the default.
func resolveReposConfig(c orchcfg.Repos) (reposResolved, error) {
	out := reposResolved{
		Enabled: true,
		Dir:     c.Dir,
		Policy: RepoPolicy{
			Prefetch:         true,
			PrefetchInterval: defaultRepoPrefetchInterval,
			BranchRetention:  defaultRepoBranchRetention,
		},
	}
	if c.Enabled != nil {
		out.Enabled = *c.Enabled
	}
	if c.Prefetch != nil {
		out.Policy.Prefetch = *c.Prefetch
	}
	if c.PrefetchInterval != "" {
		d, err := time.ParseDuration(c.PrefetchInterval)
		if err != nil {
			return reposResolved{}, fmt.Errorf("prefetch_interval: %w", err)
		}
		if d <= 0 {
			return reposResolved{}, fmt.Errorf("prefetch_interval must be positive")
		}
		out.Policy.PrefetchInterval = d
	}
	if c.MirrorDepth != nil {
		if *c.MirrorDepth < 0 {
			return reposResolved{}, fmt.Errorf("mirror_depth must be non-negative")
		}
		out.Policy.MirrorDepth = *c.MirrorDepth
	}
	if c.BranchRetention != "" {
		d, err := time.ParseDuration(c.BranchRetention)
		if err != nil {
			return reposResolved{}, fmt.Errorf("branch_retention: %w", err)
		}
		if d < 0 {
			return reposResolved{}, fmt.Errorf("branch_retention must be non-negative")
		}
		out.Policy.BranchRetention = d
	}
	if c.GlobalBytes != "" {
		n, err := projconfig.ParseSize(c.GlobalBytes)
		if err != nil {
			return reposResolved{}, fmt.Errorf("global_bytes: %w", err)
		}
		out.Policy.GlobalBytes = n
	}
	return out, nil
}

// resolveCacheQuota parses the [cache] quotas, falling back to the built-in
// defaults when a field is empty.
func resolveCacheQuota(c orchcfg.Cache) (cache.Quota, error) {
	q := cache.Quota{
		PerProjectBytes: defaultCachePerProjectBytes,
		GlobalBytes:     defaultCacheGlobalBytes,
	}
	if c.PerProjectBytes != "" {
		n, err := projconfig.ParseSize(c.PerProjectBytes)
		if err != nil {
			return cache.Quota{}, fmt.Errorf("per_project_bytes: %w", err)
		}
		q.PerProjectBytes = n
	}
	if c.GlobalBytes != "" {
		n, err := projconfig.ParseSize(c.GlobalBytes)
		if err != nil {
			return cache.Quota{}, fmt.Errorf("global_bytes: %w", err)
		}
		q.GlobalBytes = n
	}
	return q, nil
}

// buildScheduler resolves the scheduler pool with CLI > TOML > host precedence.
// Host fallbacks: NumCPU / 75% memory / 75% free disk on the image cache
// filesystem. Rejects degenerate configurations early so the orchestrator
// never starts with a pool that can't admit anything.
//
// It also returns the backlog dispatch policy, because the two must agree:
// max_queue is the pipeline population the dispatcher fills and the queue bound
// the scheduler guards, and both tiers age a waiting job at the same rate.
func (c *Cmd) buildScheduler(fileCfg orchcfg.Scheduler) (*scheduler.Scheduler, DispatchPolicy, error) {
	overcommit := c.SchedCPUOvercommit
	if overcommit == 0 && fileCfg.CPUOvercommit != nil {
		overcommit = *fileCfg.CPUOvercommit
	}
	if overcommit == 0 {
		overcommit = defaultCPUOvercommit
	}
	if overcommit < 1.0 {
		return nil, DispatchPolicy{}, fmt.Errorf("cpu_overcommit must be >= 1.0, got %g", overcommit)
	}

	cpus := uint64(c.SchedCPUs)
	if cpus == 0 && fileCfg.CPUs != nil {
		cpus = uint64(*fileCfg.CPUs)
	}
	if cpus == 0 {
		cpus = uint64(scheduler.HostCPUMillis()) / 1000
	}
	cpuMillis := uint64(float64(cpus*1000) * overcommit)

	memBytes, err := resolveSize(c.SchedMemory, fileCfg.Memory, "--sched-memory", "scheduler.memory",
		func() (uint64, error) {
			host, err := scheduler.HostMemoryBytes()
			if err != nil {
				return 0, fmt.Errorf("detect host memory: %w", err)
			}
			return scheduler.FractionOf(host), nil
		})
	if err != nil {
		return nil, DispatchPolicy{}, err
	}

	diskOvercommit := c.SchedDiskOvercommit
	if diskOvercommit == 0 && fileCfg.DiskOvercommit != nil {
		diskOvercommit = *fileCfg.DiskOvercommit
	}
	if diskOvercommit == 0 {
		diskOvercommit = defaultDiskOvercommit
	}
	if diskOvercommit < 1.0 {
		return nil, DispatchPolicy{}, fmt.Errorf("disk_overcommit must be >= 1.0, got %g", diskOvercommit)
	}

	// The disk pool and the guard both concern one filesystem — the one VM
	// disks are allocated on — so the path is resolved even when the operator
	// sizes the pool by hand and the host fallback below never runs.
	diskPath, err := scheduler.DefaultImageCacheDir()
	if err != nil {
		return nil, DispatchPolicy{}, err
	}

	rawDisk, err := resolveSize(c.SchedDisk, fileCfg.Disk, "--sched-disk", "scheduler.disk",
		func() (uint64, error) {
			free, err := scheduler.HostFreeDiskBytes(diskPath)
			if err != nil {
				return 0, fmt.Errorf("detect free disk: %w", err)
			}
			return scheduler.FractionOf(free), nil
		})
	if err != nil {
		return nil, DispatchPolicy{}, err
	}
	diskBytes := uint64(float64(rawDisk) * diskOvercommit)

	// The floor is a fraction of the pool before overcommit, so raising the
	// multiplier does not quietly raise the reserve along with it.
	diskFloor, err := resolveSize(c.SchedDiskFloor, fileCfg.DiskFloor,
		"--sched-disk-floor", "scheduler.disk_floor",
		func() (uint64, error) {
			return uint64(float64(rawDisk) * defaultDiskFloorFraction), nil
		})
	if err != nil {
		return nil, DispatchPolicy{}, err
	}

	total := scheduler.Capacity{CPUMillis: cpuMillis, MemBytes: memBytes, DiskBytes: diskBytes}
	if total.CPUMillis == 0 || total.MemBytes == 0 || total.DiskBytes == 0 {
		return nil, DispatchPolicy{}, fmt.Errorf("admission pool has a zero dimension: %+v", total)
	}

	grace, err := resolveDuration(c.SchedBackfillGrace, fileCfg.BackfillGrace,
		"--sched-backfill-grace", "scheduler.backfill_grace", defaultBackfillGrace)
	if err != nil {
		return nil, DispatchPolicy{}, err
	}
	if grace < 0 {
		return nil, DispatchPolicy{}, fmt.Errorf("backfill_grace must be non-negative")
	}

	ageStep, err := resolveDuration(c.SchedPriorityAge, fileCfg.PriorityAgeStep,
		"--sched-priority-age", "scheduler.priority_age_step", defaultPriorityAgeStep)
	if err != nil {
		return nil, DispatchPolicy{}, err
	}
	if ageStep < 0 {
		return nil, DispatchPolicy{}, fmt.Errorf("priority_age_step must be non-negative")
	}

	maxQueueWait, err := resolveDuration(c.SchedMaxQueueWait, fileCfg.MaxQueueWait,
		"--sched-max-queue-wait", "scheduler.max_queue_wait", defaultMaxQueueWait)
	if err != nil {
		return nil, DispatchPolicy{}, err
	}
	if maxQueueWait < 0 {
		return nil, DispatchPolicy{}, fmt.Errorf("max_queue_wait must be non-negative")
	}

	maxQueue := resolveCount(c.SchedMaxQueue, fileCfg.MaxQueue, defaultMaxQueue)
	dispatch := DispatchPolicy{
		MaxDispatched: maxQueue,
		// The backlog ages a waiting job at the same rate the admission queue
		// does, so a job's position is decided by one rule rather than by which
		// side of dispatch it happens to be on.
		PriorityAgeStep: ageStep,
		MaxBacklog:      resolveCount(c.SchedMaxBacklog, fileCfg.MaxBacklog, defaultMaxBacklog),
		MaxQueueWait:    maxQueueWait,
		MaxAttempts:     resolveCount(c.SchedMaxAttempts, fileCfg.MaxAttempts, defaultMaxAttempts),
	}

	slog.Info("scheduler pool",
		"cpu_millis", total.CPUMillis,
		"mem_bytes", total.MemBytes,
		"disk_bytes", total.DiskBytes,
		"cpu_overcommit", overcommit,
		"disk_overcommit", diskOvercommit,
		"disk_floor_bytes", diskFloor,
		"disk_path", diskPath,
		"backfill_grace", grace.String(),
		"priority_age_step", ageStep.String(),
		"max_queue", maxQueue,
		"max_backlog", dispatch.MaxBacklog,
		"max_queue_wait", maxQueueWait.String(),
	)

	return scheduler.New(scheduler.Options{
		Total: total,
		// The dispatcher never pushes more than MaxDispatched into the
		// pipeline, so this bound is a guard on that invariant rather than a
		// limit submissions meet: backpressure is the backlog's job now.
		MaxQueue:       maxQueue,
		CPUOvercommit:  overcommit,
		DiskOvercommit: diskOvercommit,
		DiskPath:       diskPath,
		DiskFloorBytes: diskFloor,
		// The three wrappers each do one thing — Capped hides who may not run,
		// Fair decides who deserves the host next, Backfill picks the first of
		// those that fits — and all are wired unconditionally: with nothing
		// configured Capped hides nobody, Fair ties every waiter and falls back
		// to arrival order, and a zero grace makes Backfill exactly FIFO.
		Policy: scheduler.Capped{
			Inner: scheduler.Fair{
				AgeStep: ageStep,
				Inner:   scheduler.Backfill{Grace: grace},
			},
		},
	}), dispatch, nil
}

// resolveCount applies CLI > file > built-in precedence to a bound expressed as
// a count. Zero means "unset" on both inputs so the default can be reached, and
// a negative value is how an operator asks for no bound at all — a distinction
// the value itself cannot carry, since zero is already the encoding for
// unbounded in the field being set.
func resolveCount(flagVal int, fileVal *int, def int) int {
	v := def
	if fileVal != nil {
		v = *fileVal
	}
	if flagVal != 0 {
		v = flagVal
	}
	if v < 0 {
		return 0
	}
	return v
}

// resolveDuration applies CLI > file > built-in precedence to a duration field.
// An explicit "0" is honored as zero; only an empty string falls through to the
// default.
func resolveDuration(flagVal, fileVal, flagName, fileField string, def time.Duration) (time.Duration, error) {
	raw, source := flagVal, flagName
	if raw == "" {
		raw, source = fileVal, fileField
	}
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", source, err)
	}
	return d, nil
}

// resolveMaxVMLifetime applies CLI > file > built-in precedence to the
// per-VM wall-time failsafe. Returns 0 only if the operator explicitly sets
// the lifetime to "0" — the empty string falls through to the default.
func (c *Cmd) resolveMaxVMLifetime(fileCfg orchcfg.Scheduler) (time.Duration, error) {
	d, err := resolveDuration(c.SchedMaxVMLifetime, fileCfg.MaxVMLifetime,
		"--sched-max-vm-lifetime", "scheduler.max_vm_lifetime", defaultMaxVMLifetime)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("max_vm_lifetime must be non-negative")
	}
	return d, nil
}

// defaultSessionRetention is how long terminal sessions are kept when the
// operator leaves [sessions].retention unset.
const defaultSessionRetention = 720 * time.Hour // 30 days

// resolveSessionRetention parses [sessions].retention. An empty value falls
// through to the built-in default; an explicit "0" disables pruning (keep
// forever). time.ParseDuration units apply (e.g. "720h").
func resolveSessionRetention(cfg orchcfg.Sessions) (time.Duration, error) {
	if cfg.Retention == "" {
		return defaultSessionRetention, nil
	}
	d, err := time.ParseDuration(cfg.Retention)
	if err != nil {
		return 0, fmt.Errorf("sessions.retention: %w", err)
	}
	if d < 0 {
		return 0, fmt.Errorf("sessions.retention: must be non-negative")
	}
	return d, nil
}

// startSessionRetention prunes terminal sessions older than retention once at
// startup and then hourly until ctx is cancelled. A non-positive retention
// keeps sessions forever (no pruning).
func startSessionRetention(ctx context.Context, store session.Store, retention time.Duration) {
	if retention <= 0 {
		slog.Info("session retention disabled; keeping terminal sessions forever")
		return
	}
	prune := func() {
		n, err := store.PruneTerminalBefore(ctx, time.Now().Add(-retention))
		if err != nil {
			slog.Warn("session prune failed", "error", err)
		} else if n > 0 {
			slog.Info("pruned terminal sessions", "count", n, "older_than", retention)
		}
	}
	prune()
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				prune()
			}
		}
	}()
}

// resolveSize applies CLI > file > host precedence to a size field. flagName /
// fileField are surfaced in error messages so the operator can tell which input
// failed to parse.
func resolveSize(flagVal, fileVal, flagName, fileField string, host func() (uint64, error)) (uint64, error) {
	if flagVal != "" {
		n, err := projconfig.ParseSize(flagVal)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", flagName, err)
		}
		return uint64(n), nil
	}
	if fileVal != "" {
		n, err := projconfig.ParseSize(fileVal)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", fileField, err)
		}
		return uint64(n), nil
	}
	return host()
}
