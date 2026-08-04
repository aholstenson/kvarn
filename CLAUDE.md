# kvarn

## Module

`github.com/aholstenson/kvarn`

## Build

These commands need access to the Go build cache and toolchain, so they **will not work inside the sandbox**. Always run them with `dangerouslyDisableSandbox: true`.

```sh
task build          # generate + build (preferred)
task generate       # regenerate proto code only
task test           # generate + run all tests
task --list         # see all available tasks
```

## Host requirements

- **git ≥ 2.26 on PATH.** Every clone, fetch and push shells out to it. The floor is protocol v2 (default since 2.26), which is what keeps the mirror's per-job `ls-remote` one small round trip on a repository with thousands of refs; it also covers `--end-of-options` (2.24). `kvarn orchestrator` checks this at startup and refuses to boot without it, so an operator learns from one clear message rather than from every job failing to clone.

## Structure

- `cmd/kvarn/` — CLI entry point (Kong)
- `proto/kvarn/v1/` — Protobuf definitions
- `gen/` — Generated protobuf + ConnectRPC code (not checked in)
- `internal/vm/` — VM provider interface + implementations (local, disk, transfer)
- `internal/config/` — User-level config stores (credential, project, secret, forge, apikey); `atomicfile` for temp-file+rename writes
- `internal/project/` — Per-repo kvarn.yml parsing and step execution
- `internal/orchestrator/` — Orchestrator service; `auth/` holds the API-key interceptor + identity; `repos.go` holds the mirror-backed clone path and the background mirror maintenance loop
- `internal/scm/git/` — `scm.SCM` on the git command line. `run.go` runs one command and owns the option/operand boundary, `auth.go` builds per-invocation credentials, `hostkey.go` + `hostkeys.txt` pin forge SSH host keys
- `internal/scm/mirror/` — Host-side bare mirror per project, so N concurrent jobs on one repository share a single fetch
- `internal/session/` — Orchestrator-owned session store. `Store` (sessions + per-session monotonic event log) has two impls: `memstore` (tests) and `sqlite/` (production, pure-Go `modernc.org/sqlite`). `Manager` owns the in-memory pub/sub hub and layers replay + reconnect-from-cursor on top; `codec.go` encodes the durable event kinds (256 KiB payload cap) and the session↔row mapping
- `internal/runner/` — Runner service (ConnectRPC handler)
- `internal/runnerbin/` — Embeds the linux runner binary into the CLI (build with `-tags embedrunner`; the artifact is gitignored and produced by `task build:runner`)
- `internal/cmd/` — CLI command handlers (startjob, secrets, key, client, repo)

## Test

```sh
task test             # run all tests
task test:verbose     # verbose output
```

- **All tests must use Ginkgo v2 + Gomega** — never use plain `testing.T` assertions or `testify`. Write `Describe`/`It` blocks with `Expect` matchers.
- Each test package needs a `_suite_test.go` that bootstraps the Ginkgo runner (see existing examples)
- Integration tests exercise real servers on random ports with mock providers

## VM Image

```sh
task image:build         # build disk image for the host arch into dist/<arch>/
task image:build:arm64   # build for arm64
task image:build:amd64   # build for amd64
task image:clean         # remove dist/
```

- `image/build-image.sh` runs `image/customize.sh` in a privileged Docker container. The image is purely the base OS userspace — it does **not** contain the runner.
- `customize.sh` downloads a pinned, checksum-verified Debian trixie genericcloud snapshot (dated directory + recorded sha512), mounts its rootfs, installs podman/nix/tooling plus the `image/overlay/` systemd units and scripts, loads the vsock/virtio/iso9660 modules at boot, and reconverts to a compressed qcow2.
- Output: `dist/<arch>/disk.qcow2` — one base image per arch.
- The runner is **embedded in the CLI/orchestrator** (`internal/runnerbin`) and injected into each VM at boot: it is written as a raw `/kvarn-runner` file onto the cloud-init seed ISO, and `image/overlay/.../kvarn-runner-setup.sh` stages it to `/usr/local/bin/kvarn`. The orchestrator therefore always boots the exact runner it speaks to (no runner↔orchestrator skew); the only remaining contract is the coarser image ABI, handled by semver selection.

### Release flow

- Pushes to `main` drive Release Please (`.github/workflows/release-please.yml`) with two independent components: the root (`vX.Y.Z`, CLI) and `image/` (`image-vX.Y.Z`, VM image). Conventional Commits express compatibility intent (`feat(image)!` = major image bump).
- `.github/workflows/release.yml` builds the `kvarn` CLI binaries per arch (`.tar.gz`/`.zip` + `.sha256`), embedding the cross-compiled `linux/<arch>` runner via `-tags embedrunner`.
- `.github/workflows/image-release.yml` builds the per-arch `kvarn-disk-<arch>.qcow2` + `.sha256` on `image-v*` releases, and regenerates `images.json` (the version/arch manifest) onto a perpetual `image-index` release.
- At runtime, VM commands resolve the image via `vm.EnsureDiskImage`. The version input is `--version`/`opts.Version` → `KVARN_IMAGE_VERSION` → the compiled-in `buildinfo.ImageConstraint` (a semver range). A concrete version resolves by exact path/cache/download; a range is satisfied by a local `dist/` image, then the highest matching cached version, then the highest match from `images.json` (downloaded from `image-v<version>`). Downloads are checksum-verified and cached at `~/.cache/kvarn/images/<version>/<arch>/disk.qcow2`; `kvarn image pull` pre-seeds the cache.
- On Linux the resolved image is the **backing file** of each VM's disk, not a template copied per VM: `disk.CreateOverlayQcow2` gives every VM a qcow2 overlay sized to its disk request, so booting writes ~200 KiB instead of duplicating the image, and concurrent VMs share one copy of the base in the host page cache. The resolved path must therefore stay readable and byte-stable for as long as any VM using it runs. Cached images satisfy this — they live in version-scoped directories written temp-file+rename — but `task image:build` rewrites `dist/<arch>/disk.qcow2` in place, so rebuilding the image while a VM boots from it corrupts that VM.

## Job startup: cloning and transfer

Two costs dominate the start of a job, and each is addressed at a different layer.

### Only the repository crosses the transport

A fresh clone has no untracked files, so shipping the checked-out tree alongside `.git` sends the same content twice — once as packed objects, once as expanded files. `sandbox.Opts.PristineClone` says "the source is a clone whose worktree is exactly HEAD": the upload then admits only `.git` (`transfer.GitDirOnlyFilter`), and `sandbox.CheckoutWorktree` runs `git reset --hard HEAD` in the guest to write the files. That happens immediately after the upload and **before** cache restore, dependency install, image pull and container start, all of which assume a populated workspace; the container bind-mounts the same path and starts later, so it sees the finished tree.

The checkout carries `-c filter.lfs.smudge=cat -c filter.lfs.required=false`. Those are load-bearing: running the real smudge filter would replace pointer files with their contents *and* make the guest authenticate to an LFS endpoint, which is the one thing the sandbox is built to prevent.

The host clone stays a full checkout — `project.Load`, `repocontext.Load` and `cache.DeriveLayers`' lockfile hashing all read the worktree before the VM boots, and `ExtractChanges` needs it as the pre-image. `kvarn run`/`kvarn test` keep full-worktree transfer: their purpose is shipping a developer's dirty tree, which a guest `reset --hard` would destroy.

`transfer.Options` carries `SkipFile` and `OnProgress` per `Upload` call rather than per Transferer, because the orchestrator shares one Transferer across every concurrent job.

### Per-project mirrors

`internal/scm/mirror` keeps one bare repository per project under `~/.cache/kvarn/repos/<project>/`, so N jobs across N branches cost one fetch of the shared history instead of N full clones. Mirrors are **keyed by project name, not repository URL**: two projects can point at one URL with different credentials, and hashing the URL would hand them a shared object store, letting a weaker-scoped project read objects a stronger-scoped one fetched.

Per job, per branch, `Refresh` does as little as the situation allows:

1. If the caller already knows the SHA it needs — a feedback run carries the PR's head — and the mirror has it, stop. **Zero network.**
2. `git ls-remote <url> refs/heads/<branch>`. Under protocol v2 the server filters on a ref-prefix, so this is one small round trip whatever the repo's ref count.
3. If the mirror is already at that SHA, stop.
4. Otherwise fetch just that branch: `+refs/heads/<b>:refs/heads/<b>`.

This beats a time-based freshness window in both directions: correct when the branch moved two seconds ago, free when nothing moved in a week. **Refspecs are narrow** — only branches actually worked on are mirrored, so dead upstream branches are never acquired, and branch #2 onward costs a delta rather than a clone. No remote is configured in the mirror at all; each fetch names its refspec, and `meta.json` is the sole record of what the mirror tracks, so pruning a branch is dropping its ref and its entry.

Job clones come out of the mirror over a local path. With a depth, that path must be `file://`: a plain path makes git take the local-clone shortcut, which hardlinks the object store and **silently ignores `--depth`**. Without a depth the plain path is used deliberately, since hardlinking a full history is far cheaper than a local pack transfer. If a project's `clone_depth` exceeds its `mirror_depth`, the mirror is deepened to match rather than serving a truncated history.

**Locking**: `LOCK_SH` for cloning out of a mirror, `LOCK_EX` for fetch, ref edits and `gc`. `atomicfile.Acquire` polls with `LOCK_NB` instead of blocking in `flock(2)`, so a cancelled job can unwind rather than sit uninterruptibly behind a multi-gigabyte fetch. An in-process `singleflight` keyed by project+branch collapses several jobs starting at once into one fetch. Unlike `filecache.lockProject` this lock is **not** best-effort: a `gc` running unlocked while a job clones can hand it a half-rewritten pack, so failing to take the lock fails the operation.

**A broken cache never fails a job.** A mirror whose git commands fail is discarded and rebuilt once; if that fails too, the job falls back to a direct clone with a warning. The first job on a project otherwise pays for the initial mirror clone, which is what prefetch exists to keep off the critical path.

Mirrors share the filesystem the scheduler sizes its VM disk pool from (75% of free space on the image filesystem), so an uncapped mirror store quietly shrinks what can be admitted — `[repos].global_bytes` caps it with LRU eviction by project last-used.

```toml
[repos]
enabled = true             # false disables mirrors; jobs clone directly
dir = ""                   # default ~/.cache/kvarn/repos
prefetch = true
prefetch_interval = "5m"
mirror_depth = 0           # 0 = full history
branch_retention = "720h"  # "0" = never prune
global_bytes = ""          # optional LRU cap across mirrors
```

Per-project `mirror_depth` overrides the global value in `projects.toml`. It is a separate knob from `clone_depth`: that bounds what each job and its VM see, this bounds what the host caches on their behalf.

The prefetch loop lives on `Service`, not `cmd.go`, because it needs `resolveForge` for credentials and the project store. It follows `startSessionRetention` with one deliberate difference: that runs its first pass synchronously, which here would hold the listener closed for as long as cloning a fleet of projects takes, so the initial warm happens **inside** the goroutine.

`kvarn repo pull|list|gc|clear` manages mirrors. All but `pull` read the on-disk store directly with no running orchestrator, matching the `kvarn image-cache` convention; `pull` reaches the forge and so resolves credentials the way a job would.

## Logging

`logging.Setup` picks the handler — colorized text under `DEVELOPMENT=true`, JSON otherwise — and the level from `KVARN_LOG_LEVEL` (`debug`/`info`/`warn`/`error`), defaulting to debug in development and info elsewhere.

The split between the two levels is what makes an info-level log worth reading:

- **Info** is work the host actually did and time it actually spent: a mirror created, a branch fetched (with the tip it moved from and to), a clone taken out of the mirror or from the forge, a prefetch pass summarized, space reclaimed or evicted, a lock waited on for longer than a second. Everything carries `project`, `branch` and a `duration` where one applies, so "why did this job take so long to start" is answerable from the log alone.
- **Debug** is per-step detail on paths that run constantly and usually cost nothing: which credential kind a git invocation resolved, an `ls-remote` result, a mirror already at the upstream tip, a repack that reclaimed nothing. `ResolveAuth` in particular runs once per git invocation and says nothing about what is being done — at info it would bury the lines that do.

Durations are logged as strings (`logging.Elapsed`) rather than `time.Duration`, which slog would write as a bare nanosecond count.

**Repository URLs are logged through `git.RedactURL`**, which replaces the userinfo of a credentialed URL. A project may be configured as `https://user:token@host/repo.git`, and a log file is as durable a place for a token to come to rest as the `.git/config` that `SanitizeClone` exists to strip. The same applies to the two places a mirror writes a URL to disk — the `SOURCE` marker and `meta.json` — both of which exist to be read by a human and never to be fetched from. scp-style `git@host:path` keeps its user: ssh authenticates with a key, so the user is part of the address rather than a secret.

## Comments and documentation

- Comments should explain **why** something is done, not investigation history or migration details
- Don't reference previous implementations (e.g. "this used to be X") — only explain the current design

## Authentication

- External `OrchestratorService` calls authenticate with an API key sent as `Authorization: Bearer <token>`. Validated by a ConnectRPC interceptor (`internal/orchestrator/auth`); each key is scoped to a set of projects and every project-scoped RPC checks that scope. The host-local `BridgeService` (runner↔orchestrator) is intentionally **not** authenticated.
- Token format: `kvarn_<keyid>_<secret>`. The `kvarn_` prefix lets secret scanners recognize leaks; `keyid` is the O(1) lookup handle, `secret` is 160 bits of CSPRNG. Both components are base32 (lowercased, unpadded) so they never contain the `_` delimiter. Only `sha256(secret)` is persisted (plain SHA-256 is correct for high-entropy random secrets).
- Auth is **enforced by default**; `--no-auth` (or `KVARN_NO_AUTH`) disables it for local dev. With auth on and zero keys, all requests are denied. Keys are bootstrapped with `kvarn key create`, which writes `apikeys.toml` directly — no running orchestrator needed.
- **TLS is out of scope**: the orchestrator stays on h2c and assumes an external TLS-terminating reverse proxy. A bearer token is only safe over TLS.
- **The VM never sees git credentials**, and that is enforced structurally rather than by sanitizing. Three points carry it:
  - `git.SanitizeClone` runs immediately after every clone, removing the remote *and* the reflog. Both record where the clone came from, so a project configured as `https://user:token@host/repo.git` would otherwise embed that token in `.git/config` and in the reflog's `clone: from <url>` entry — and `.git` is shipped into the VM. With no remote there is no field a credential can hide in, which is stronger than scrubbing one. Nothing downstream wants it: the VM never fetches or pushes, changes return via `ExtractChanges`, and the host-side push names its target through `CommitAndPushOpts.RemoteURL`.
  - Credentials reach git only through per-invocation `-c` config and process environment, never a config file. The inline `credential.helper` names two environment variables rather than carrying the secret, so a token never appears in argv where `ps` would expose it; a leading empty `credential.helper=` resets the chain so a host-configured helper can neither supply credentials kvarn did not choose nor capture the ones it did.
  - With mirrors on, a job clone's source is a local mirror path, so no credentialed URL exists anywhere near the VM.
- **Argument injection**: every caller-supplied value — branch names, refs, SHAs, URLs — sits after `--end-of-options`. `git.Cmd` splits flags from operands and `git.Run` inserts the separator, so a new call site cannot forget it. This is not cosmetic: branch names arrive from project config and, on a feedback run, from whoever opened the PR, and a branch named `--upload-pack=...` reaching `clone` or `ls-remote` as a bare argument is remote code execution on the orchestrator host.
- **SSH host keys** are pinned as full public keys in `hostkeys.txt`, written into a generated `known_hosts` alongside the operator's own. `pinnedHostFingerprints` remains the human-verifiable authority and a test asserts every embedded key hashes to it, so a wrong or tampered key blob cannot pass review. Rotate both together. Passphrase-protected keys are decrypted in process and re-written unencrypted into a 0700 per-invocation directory, which avoids `SSH_ASKPASS` entirely.
- **Forge credentials** (a separate axis from API keys) are resolved into an `scm.CredentialSource`, never a fixed token. A GitHub App installation token expires after an hour while a job can run longer, so `CloneOpts`/`CommitAndPushOpts` and the `forge.*Opts` all carry the source and each operation resolves it as it authenticates — the push at the end of a job mints a token then, not at job start. The GitHub source caches until 5 minutes before expiry, so re-reads inside one token's lifetime cost nothing. `ResolveCredentials` still mints once eagerly so a misconfigured App is reported where it is configured rather than at push time.
- **Hot-reload**: every tomlstore re-reads its file per `Get`/`List`, so key changes apply on the next request with no restart. All stores write atomically (`internal/config/atomicfile`, temp file + rename) so a concurrent `kvarn key create` is never read mid-write. Writers also hold a `flock(2)` on `<file>.lock` around the load → mutate → save sequence (`atomicfile.WithLock`) so two CLI invocations (or a CLI racing the orchestrator) can't lose each other's edits; readers don't need the lock.

## Sessions

- Sessions are **persistent and orchestrator-owned** (the CLI never writes them), backed by SQLite at `~/.config/kvarn/sessions.db` (`--sessions-db` to override). Single-process access uses WAL + `busy_timeout` + `SetMaxOpenConns(1)`; no cross-process `flock` is needed.
- Each session carries a **monotonic event log** (per-session `seq` starting at 1). Clients **watch** live (`WatchSession`, resumable via `from_sequence`) or **poll** history (`ListSessionEvents`, paged via `after_sequence`). `SessionUpdate.sequence` is the durable seq (0 = ephemeral/live-only).
- **Durable kinds** persisted to history: `state_change`, `agent_message`, `agent_tool_use`, `agent_tool_result`, `step_result`, `cost`, `pull_request`, `vm_info`. High-volume telemetry (VM console, step stdout/stderr chunks, transfer/cache/dependency progress) is broadcast live-only. Each persisted payload is truncated at 256 KiB; live watchers still get the full stream.
- **Slow watchers** are disconnected on lag rather than silently dropping a durable event (which would create an undetectable gap). The client reconnects via `Watch(from_sequence=lastSeen)` and replays the gap from the store, the source of truth.
- **Terminal writes are uncancellable**: a job's outcome (`failed`/`completed`, the final cost snapshot, a created PR's ref) is written on a `context.WithoutCancel` copy of the job context. A cost-cap trip or a shutdown cancels the job context, and a terminal write made on it would never reach SQLite — leaving the session non-terminal until the next boot's reconciliation.
- **Startup reconciliation**: non-terminal sessions are flipped to `failed` on boot (their VMs are gone), appending a `state_change` event. **Retention**: terminal sessions older than `[sessions].retention` (default 720h; `0` = keep forever) are pruned on startup and hourly; events cascade.
- **Terminal states** are `completed`, `failed` and `cancelled`. `session.TerminalStates()` is the single source of truth — the SQLite `state IN (…)` predicates behind active-only listing, reconciliation and retention build their placeholders from it, so a state added there cannot be missed in one of them.

### Cancellation

- `CancelJob(session_id, reason)` stops an in-flight run (CLI: `kvarn cancel <session-id>`). It cancels the job's context and returns; the run unwinds on its own and its deferred `Sandbox.Close` tears the VM down, so the caller is not made to wait for a VM to stop.
- The service keeps a `session ID → context.CancelCauseFunc` map. The entry is registered by `beginJob` **before** the job goroutine is spawned — creating the context inside `runJob` would leave a window where a caller holding a session ID could not yet cancel it — and removed after the run has written its terminal state, so a cancel racing the end of a job either finds the job or sees the terminal state.
- The **cancel cause** is what tells the outcomes apart. Every failure path in `runJob` goes through `failRun`, which records `cancelled` when the cause is `errJobCancelled` and `failed` otherwise. A shutdown (plain `context.Canceled`) and a tripped cost cap (`cost.ErrBudgetExceeded`) therefore stay failures, and the cancellation is reported the same way wherever it lands — the scheduler queue, a clone, the agent, validation. A cancel that arrives after submission succeeded loses the race and the run is reported `completed`: the pull request exists.
- A cancelled session is **not an error**: the reason lands in `message` and `error` stays empty.

### Feedback runs

- `SubmitFeedback(project, pr_ref, feedback)` continues work on an **existing** pull request: it clones that PR's head branch, runs the agent with the feedback as its task, and pushes a follow-up commit to the same branch, then posts a comment. No second PR is opened and the PR title/body are left alone. `StartJob` keeps its "clone base, open a new PR" meaning; the two never share an entry point. CLI: `kvarn feedback <project> <pr-ref> <text>`.
- A feedback run is a **new session** (terminal states stay terminal). Lineage is recorded via `parent_session_id`, resolved by looking up the oldest session on the same `pr_ref`; nothing depends on that session still existing, since retention may have pruned it.
- **`pr_ref` is opaque to kvarn** — each forge interprets its own format, and the GitHub forge is the only place that knows a ref is a number. Sessions persist `pr_ref`, `head_branch`, `base_branch` and `parent_session_id`; the `SessionFilter.PRRef` filter serves both the single-flight check and the parent lookup.
- **One run per PR at a time**: a `SubmitFeedback` while another is in flight for the same PR is rejected with `FailedPrecondition` naming the running session. Startup reconciliation fails non-terminal sessions, so a crash cannot leave the lock stuck.
- **Fork PRs are rejected** (`InvalidArgument`): pushing to a head branch in another repo needs the maintainer-edit flag and is impossible with an org-scoped App installation token. The real constraint on which forges this can serve is that the PR must have a mutable head branch to push to — which excludes Gerrit and mail-based flows regardless of how they name things.
- Every rejection (no forge, PR not open, fork, in-flight, unreadable ref) happens **before a session is created**, so a refused request leaves no trace. Before pushing, the PR head is re-read and the run fails rather than pushing if it moved underneath.
- The `feedback` mode's task message is a context pack: `## Original task` (omitted when the parent was pruned), `## Current pull request`, `## Diff` (best-effort, capped at 256 KiB), `## Feedback to address`. The session's stored `Prompt` stays the raw feedback so `GetSession` shows what was actually asked. Cost caps and validation retries come from `[jobs.feedback]` like any other mode.

## Conventions

- **RPC**: ConnectRPC with buf v2 for code generation
- **CLI**: Kong for argument parsing and subcommand dispatch
- **VM Providers**: Implement the `vm.Provider` interface
- **Generated code**: Always regenerate with `buf generate`, never edit `gen/` directly
- **Build tags**: `//go:build darwin` for macOS-specific code (e.g., vz provider). Non-darwin stubs return `errors.ErrUnsupported`.
