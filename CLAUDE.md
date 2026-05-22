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
- `internal/config/` — User-level config stores (credential, project)
- `internal/jobconfig/` — Per-repo kvarn.yml parsing and step execution
- `internal/orchestrator/` — Orchestrator service
- `internal/runner/` — Runner service (ConnectRPC handler)
- `internal/cmd/` — CLI command handlers (startjob, verify)

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

- `image/build-image.sh` cross-compiles the runner for `linux/<arch>`, then runs `image/customize.sh` in a privileged Docker container.
- `customize.sh` downloads the Debian trixie genericcloud qcow2, mounts its rootfs, bakes in the kvarn runner (installed as `/usr/local/bin/kvarn`) plus the `image/overlay/` systemd units and scripts, loads the vsock/virtio modules at boot, and reconverts to a compressed qcow2.
- Output: `dist/<arch>/disk.qcow2` — one self-contained image per arch.
- The runner binary is baked into the image, so the image and the CLI/orchestrator are version-coupled by convention: the same release tag builds both.

### Release flow

- Pushes to `main` drive Release Please (`.github/workflows/release-please.yml`), which maintains a release PR from Conventional Commits. Merging it tags `vX.Y.Z` and publishes a GitHub Release.
- `.github/workflows/release.yml` then builds and uploads, per arch, `kvarn-disk-<arch>.qcow2` + `.sha256` and the `kvarn` CLI binaries (`.tar.gz`/`.zip` + `.sha256`).
- At runtime, VM commands resolve the image via `vm.EnsureDiskImage`: explicit `--disk-image-path` → local `dist/`/system paths → per-version user cache → download from the release. Downloads are checksum-verified and cached at `~/.cache/kvarn/images/<version>/<arch>/disk.qcow2`. `KVARN_IMAGE_VERSION` (or `--version` on `kvarn image`) overrides the version; `kvarn image pull` pre-seeds the cache.

## Comments and documentation

- Comments should explain **why** something is done, not investigation history or migration details
- Don't reference previous implementations (e.g. "this used to be X") — only explain the current design

## Conventions

- **RPC**: ConnectRPC with buf v2 for code generation
- **CLI**: Kong for argument parsing and subcommand dispatch
- **VM Providers**: Implement the `vm.Provider` interface
- **Generated code**: Always regenerate with `buf generate`, never edit `gen/` directly
- **Build tags**: `//go:build darwin` for macOS-specific code (e.g., vz provider). Non-darwin stubs return `errors.ErrUnsupported`.
