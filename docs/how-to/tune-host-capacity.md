# Tune host capacity

The orchestrator admits jobs against a fixed pool of vCPUs, memory and disk.
Each job is charged the `vm.cpus`, `vm.memory` and `vm.disk` from its
repository's `kvarn.yml` — 2 vCPU / 4G / 16G by default — and a job that does
not fit waits in the queue.

Everything here lives in
[`orchestrator.toml`](../reference/orchestrator-toml.md), which is read **once
at startup**: changes need a restart. Each field also has a flag and a
`KVARN_SCHED_*` environment variable that take precedence over the file.

## Size the pool

Out of the box the pool is the host's CPU count, 75% of its memory, and 75% of
the free space on the image filesystem. Set it explicitly when the host is
shared with something else:

```toml
[scheduler]
cpus = 24
memory = "64G"
disk = "400G"
```

Memory is strict. CPU and disk are overcommitted, by `1.5×` and `3.0×`
respectively:

```toml
[scheduler]
cpu_overcommit = 1.5
disk_overcommit = 3.0
disk_floor = "40G"
```

Disk overcommit is not a guess. A job is charged its VM's *virtual* disk size
while the image on the host stays thin — a qcow2 overlay on Linux, a sparse file
on macOS — so charging the full request would idle most of the pool. What makes
it safe is `disk_floor`: real free space is measured continuously, and admission
stops entirely while it is below the floor, whatever the accounting says. It
defaults to 10% of the pool. Lower `disk_overcommit` to `1.0` if you would
rather account exactly.

## Bound the queue, the backlog and the pre-admission work

```toml
[scheduler]
max_queue = 64
max_backlog = 1000
max_queue_wait = "24h"
max_concurrent_clones = 4
```

Waiting happens in two places, and they cost very different amounts.

A job **in the pipeline** is not free: it holds a goroutine and a clone already
written to disk — the same filesystem the pool is sized from. `max_queue` bounds
how many jobs may be there at once, counting those cloning, those waiting for
capacity and those running.

A job **in the backlog** is a row in the sessions database. It has been accepted
and persisted but nothing has been resolved for it yet, so `max_backlog` can sit
orders of magnitude higher; reaching it means a submitter has run away rather
than that the host is busy, and the submission is refused with "job backlog is
full; retry when the host has caught up".

`max_queue_wait` fails a backlog entry that has waited that long undispatched.
Its job is the day-after case: a host that was down over a weekend should not
boot into a flood of work whose requesters gave up long ago.

`max_concurrent_clones` bounds the cloning and `kvarn.yml` reading that happens
*before* admission and so isn't covered by the pool at all. Raise it if jobs sit
idle waiting to clone; lower it if clone bursts saturate the disk.

In this file, `0` means unbounded for each count. On the command line, `0` means
"unset" and `-1` means unbounded.

## Drain before a planned restart

Everything below is about a restart the host did not choose. For one you *did* —
a deploy, host maintenance — drain it first and let the running jobs finish, so
none of the reconciliation here has to happen at all. See
[Take a host out of service](take-a-host-out-of-service.md).

## Survive a restart

Queued work is durable. A submission is written to the sessions database before
its RPC returns, so a job accepted a moment before the orchestrator stops still
runs when it comes back.

Runs that were already in flight are sorted by how far they had got:

- Cloning, waiting for capacity, booting a VM, pulling an image, running setup —
  all of it is work that can simply be done again. These return to the backlog
  and run from the start.
- Running the agent, or validating — these have spent against the job's cost cap
  and hold work that only existed inside a VM that is now gone. They fail.
- Submitting — a push may already have landed. These fail rather than risk
  opening a second pull request for the same work.

```toml
[scheduler]
max_attempts = 3
```

Each requeue spends an attempt, and past `max_attempts` the job fails instead of
returning to the backlog — so a job that takes the orchestrator down with it
stops doing so. Spend already recorded on a session carries into its retry, so a
requeued job resumes against what is left of its budget rather than a fresh one.

There is no resuming a VM: it dies with the orchestrator on both platforms, and
its temp files are swept at the next startup.

## Cap what one tenant may hold

On a host serving several projects or clients, cap each so one cannot take
everything:

```toml
[scheduler.per_project]
max_jobs = 4
max_cpu = 16
max_memory = "48G"

[scheduler.per_key]
max_jobs = 2
```

Unset fields don't cap that dimension — the right default for a single-tenant
host. Individual projects
([`projects.toml`](../reference/projects-toml.md#concurrency-caps)) and API keys
([`apikeys.toml`](../reference/apikeys-toml.md)) override these per field; an
explicit `0` there means unlimited even when a default is set here.

A job over its tenant's cap waits without blocking other tenants. A job that
could *never* fit — one whose request exceeds a configured limit, or the pool
itself — fails immediately and names the limit, rather than queueing forever
behind a misconfiguration.

## Decide who goes first

The queue is ordered by, in this order: effective priority (highest first), then
dominant resource share (a project holding little of the host goes before one
holding a lot), then arrival time.

The backlog in front of it is ordered by the same effective priority and the
same arrival tie-break, so a job's place is decided by one rule on both sides of
dispatch. What it cannot weigh is resource share, since a backlog entry has no
footprint until it is cloned; instead each project waiting for the pipeline gets
an equal share of it, so one project's burst cannot lock another out of starting
at all.

Give a project a priority in `projects.toml`:

```toml
[projects.release-tooling]
repo = "owner/release-tooling"
priority = 10

[projects.release-tooling.jobs.review]
priority = 0
```

Priority orders the queue; it never reserves capacity. Two knobs shape how
strictly it is applied:

```toml
[scheduler]
priority_age_step = "5m"
backfill_grace = "1m"
```

`priority_age_step` is how long a waiting job takes to gain one level of
effective priority, so a low-priority project can't be starved by a stream of
high-priority ones. `"0"` disables aging and lets priority strictly dominate.

`backfill_grace` is how long a queued job that doesn't fit may be skipped by
smaller jobs behind it before it holds the line and nothing is admitted ahead of
it. `"0"` is strict FIFO — a job that doesn't fit stalls everything behind it,
which wastes capacity but is easy to reason about.

On a single-project host, every waiter ties on priority and share, and the order
falls back to arrival.

## Bound how long a VM may live

```toml
[scheduler]
max_vm_lifetime = "4h"
```

A host-wide failsafe on per-VM wall time, defaulting to 24h. It is not a
substitute for a cost cap — see [Control job costs](control-job-costs.md) — but
it catches a VM that is stuck rather than spending.

## Watch what's happening

`kvarn queue status` answers the question the settings above are tuned against —
whether jobs are waiting because the pipeline is at `max_queue` or because the
resource pool is exhausted — and `kvarn queue list` shows the backlog in the
order it will be dispatched, with the effective priority each entry is ordered
by. `kvarn jobs priority <session-id> <n>` promotes one entry that should not
wait its turn; see [CLI](../reference/cli.md#kvarn-queue).

Queued sessions report their position and what they are waiting for, including
when admission is paused because host disk is below the reserve. Admission waits
longer than a second are logged with a duration, and metrics for admission
waits and denials (`too_large`, `exceeds_limit`, `queue_full`, `backlog_full`)
are exported when `--otel-metrics-enabled` is set.

`kvarn.scheduler.queue_depth` and `kvarn.scheduler.backlog_depth` are worth
reading together: a full pipeline with an empty backlog is a host working at
capacity, while a backlog that keeps growing is a host that is not keeping up
at all.

Remember that mirrors and caches share the filesystem the disk pool is sized
from. Cap them too, or they quietly shrink what can be admitted — see
[Speed up job startup](speed-up-job-startup.md).
