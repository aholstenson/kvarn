# Speed up job startup

Between "job accepted" and "the agent starts working" sit a clone, a VM boot, a
dependency install and an image pull. Kvarn caches each of those on the host.
All of them are on by default; this guide is about tuning them, and about
keeping them from eating the disk the scheduler is sized from.

## Repository mirrors

Kvarn keeps one bare mirror per project under `~/.cache/kvarn/repos/<project>/`,
so ten jobs across ten branches cost one fetch of the shared history rather than
ten full clones. Per job it does as little as the situation allows: if it
already knows the SHA it needs and has it, no network at all; otherwise one
`ls-remote` for that branch, and a fetch only if the branch actually moved.

```toml
[repos]
enabled = true
prefetch = true
prefetch_interval = "5m"
mirror_depth = 0
branch_retention = "720h"
global_bytes = "100G"
```

Things worth changing:

- **`global_bytes`** — set it. Mirrors live on the same filesystem the scheduler
  sizes its disk pool from, so an uncapped mirror store quietly shrinks what can
  be admitted. With a cap, least-recently-used projects are evicted first.
- **`prefetch_interval`** — the background warm keeps the first job on a project
  off the critical path. Shorten it for projects that move constantly; lengthen
  it on a host with many projects and slow upstreams.
- **`branch_retention`** — how long an unused branch ref is kept. `"0"` never
  prunes.
- **`mirror_depth`** — bound the history the host caches. `0` is full history.
  Per-project overrides go in `projects.toml`.

Manage the store by hand with:

```sh
kvarn repo list
kvarn repo pull my-project --branch main
kvarn repo gc --older-than 720h
kvarn repo clear my-project
```

A mirror whose git commands fail is discarded and rebuilt once; if that fails
too, the job clones directly with a warning. A broken cache never fails a job.

## Clone depth

Each job clones 100 commits of history by default. Override it per project:

```toml
[projects.my-project]
repo = "owner/repo"
clone_depth = 0     # full history — needed for version inference from tags
mirror_depth = 500
```

`clone_depth` bounds what the job and its VM see; `mirror_depth` bounds what the
host caches. If `clone_depth` exceeds `mirror_depth`, the mirror is deepened
rather than serving truncated history — so raising one usually means thinking
about the other.

## Tool caches

Language package managers and build caches are captured after a run and restored
into the next VM for the same project. [Registered
tools](../reference/registered-tools.md) need no configuration — declaring the
nixpkgs attribute is enough. Unregistered ones are added with a `cache:` block in
`kvarn.yml` — see
[Configure a repository](configure-a-repository.md#cache-what-the-build-downloads).

Quotas, swept LRU:

```toml
[cache]
per_project_bytes = "10G"
global_bytes = "100G"
```

```sh
kvarn cache list
kvarn cache evict --per-project 10G --global 100G
kvarn cache clear my-project
```

## OCI image cache

Container images are pulled through a host-side registry cache rather than from
the upstream registry in every VM. The mirror is written into the guest's
container configuration at boot, so it covers every pull a job makes — anything
a step pulls through `docker`/`podman` or a compose file:

```toml
[image-cache]
enabled = true
upstreams = ["docker.io", "ghcr.io", "quay.io", "gcr.io"]
manifest_tag_ttl = "5m"
global_bytes = "50G"
```

`manifest_tag_ttl` is how long a tag → digest resolution is trusted before
re-checking upstream; digests themselves are immutable and cached indefinitely.
Shorten it if you deploy against fast-moving tags, lengthen it to cut registry
round trips.

```sh
kvarn image-cache stats
kvarn image-cache evict --global 50G
kvarn image-cache clear --repo library/python
```

## VM disk image

The VM disk image is resolved once and shared. Pre-seed it so the first job
after an upgrade doesn't wait on a download:

```sh
kvarn image pull
kvarn image path --no-download   # what would be used, without fetching
```

On Linux, the resolved image is the **backing file** of every VM's disk, not a
copy: booting writes ~200 KiB instead of duplicating gigabytes, and concurrent
VMs share one copy in the host page cache. That means the resolved path must
stay byte-stable while any VM is using it. Cached images satisfy this; a local
`task image:build`, which rewrites `dist/<arch>/disk.qcow2` in place, does not —
don't rebuild the image while jobs are running against it.

## What the log tells you

At info level the orchestrator logs the work it actually did and the time it
spent: a mirror created, a branch fetched with the tips it moved between, a
clone taken from the mirror or the forge, a prefetch pass summarized, space
reclaimed, a lock waited on for more than a second. Each line carries the
project, the branch and a duration, so "why was this job slow to start" is
answerable from the log alone. Set `KVARN_LOG_LEVEL=debug` for the per-step
detail behind it.
