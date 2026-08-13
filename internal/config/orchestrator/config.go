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
	Preview    Preview    `toml:"preview"`
}

// Preview mirrors the [preview] table: the operator half of preview
// environments. It is the layer that owns the domain and the resource envelope;
// a repository's kvarn.yml owns only the shape of the preview inside it.
//
// An absent section disables previews entirely, which is why Domain has no
// default: there is no domain kvarn could pick that the operator actually
// controls, and serving previews on a name nobody owns is worse than serving
// none.
type Preview struct {
	// Domain is the base domain preview hostnames are formed under, e.g.
	// "preview.example.com". Empty disables previews.
	Domain string `toml:"domain,omitempty"`
	// Listen is the address the plain-HTTP ingress listener binds, e.g.
	// "100.64.0.1:8080". TLS terminates outside kvarn — Caddy, Tailscale, a
	// load balancer — so this should be an address only that fronting layer can
	// reach. Empty disables the listener.
	Listen string `toml:"listen,omitempty"`
	// IdleTimeout stops a preview that has served no request for this long
	// (e.g. "30m"). The next request boots it again. "0" never reaps on idle;
	// empty takes the built-in default.
	IdleTimeout string `toml:"idle_timeout,omitempty"`
	// MaxLifetime stops a preview this long after it booted regardless of
	// traffic (e.g. "8h"), so a preview somebody keeps poking at is still
	// rebuilt from the ref eventually. "0" disables the cap; empty takes the
	// built-in default.
	MaxLifetime string `toml:"max_lifetime,omitempty"`
	// MaxConcurrent bounds how many previews may be running at once. Reaching
	// it evicts the least-recently-requested idle preview to make room, and
	// answers with a holding page only when there is nothing idle to evict. 0
	// is unbounded; empty takes the built-in default.
	MaxConcurrent *int `toml:"max_concurrent,omitempty"`
	// MaxMemory and MaxDisk cap what any one preview VM may request,
	// independently of what its kvarn.yml asks for (e.g. "8G"). A preview is
	// long-lived, so the ceiling that makes sense for a job that finishes in
	// minutes is not the one that makes sense here. Empty means the project's
	// own request stands.
	MaxMemory string `toml:"max_memory,omitempty"`
	MaxDisk   string `toml:"max_disk,omitempty"`
}

// Enabled reports whether the operator has configured previews at all. Both a
// domain and a listen address are needed: a preview with no name is
// unaddressable, and one with no listener is unreachable.
func (p Preview) Enabled() bool {
	return p.Domain != "" && p.Listen != ""
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
	// MaxQueue bounds how many jobs may occupy the in-memory pipeline at once
	// — cloning, waiting for capacity, or running. Each holds a goroutine and
	// a clone already on disk, so an unbounded pipeline quietly consumes the
	// same filesystem the pool is sized from. Jobs beyond it wait in the
	// durable backlog instead. 0 is unbounded; empty takes the default.
	MaxQueue *int `toml:"max_queue,omitempty"`
	// MaxBacklog bounds the durable backlog: jobs accepted and persisted but
	// not yet dispatched into the pipeline. A backlog entry costs a row rather
	// than a clone, so this can sit far above max_queue; reaching it refuses
	// the submission. 0 is unbounded; empty takes the default.
	MaxBacklog *int `toml:"max_backlog,omitempty"`
	// MaxQueueWait fails a backlog entry that has waited this long without
	// being dispatched (e.g. "24h"), so a host that was down for days does not
	// boot into a flood of work nobody is waiting for. "0" never expires;
	// empty takes the default.
	MaxQueueWait string `toml:"max_queue_wait,omitempty"`
	// MaxAttempts caps how many times one job may be dispatched. A run
	// interrupted by a restart returns to the backlog and spends an attempt;
	// past the cap it fails, so a job that kills the orchestrator on every
	// attempt stops killing it. 0 disables the cap; empty takes the default.
	MaxAttempts *int `toml:"max_attempts,omitempty"`
	// MaxConcurrentClones bounds how many jobs may be cloning and reading
	// their kvarn.yml at once — the work that happens *before* admission and
	// is therefore not covered by the pool. 0 is unbounded; empty takes the
	// default.
	MaxConcurrentClones *int `toml:"max_concurrent_clones,omitempty"`
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
