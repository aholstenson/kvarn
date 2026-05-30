package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aholstenson/kvarn/internal/agent/coding"
	apikeytoml "github.com/aholstenson/kvarn/internal/config/apikey/tomlstore"
	credtoml "github.com/aholstenson/kvarn/internal/config/credential/tomlstore"
	forgetoml "github.com/aholstenson/kvarn/internal/config/forge/tomlstore"
	modelcfg "github.com/aholstenson/kvarn/internal/config/model"
	orchcfg "github.com/aholstenson/kvarn/internal/config/orchestrator"
	modeltoml "github.com/aholstenson/kvarn/internal/config/model/tomlstore"
	projtoml "github.com/aholstenson/kvarn/internal/config/project/tomlstore"
	secrettoml "github.com/aholstenson/kvarn/internal/config/secret/tomlstore"
	"github.com/aholstenson/kvarn/internal/forge"
	forgegit "github.com/aholstenson/kvarn/internal/forge/git"
	forgegithub "github.com/aholstenson/kvarn/internal/forge/github"
	"github.com/aholstenson/kvarn/internal/orchestrator/scheduler"
	projconfig "github.com/aholstenson/kvarn/internal/project"
	"github.com/aholstenson/kvarn/internal/sandbox/transfer"
	"github.com/aholstenson/kvarn/internal/session"
	"github.com/aholstenson/kvarn/internal/vm"
	"github.com/aholstenson/kvarn/internal/vm/local"
	llms "github.com/aholstenson/llms-go"
)

type Cmd struct {
	Addr            string `help:"Address to listen on." default:":8080"`
	DiskImagePath   string `help:"Path to VM disk image. Auto-detected if not set."`
	ProjectsFile    string `help:"Path to projects TOML file." default:""`
	CredentialsFile string `help:"Path to credentials TOML file." default:""`
	SecretsFile     string `help:"Path to per-project secrets TOML file." default:""`
	ForgesFile      string `help:"Path to forges TOML file." default:""`
	AgentsFile      string `help:"Path to agents config TOML file." default:""`
	APIKeysFile     string `help:"Path to API keys TOML file." default:""`
	NoAuth            bool   `help:"Disable API-key auth (local dev only)." env:"KVARN_NO_AUTH"`
	Model             string `help:"LLM model alias for the coding agent." default:"coding-agent"`
	OrchestratorFile  string `help:"Path to orchestrator TOML file (host-level settings, e.g. scheduler pool)." default:""`

	SchedCPUs          uint    `help:"Total vCPUs in the admission pool. 0 = file / runtime.NumCPU()." env:"KVARN_SCHED_CPUS" default:"0"`
	SchedMemory        string  `help:"Total admission-pool memory (e.g. 32G). Empty = file / 75% of host total." env:"KVARN_SCHED_MEMORY" default:""`
	SchedDisk          string  `help:"Total admission-pool disk (e.g. 200G). Empty = file / 75% of free space on the image cache filesystem." env:"KVARN_SCHED_DISK" default:""`
	SchedCPUOvercommit float64 `help:"CPU overcommit multiplier (>=1.0). 0 = file / built-in default." env:"KVARN_SCHED_CPU_OVERCOMMIT" default:"0"`
	SchedMaxVMLifetime string  `help:"Host-wide per-VM wall-time failsafe (e.g. 4h). Empty = file / built-in default." env:"KVARN_SCHED_MAX_VM_LIFETIME" default:""`
}

// defaultCPUOvercommit is the built-in CPU overcommit multiplier used when
// neither the CLI flag nor the orchestrator.toml file set one.
const defaultCPUOvercommit = 1.5

// defaultMaxVMLifetime is the built-in failsafe applied when no operator
// override is configured. 24h is well above any expected job runtime but
// guarantees a hung VM is reaped within a day.
const defaultMaxVMLifetime = 24 * time.Hour

func (c *Cmd) Run() error {
	ctx := context.Background()

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
	credentialsPath := c.CredentialsFile
	if credentialsPath == "" {
		credentialsPath = credtoml.DefaultPath()
	}
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
	mgr, err := llms.NewManager(llms.WithManagerLogger(logger))
	if err != nil {
		return fmt.Errorf("create llms manager: %w", err)
	}

	// One store instance serves both the named forge configs and the global
	// [defaults] block.
	forgeStore := forgetoml.New(forgesPath)

	agentsStore := modeltoml.OpenDefault(c.AgentsFile)
	models, configs, err := modelcfg.Resolve(
		ctx, mgr,
		agentsStore,
		coding.DefaultModels(),
		coding.ModelMain, c.Model,
	)
	if err != nil {
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

	sched, err := c.buildScheduler(orchFile.Scheduler)
	if err != nil {
		return fmt.Errorf("scheduler: %w", err)
	}

	maxLifetime, err := c.resolveMaxVMLifetime(orchFile.Scheduler)
	if err != nil {
		return fmt.Errorf("scheduler: %w", err)
	}

	return run(c.Addr, ServiceOpts{
		Provider:           p,
		CreateOpts:         vm.CreateOpts{Image: image, MaxLifetime: maxLifetime},
		ProjectStore:       projtoml.New(projectsPath),
		CredentialStore:    credtoml.New(credentialsPath),
		SecretStore:        secrettoml.New(secretsPath),
		ForgeConfigStore:   forgeStore,
		ForgeDefaultsStore: forgeStore,
		ForgeTypes: map[string]forge.Forge{
			"github": forgegithub.New(),
			"git":    forgegit.New(),
		},
		SessionMgr:     session.NewMemoryManager(),
		Agent:          coding.NewCodingAgent(models, configs),
		Transferer:     &transfer.StreamingTransferer{},
		DefaultsStore:  agentsStore,
		PricingManager: llms.NewPricingManager(logger),
		APIKeyStore:    apiKeyStore,
		AuthEnabled:    !c.NoAuth,
		Scheduler:      sched,
	})
}

// buildScheduler resolves the scheduler pool with CLI > TOML > host precedence.
// Host fallbacks: NumCPU / 75% memory / 75% free disk on the image cache
// filesystem. Rejects degenerate configurations early so the orchestrator
// never starts with a pool that can't admit anything.
func (c *Cmd) buildScheduler(fileCfg orchcfg.Scheduler) (*scheduler.Scheduler, error) {
	overcommit := c.SchedCPUOvercommit
	if overcommit == 0 && fileCfg.CPUOvercommit != nil {
		overcommit = *fileCfg.CPUOvercommit
	}
	if overcommit == 0 {
		overcommit = defaultCPUOvercommit
	}
	if overcommit < 1.0 {
		return nil, fmt.Errorf("cpu_overcommit must be >= 1.0, got %g", overcommit)
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
		return nil, err
	}

	diskBytes, err := resolveSize(c.SchedDisk, fileCfg.Disk, "--sched-disk", "scheduler.disk",
		func() (uint64, error) {
			cacheDir, err := scheduler.DefaultImageCacheDir()
			if err != nil {
				return 0, err
			}
			free, err := scheduler.HostFreeDiskBytes(cacheDir)
			if err != nil {
				return 0, fmt.Errorf("detect free disk: %w", err)
			}
			return scheduler.FractionOf(free), nil
		})
	if err != nil {
		return nil, err
	}

	total := scheduler.Capacity{CPUMillis: cpuMillis, MemBytes: memBytes, DiskBytes: diskBytes}
	if total.CPUMillis == 0 || total.MemBytes == 0 || total.DiskBytes == 0 {
		return nil, fmt.Errorf("admission pool has a zero dimension: %+v", total)
	}

	slog.Info("scheduler pool",
		"cpu_millis", total.CPUMillis,
		"mem_bytes", total.MemBytes,
		"disk_bytes", total.DiskBytes,
		"cpu_overcommit", overcommit,
	)

	return scheduler.New(scheduler.Options{Total: total, CPUOvercommit: overcommit}), nil
}

// resolveMaxVMLifetime applies CLI > file > built-in precedence to the
// per-VM wall-time failsafe. Returns 0 only if the operator explicitly sets
// the lifetime to "0" — the empty string falls through to the default.
func (c *Cmd) resolveMaxVMLifetime(fileCfg orchcfg.Scheduler) (time.Duration, error) {
	raw := c.SchedMaxVMLifetime
	source := "--sched-max-vm-lifetime"
	if raw == "" {
		raw = fileCfg.MaxVMLifetime
		source = "scheduler.max_vm_lifetime"
	}
	if raw == "" {
		return defaultMaxVMLifetime, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", source, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s: must be non-negative", source)
	}
	return d, nil
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
