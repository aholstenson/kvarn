# `kvarn.yml`

The per-repository configuration file: how to build the project, what it may
reach on the network, and how to tell whether a change is good. It is read from
the repository root under the first of these names that exists: `kvarn.yml`,
`kvarn.yaml`, `.kvarn.yml`, `.kvarn.yaml`.

A machine-readable schema lives at [`kvarn.schema.json`](../../kvarn.schema.json)
in the repository root; point your editor at it for completion and validation.

Inside the VM, steps run as the unprivileged `kvarn` user with the repository
checked out at `/home/kvarn/workspace` (the working directory for every step).
The home directory is `/home/kvarn`.

## Top-level keys

| Key | Type | Purpose |
| --- | --- | --- |
| `image` | string | OCI image to run steps in. Mutually exclusive with `dependencies`. |
| `dependencies` | map | Nix flake sources mapped to attribute names to install. |
| `vm` | object | VM sizing overrides. |
| `network` | object | Outbound egress allowlist. |
| `cache` | object | Extra guest paths to persist across runs. |
| `environment` | map | Environment variables injected into every step. |
| `secrets` | list | Runtime secrets the project needs. |
| `setup` | object | Steps and health checks run before the agent. |
| `validation` | object | Steps run after the agent. |

All keys are optional; a repository with no `kvarn.yml` gets a bare VM with no
setup and no validation.

## `image`

```yaml
image: node:22-alpine
```

Steps run inside this container image rather than a Nix environment. Cannot be
combined with `dependencies` — shell sessions in an `image:` job run via `podman
exec`, where host-installed Nix binaries are invisible.

Images are pulled through the host's OCI image cache; see
[Speed up job startup](../how-to/speed-up-job-startup.md).

## `dependencies`

```yaml
dependencies:
  nixpkgs:
    - go
    - nodejs
  nixpkgs/nixos-24.11:
    - python312
  github:owner/repo:
    - some-attr
```

Each key is a flake source, each value the list of attributes to install from
it. Accepted source forms:

| Form | Notes |
| --- | --- |
| `nixpkgs` | Resolves to the built-in default channel (`nixos-25.11`). |
| `nixpkgs/<channel>` | e.g. `nixpkgs/nixos-24.11`. |
| `github:owner/repo` | |
| `gitlab:owner/repo` | |
| `git+https://…`, `git+ssh://…` | |
| `https://…`, `tarball+https://…` | |

Attribute names are validated conservatively (they are concatenated into a shell
command). The host of each non-`nixpkgs` source is added to the egress allowlist
automatically.

## `vm`

```yaml
vm:
  cpus: 4
  memory: 8G
  disk: 32G
```

| Key | Default | Minimum |
| --- | --- | --- |
| `cpus` | `2` | `1` |
| `memory` | `4G` | `2G` |
| `disk` | `16G` | `4G` |

Sizes accept `M`, `MiB`, `G`, `GiB` suffixes and whole numbers only. These
figures are also what the job is charged against the host's admission pool, so
raising them lowers how many jobs run concurrently — see
[Tune host capacity](../how-to/tune-host-capacity.md).

## `network`

```yaml
network:
  allowed_hosts:
    - api.example.com
    - "*.internal.example.com"
    - 10.0.0.5
```

Outbound TCP (ports 80 and 443) is denied unless the host matches the
allowlist, on top of the defaults needed to fetch dependencies. Entries are
hostnames, IP addresses, or a `*.domain` wildcard that matches any subdomain.
Schemes, paths and ports are rejected.

## `cache`

Registered tools (language package managers and build caches Kvarn knows about)
are cached automatically and need no `cache:` block. These fields exist for
unregistered tools or custom keying.

```yaml
cache:
  paths:
    - /home/kvarn/.cache/go-build
  entries:
    - path: ~/.cache/custom-tool
      lockfiles:
        - package-lock.json
      bucket: custom-tool
```

`cache.paths` are unkeyed (write-once) guest paths. `cache.entries` give one
path an explicit key:

| Field | Purpose |
| --- | --- |
| `path` | Guest path to cache. Required. |
| `lockfiles` | Files hashed into a content-addressed key; the cache is invalidated when they change. |
| `key` | A fixed key string. You own invalidation. |
| `bucket` | Groups related entries under one name. |

Setting neither `lockfiles` nor `key` gives an unkeyed cache, the same as a
`cache.paths` entry.

Path rules, applied to both forms:

- Absolute paths are used as-is.
- `~` and `~/foo` expand against `/home/kvarn`.
- Relative paths resolve under `/home/kvarn/workspace` and may not escape it
  with `..`.
- The workspace root itself is rejected — it is transferred separately.
- Anything under `/nix` is rejected; the Nix store is cached by its own
  mechanism.

## `environment`

```yaml
environment:
  CI: "true"
```

Injected into every step. Keys must be valid POSIX environment variable names;
values may not contain NUL or newline.

## `secrets`

```yaml
secrets:
  - API_TOKEN
  - name: DOCKERHUB
    scheme: basic
    hosts:
      - registry-1.docker.io
      - auth.docker.io
```

Declares the secrets the project needs; each is exposed inside the VM as an
environment variable of that name. The value comes from the orchestrator's
secret store (`kvarn secrets set`) — this file never contains one.

| Field | Purpose |
| --- | --- |
| `name` | Secret name. Must be a valid POSIX env-var name. |
| `scheme` | How a `managed` secret is applied to an outbound request: `bearer` (default), `basic`, or `oauth`. |
| `hosts` | Restricts substitution to these hosts. Same syntax as `network.allowed_hosts`. Empty means any allowlisted host. |

A bare string entry is shorthand for `{name: NAME}` — the `bearer` scheme over
any allowlisted host. `scheme` and `hosts` only matter for `managed` secrets,
whose real value stays on the host; see
[Manage secrets](../how-to/manage-secrets.md).

Names must be unique and must not collide with an `environment` key.

## `setup`

```yaml
setup:
  steps:
    - name: Install
      run: npm ci
      working_dir: frontend
      timeout: 10m
      retry: 1
  health_checks:
    - name: Check service
      run: curl -f http://localhost:3000/health
```

`steps` run in order before the agent starts and short-circuit on the first
failure. `health_checks` run only if every setup step passed.

## `validation`

```yaml
validation:
  required:
    - name: Tests
      run: npm test
  advisory:
    - name: Lint
      run: npm run lint
      paths:
        - "**/*.ts"
```

Runs after the agent finishes. Every step runs regardless of individual
failures. A failing `required` step fails the job (and may trigger a retry
pass — see [Control job costs](../how-to/control-job-costs.md)); a failing
`advisory` step is reported but does not affect the outcome.

## Step fields

Used by `setup.steps`, `setup.health_checks`, `validation.required` and
`validation.advisory`.

| Field | Type | Notes |
| --- | --- | --- |
| `name` | string | Required. Identifies the step in output and session events. |
| `run` | string | Required. Executed via `sh -c`. |
| `working_dir` | string | Relative to the workspace root. |
| `timeout` | int or duration | Seconds as an integer, or a Go duration string (`30s`, `10m`, `1h30m`). Omitted or `0` means no timeout. |
| `retry` | int | Additional attempts on failure, max 10. **Setup steps only.** |
| `paths` | list | Doublestar globs. **Validation steps only** — the step runs only when a changed file matches. |

## Checking a file

`kvarn test` boots a VM against the current working tree and runs dependencies,
setup, health checks and validation without invoking the agent. That is the
fastest way to confirm a `kvarn.yml` is correct; see
[Configure a repository](../how-to/configure-a-repository.md).
