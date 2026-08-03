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

## Structure

- `cmd/kvarn/` — CLI entry point (Kong)
- `proto/kvarn/v1/` — Protobuf definitions
- `gen/` — Generated protobuf + ConnectRPC code (not checked in)
- `internal/vm/` — VM provider interface + implementations (local, disk, transfer)
- `internal/config/` — User-level config stores (credential, project, secret, forge, apikey); `atomicfile` for temp-file+rename writes
- `internal/jobconfig/` — Per-repo kvarn.yml parsing and step execution
- `internal/orchestrator/` — Orchestrator service; `auth/` holds the API-key interceptor + identity
- `internal/session/` — Orchestrator-owned session store. `Store` (sessions + per-session monotonic event log) has two impls: `memstore` (tests) and `sqlite/` (production, pure-Go `modernc.org/sqlite`). `Manager` owns the in-memory pub/sub hub and layers replay + reconnect-from-cursor on top; `codec.go` encodes the durable event kinds (256 KiB payload cap) and the session↔row mapping
- `internal/runner/` — Runner service (ConnectRPC handler)
- `internal/runnerbin/` — Embeds the linux runner binary into the CLI (build with `-tags embedrunner`; the artifact is gitignored and produced by `task build:runner`)
- `internal/cmd/` — CLI command handlers (startjob, secrets, key, client)

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

## Comments and documentation

- Comments should explain **why** something is done, not investigation history or migration details
- Don't reference previous implementations (e.g. "this used to be X") — only explain the current design

## Authentication

- External `OrchestratorService` calls authenticate with an API key sent as `Authorization: Bearer <token>`. Validated by a ConnectRPC interceptor (`internal/orchestrator/auth`); each key is scoped to a set of projects and every project-scoped RPC checks that scope. The host-local `BridgeService` (runner↔orchestrator) is intentionally **not** authenticated.
- Token format: `kvarn_<keyid>_<secret>`. The `kvarn_` prefix lets secret scanners recognize leaks; `keyid` is the O(1) lookup handle, `secret` is 160 bits of CSPRNG. Both components are base32 (lowercased, unpadded) so they never contain the `_` delimiter. Only `sha256(secret)` is persisted (plain SHA-256 is correct for high-entropy random secrets).
- Auth is **enforced by default**; `--no-auth` (or `KVARN_NO_AUTH`) disables it for local dev. With auth on and zero keys, all requests are denied. Keys are bootstrapped with `kvarn key create`, which writes `apikeys.toml` directly — no running orchestrator needed.
- **TLS is out of scope**: the orchestrator stays on h2c and assumes an external TLS-terminating reverse proxy. A bearer token is only safe over TLS.
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
