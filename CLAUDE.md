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

## Comments and documentation

- Comments should explain **why** something is done, not investigation history or migration details
- Don't reference previous implementations (e.g. "this used to be X") — only explain the current design

## Authentication

- External `OrchestratorService` calls authenticate with an API key sent as `Authorization: Bearer <token>`. Validated by a ConnectRPC interceptor (`internal/orchestrator/auth`); each key is scoped to a set of projects and every project-scoped RPC checks that scope. The host-local `BridgeService` (runner↔orchestrator) is intentionally **not** authenticated.
- Token format: `kvarn_<keyid>_<secret>`. The `kvarn_` prefix lets secret scanners recognize leaks; `keyid` is the O(1) lookup handle, `secret` is 160 bits of CSPRNG. Both components are base32 (lowercased, unpadded) so they never contain the `_` delimiter. Only `sha256(secret)` is persisted (plain SHA-256 is correct for high-entropy random secrets).
- Auth is **enforced by default**; `--no-auth` (or `KVARN_NO_AUTH`) disables it for local dev. With auth on and zero keys, all requests are denied. Keys are bootstrapped with `kvarn key create`, which writes `apikeys.toml` directly — no running orchestrator needed.
- **TLS is out of scope**: the orchestrator stays on h2c and assumes an external TLS-terminating reverse proxy. A bearer token is only safe over TLS.
- **Hot-reload**: every tomlstore re-reads its file per `Get`/`List`, so key changes apply on the next request with no restart. All stores write atomically (`internal/config/atomicfile`, temp file + rename) so a concurrent `kvarn key create` is never read mid-write.

## Conventions

- **RPC**: ConnectRPC with buf v2 for code generation
- **CLI**: Kong for argument parsing and subcommand dispatch
- **VM Providers**: Implement the `vm.Provider` interface
- **Generated code**: Always regenerate with `buf generate`, never edit `gen/` directly
- **Build tags**: `//go:build darwin` for macOS-specific code (e.g., vz provider). Non-darwin stubs return `errors.ErrUnsupported`.
