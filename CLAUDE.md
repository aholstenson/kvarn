# kvarn

## Module

`github.com/aholstenson/kvarn`

## Build

These commands need access to the Go build cache and toolchain, so they **will not work inside the sandbox**. Always run them with `dangerouslyDisableSandbox: true`.

```sh
task build          # generate + build for the host (preferred)
task build:linux:amd64   # cross-compile the linux CLI into dist/linux_<arch>/
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
- `internal/config/` — User-level config stores (credential, project, secret, forge, apikey); `atomicfile` for temp-file+rename writes. `credential` serves two blocks of one file: the named forge credentials and the `[llm]` provider API keys
- `internal/project/` — Per-repo kvarn.yml parsing and step execution. `project.DefaultNixpkgsChannel` is the channel a bare `nixpkgs:` dependency tracks and `project.DefaultNixpkgsRev` the commit used when that channel cannot be resolved; an upgrade moves both together, and `nixpkgs_test.go` fails if `docs/reference/kvarn-yml.md` still names the old channel. A `ResolvedDep` carries `Channel` alongside `FlakeURI` because the two answer different questions — the URI is what gets fetched and moves with the channel, `StableURI()` is what cache keys are derived from and does not
- `internal/orchestrator/` — Orchestrator service; `auth/` holds the identity plus both interceptors that produce one (API key, host-local socket); `repos.go` holds the mirror-backed clone path and the background mirror maintenance loop; `dispatcher.go` drains the durable backlog into the in-memory job pipeline; `drain.go` is the admission stance that stands that dispatcher down for a controlled shutdown; `previewauto.go` is the only path where an unauthenticated request creates something, so it holds the route table, the single-flight, the negative cache and the rate limit in front of the forge lookup
- `internal/nixpkgs/` — Resolves a nixpkgs channel name to a commit on the host, cached per channel, so a dependency install starts from a commit instead of a branch Nix would resolve over the network from inside the VM. It never fails: an unresolvable channel falls back to the last commit it saw, then to `project.DefaultNixpkgsRev`, then to leaving the branch name alone
- `internal/llmauth/` — Adapts the stored `[llm]` credentials to the `llms.CredentialSource` llms-go resolves model auth through, falling back to the provider env vars
- `internal/localsock/` — The host-local control socket: where it lives, how it is created, and how a client dials it. Shared by both ends so the path cannot drift
- `internal/scm/git/` — `scm.SCM` on the git command line. `run.go` runs one command and owns the option/operand boundary, `auth.go` builds per-invocation credentials, `hostkey.go` + `hostkeys.txt` pin forge SSH host keys
- `internal/scm/mirror/` — Host-side bare mirror per project, so N concurrent jobs on one repository share a single fetch
- `internal/session/` — Orchestrator-owned session store. `Store` (sessions + per-session monotonic event log) has two impls: `memstore` (tests) and `sqlite/` (production, pure-Go `modernc.org/sqlite`). `Manager` owns the in-memory pub/sub hub and layers replay + reconnect-from-cursor on top; `codec.go` encodes the durable event kinds (256 KiB payload cap) and the session↔row mapping
- `internal/preview/` — Preview environments: the durable record and its `Store` (`memstore` for tests, `sqlite/` in production), plus `boot.go`, which starts the declared serve processes and waits on the ready checks. Both the orchestrator's hosted previews and `kvarn local preview` go through `boot.go`, so the two cannot drift on what bringing a preview up means. `route.go` is the reverse of hostname resolution — matching a `pr-{pr}.{domain}` pattern back to a pull request — which is what lets a request for a name nothing has claimed start a preview
- `internal/runner/` — Runner service (ConnectRPC handler); `proxyconn.go` dials a port from inside the guest and carries the connection back over the bridge, which is how preview traffic reaches a server bound to the guest's own loopback. `connect.go` runs each bridge command on its own goroutine, capped by `maxConcurrentCommands`, so a long exec does not hold up the file reads and searches issued alongside it; per-session shells still serialize on their own mutex. A command that runs past its timeout is killed with its whole process group and comes back as a result carrying `timed_out` and exit code 124 — never as a failed call, because that would discard the output the timeout is diagnosed from
- `internal/runnerbin/` — Embeds the linux runner binary into the CLI (build with `-tags embedrunner`; the artifact is gitignored and produced by `task build:runner`)
- `internal/cmd/` — CLI command handlers (jobs, queue, preview, secrets, key, client, repo). `local/` is the group that boots a VM on this machine against the working directory — `local test`, `local job`, `local preview` — with `local/bootui/` holding the sandbox-boot rendering all three share. `local/preview/sites.go` decides how a preview's sites are addressed on this machine: a loopback port each (`forward.go`), or, given `--base-domain`, real hostnames behind one Host-routed listener (`ingress.go`)

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
- `customize.sh` downloads a pinned, checksum-verified Debian trixie genericcloud snapshot (dated directory + recorded sha512), mounts its rootfs, installs podman/nix/tooling plus the `image/overlay/` systemd units and scripts, loads the vsock/virtio/iso9660/tun modules at boot, and reconverts to a compressed qcow2.
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

## Conventions

- **RPC**: ConnectRPC with buf v2 for code generation
- **CLI**: Kong for argument parsing and subcommand dispatch
- **VM Providers**: Implement the `vm.Provider` interface
- **Generated code**: Always regenerate with `buf generate`, never edit `gen/` directly
- **Build tags**: `//go:build darwin` for macOS-specific code (e.g., vz provider). Non-darwin stubs return `errors.ErrUnsupported`.
