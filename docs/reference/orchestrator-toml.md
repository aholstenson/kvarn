# `orchestrator.toml`

Host-level settings for the orchestrator: how much of the machine jobs may use,
how big the caches may grow, and how long history is kept. Unlike the other
config files this one is **read once at startup**; changing it needs a restart.

Default location `~/.config/kvarn/orchestrator.toml`, overridable with
`--orchestrator-file`. A missing file is equivalent to an empty one — every
setting has a built-in default.

```toml
[scheduler]
cpus = 32
memory = "96G"
disk = "400G"

[scheduler.per_project]
max_jobs = 4

[cache]
global_bytes = "150G"

[repos]
prefetch_interval = "10m"

[sessions]
retention = "1440h"
```

## `[scheduler]`

The admission pool. Every job is charged the `vm.cpus`, `vm.memory` and
`vm.disk` from its repository's [`kvarn.yml`](kvarn-yml.md); a job that does not
fit waits in the queue.

| Key | Type | Default | Notes |
| --- | --- | --- | --- |
| `cpus` | int | host CPU count | Total vCPUs in the pool, before overcommit. |
| `memory` | size | 75% of host memory | Total pool memory. Strict — never overcommitted. |
| `disk` | size | 75% of free space on the image filesystem | Total pool disk, before overcommit. |
| `cpu_overcommit` | float | `1.5` | Multiplier on `cpus`. Must be ≥ 1.0. |
| `disk_overcommit` | float | `3.0` | Multiplier on `disk`. Must be ≥ 1.0. Set to `1.0` to charge each job its full request. |
| `disk_floor` | size | 10% of the pool | Real free space the VM disk filesystem must keep. Admission pauses below it. `"0"` disables the guard. |
| `max_vm_lifetime` | duration | `24h` | Host-wide failsafe on per-VM wall time. |
| `backfill_grace` | duration | `1m` | How long a queued job may be skipped by later jobs that fit before it holds the line. `"0"` is strict FIFO. |
| `priority_age_step` | duration | `5m` | How long a queued job waits to gain one level of effective priority. `"0"` disables aging, letting priority strictly dominate. |
| `max_queue` | int | `64` | Jobs that may occupy the in-memory pipeline at once — cloning, waiting for capacity, or running. Work beyond it waits in the backlog. `0` is unbounded. |
| `max_backlog` | int | `1000` | Jobs the durable backlog will accept. Submissions beyond it are refused. `0` is unbounded. |
| `max_queue_wait` | duration | `24h` | Fail a backlog entry that has waited this long without being dispatched. `"0"` never expires. |
| `max_attempts` | int | `3` | Dispatches one job may have. A run interrupted by a restart returns to the backlog and spends one. `0` disables the cap. |
| `max_concurrent_clones` | int | `4` | Jobs that may clone and read their `kvarn.yml` at once — work that happens *before* admission. `0` is unbounded. |

Disk is overcommitted because a job is charged its VM's *virtual* disk size
while the image on the host stays thin — a qcow2 overlay on Linux, a sparse file
on macOS. `disk_floor` is what makes that safe: real free space is measured
continuously, and admission stops entirely while it is under the floor.

Sizes accept `M`, `MiB`, `G`, `GiB`. Durations use Go syntax (`30s`, `10m`,
`4h`).

### Flags and environment

Each scheduler field also has a flag and an environment variable, which take
precedence over this file:

| Flag | Environment variable |
| --- | --- |
| `--sched-cpus` | `KVARN_SCHED_CPUS` |
| `--sched-memory` | `KVARN_SCHED_MEMORY` |
| `--sched-disk` | `KVARN_SCHED_DISK` |
| `--sched-cpu-overcommit` | `KVARN_SCHED_CPU_OVERCOMMIT` |
| `--sched-disk-overcommit` | `KVARN_SCHED_DISK_OVERCOMMIT` |
| `--sched-disk-floor` | `KVARN_SCHED_DISK_FLOOR` |
| `--sched-backfill-grace` | `KVARN_SCHED_BACKFILL_GRACE` |
| `--sched-priority-age` | `KVARN_SCHED_PRIORITY_AGE_STEP` |
| `--sched-max-queue` | `KVARN_SCHED_MAX_QUEUE` |
| `--sched-max-backlog` | `KVARN_SCHED_MAX_BACKLOG` |
| `--sched-max-queue-wait` | `KVARN_SCHED_MAX_QUEUE_WAIT` |
| `--sched-max-attempts` | `KVARN_SCHED_MAX_ATTEMPTS` |
| `--sched-max-clones` | `KVARN_SCHED_MAX_CLONES` |
| `--sched-max-vm-lifetime` | `KVARN_SCHED_MAX_VM_LIFETIME` |

For the numeric flags, `0` (or empty) means "not set — fall through to the file,
then the default". To express *unbounded* on the command line for
`--sched-max-queue`, `--sched-max-backlog`, `--sched-max-attempts` and
`--sched-max-clones`, pass `-1`; in this file the same thing is written as `0`.

## Per-tenant caps

`[scheduler.per_project]` and `[scheduler.per_key]` cap what any one project or
API key may hold at once, summed across its running jobs.

```toml
[scheduler.per_project]
max_jobs = 4
max_cpu = 16
max_memory = "48G"
max_disk = "200G"

[scheduler.per_key]
max_jobs = 2
```

| Key | Type |
| --- | --- |
| `max_jobs` | int |
| `max_cpu` | int (whole vCPUs) |
| `max_memory` | size |
| `max_disk` | size |

Every field is optional, and an unset field does not cap that dimension at all —
the right default for a single-tenant host. These are the values a project
([`projects.toml`](projects-toml.md#concurrency-caps)) or key
([`apikeys.toml`](apikeys-toml.md)) inherits when it sets none of its own; an
explicit `0` there means unlimited even when a default exists here.

A job over its tenant's cap waits without blocking other tenants.

## `[cache]`

Disk quotas for the per-project tool caches restored into each VM.

| Key | Type | Default |
| --- | --- | --- |
| `per_project_bytes` | size | `10G` |
| `global_bytes` | size | `100G` |

Least-recently-used layers are evicted to stay under both. Inspect the store
with `kvarn cache list`.

## `[image-cache]`

The pull-through OCI registry cache. It is reachable on the per-VM gateway and
registered as a mirror in the guest's container configuration at boot, so every
image a job pulls goes through it.

| Key | Type | Default |
| --- | --- | --- |
| `enabled` | bool | `true` |
| `listen_addr` | `host:port` | `10.0.2.1:5000` |
| `upstreams` | list of strings | `["docker.io", "ghcr.io", "quay.io", "gcr.io"]` |
| `manifest_tag_ttl` | duration | `5m` |
| `global_bytes` | size | `50G` |

`manifest_tag_ttl` is how long a tag → digest resolution is trusted before it is
re-checked upstream; digests themselves are immutable and cached indefinitely.
Inspect the store with `kvarn image-cache stats`.

## `[repos]`

Host-side bare mirrors, one per project, so concurrent jobs on one repository
share a single fetch.

| Key | Type | Default | Notes |
| --- | --- | --- | --- |
| `enabled` | bool | `true` | `false` makes jobs clone directly from the forge. |
| `dir` | path | `~/.cache/kvarn/repos` | Mirror root. |
| `prefetch` | bool | `true` | Warm mirrors in the background so the first job on a project does not pay for the initial clone. |
| `prefetch_interval` | duration | `5m` | How often the background warm runs. Must be positive. |
| `mirror_depth` | int | `0` | History mirrors keep. `0` is full history. Overridable per project. |
| `branch_retention` | duration | `720h` | How long an unused branch ref is kept. `"0"` never prunes. |
| `global_bytes` | size | unset | LRU cap across all mirrors, evicting by project last-used. Empty means no cap. |

Mirrors share the filesystem the scheduler sizes its disk pool from, so an
uncapped mirror store quietly shrinks what can be admitted — `global_bytes` is
what bounds that. Manage the store with `kvarn repo`.

## `[sessions]`

| Key | Type | Default | Notes |
| --- | --- | --- | --- |
| `retention` | duration | `720h` | How long terminal sessions are kept. `"0"` keeps them forever. |

Pruning runs at startup and hourly; events cascade with their session. The
database path itself is set with `--sessions-db` (default
`~/.config/kvarn/sessions.db`), not in this file.
