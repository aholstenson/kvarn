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
task image:build    # build kernel, initrd, and disk image into dist/
task image:clean    # remove dist/
```

- Docker-based build (`image/Dockerfile.rootfs`) produces Debian bookworm-slim rootfs with kvarn runner
- `image/build-image.sh` cross-compiles the runner, builds the Docker image, and extracts artifacts
- Output: `dist/vmlinuz`, `dist/initrd`, `dist/disk.img`
- `image/overlay/` contains systemd unit files and scripts copied into the image
- Virtio modules needed for boot must be listed in `/etc/initramfs-tools/modules` and baked into the initrd via `update-initramfs` — the kernel command line cannot load them

## Comments and documentation

- Comments should explain **why** something is done, not investigation history or migration details
- Don't reference previous implementations (e.g. "this used to be X") — only explain the current design

## Conventions

- **RPC**: ConnectRPC with buf v2 for code generation
- **CLI**: Kong for argument parsing and subcommand dispatch
- **VM Providers**: Implement the `vm.Provider` interface
- **Generated code**: Always regenerate with `buf generate`, never edit `gen/` directly
- **Build tags**: `//go:build darwin` for macOS-specific code (e.g., vz provider). Non-darwin stubs return `errors.ErrUnsupported`.
