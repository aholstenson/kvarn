// Package orchestrator defines the host-level orchestrator config file
// (orchestrator.toml). It holds operator state that doesn't fit per-project
// stores — currently the admission-pool sizing — and is read once at startup.
package orchestrator

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

// Config is the parsed orchestrator.toml. Fields are pointers (where the zero
// value is meaningful, e.g. CPUs=0) so callers can distinguish "operator set
// this explicitly" from "operator left it unset, fall through to defaults".
type Config struct {
	Scheduler  Scheduler  `toml:"scheduler"`
	Cache      Cache      `toml:"cache"`
	ImageCache ImageCache `toml:"image-cache"`
	Sessions   Sessions   `toml:"sessions"`
	Repos      Repos      `toml:"repos"`
}

// Repos mirrors the [repos] table: the host-side bare mirror kept per project
// so that concurrent jobs on one repository share a single fetch. Empty fields
// fall through to the built-in defaults applied by the CLI layer.
//
// The mirrors live on the same filesystem the scheduler sizes its VM disk pool
// from, so an unbounded set of them quietly shrinks how much can be admitted;
// global_bytes is what caps that.
type Repos struct {
	// Enabled turns mirroring off; jobs then clone straight from the forge.
	Enabled *bool `toml:"enabled,omitempty"`
	// Dir overrides the mirror root (default ~/.cache/kvarn/repos).
	Dir string `toml:"dir,omitempty"`
	// Prefetch warms mirrors in the background so the first job on a project
	// does not pay for the initial clone.
	Prefetch *bool `toml:"prefetch,omitempty"`
	// PrefetchInterval is how often the background warm runs (e.g. "5m").
	PrefetchInterval string `toml:"prefetch_interval,omitempty"`
	// MirrorDepth bounds the history mirrors keep. 0 is full history.
	MirrorDepth *int `toml:"mirror_depth,omitempty"`
	// BranchRetention is how long an unused branch ref is kept (e.g. "720h").
	// "0" never prunes.
	BranchRetention string `toml:"branch_retention,omitempty"`
	// GlobalBytes caps the whole mirror store; least-recently-used projects are
	// evicted first. Empty means no cap.
	GlobalBytes string `toml:"global_bytes,omitempty"`
}

// Sessions mirrors the [sessions] table: retention policy for the persistent
// session store. Empty/unset falls through to the built-in default applied by
// the CLI layer.
type Sessions struct {
	// Retention is how long terminal sessions are kept (e.g. "720h", "30d").
	// "0" keeps them forever; empty falls through to the built-in default.
	Retention string `toml:"retention,omitempty"`
}

// ImageCache mirrors the [image-cache] table: configuration for the
// pull-through OCI image cache that sits on the per-VM gvisor gateway.
// Empty fields fall through to the built-in defaults applied by the CLI
// layer.
type ImageCache struct {
	Enabled        *bool    `toml:"enabled,omitempty"`
	ListenAddr     string   `toml:"listen_addr,omitempty"`
	GlobalBytes    string   `toml:"global_bytes,omitempty"`
	Upstreams      []string `toml:"upstreams,omitempty"`
	ManifestTagTTL string   `toml:"manifest_tag_ttl,omitempty"`
}

// Cache mirrors the [cache] table: the disk quotas for the tool-cache LRU
// sweep. Sizes are human-readable (e.g. "10G"); empty falls through to the
// built-in defaults applied by the CLI layer.
type Cache struct {
	PerProjectBytes string `toml:"per_project_bytes,omitempty"`
	GlobalBytes     string `toml:"global_bytes,omitempty"`
}

// Scheduler mirrors the [scheduler] table. Unset fields stay nil/empty so the
// CLI layer can apply precedence: flag > file > host detection.
type Scheduler struct {
	CPUs          *uint    `toml:"cpus,omitempty"`
	Memory        string   `toml:"memory,omitempty"`
	Disk          string   `toml:"disk,omitempty"`
	CPUOvercommit *float64 `toml:"cpu_overcommit,omitempty"`
	// DiskOvercommit multiplies the disk pool, because a job is charged its
	// VM's virtual disk size while the image on the host stays thin. Set it to
	// 1.0 to charge the full request.
	DiskOvercommit *float64 `toml:"disk_overcommit,omitempty"`
	// DiskFloor is how much real free space the VM disk filesystem must keep
	// (e.g. "20G"). Admission pauses below it, which is what makes disk
	// overcommit safe. "0" disables the guard; empty takes the default.
	DiskFloor string `toml:"disk_floor,omitempty"`
	// MaxVMLifetime is a host-wide failsafe upper bound on per-VM wall time
	// (e.g. "4h", "1d"). Empty falls through to the built-in default.
	MaxVMLifetime string `toml:"max_vm_lifetime,omitempty"`
	// BackfillGrace is how long a queued job may be skipped by ones behind it
	// that fit, before it holds the line and nothing new is admitted ahead of
	// it (e.g. "1m"). "0" is strict FIFO: a job that does not fit stalls every
	// job behind it. Empty falls through to the built-in default.
	BackfillGrace string `toml:"backfill_grace,omitempty"`
	// PriorityAgeStep is how long a queued job waits to gain one level of
	// effective priority, so a low-priority project is never starved by a
	// stream of high-priority ones (e.g. "5m"). "0" disables aging and lets
	// priority strictly dominate. Empty falls through to the built-in default.
	PriorityAgeStep string `toml:"priority_age_step,omitempty"`
	// PerProject and PerKey cap what any one project or API key may hold at
	// once, and are the defaults a project or key overrides in its own file.
	// Unset means uncapped, which is the right default for a single-tenant
	// host: a cap is worth configuring once several tenants share one.
	PerProject TenantLimits `toml:"per_project,omitempty"`
	PerKey     TenantLimits `toml:"per_key,omitempty"`
}

// TenantLimits mirrors a [scheduler.per_project] / [scheduler.per_key] table:
// how much one scope may run concurrently. Every field is optional, and an
// unset field caps that dimension not at all. Sizes are human-readable
// ("32G"); max_cpu counts whole vCPUs.
type TenantLimits struct {
	MaxJobs   *int   `toml:"max_jobs,omitempty"`
	MaxCPUs   *uint  `toml:"max_cpu,omitempty"`
	MaxMemory string `toml:"max_memory,omitempty"`
	MaxDisk   string `toml:"max_disk,omitempty"`
}

// DefaultPath returns the standard orchestrator.toml location, mirroring the
// other TOML stores under ~/.config/kvarn/.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "kvarn", "orchestrator.toml")
}

// Load reads and parses the config at path. A missing file is not an error —
// an empty Config is returned so callers can treat absence and "all fields
// unset" identically.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Config{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &cfg, nil
}
