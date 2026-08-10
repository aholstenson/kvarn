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
| `modes` | map | Agent modes this repository defines, beside the built-in ones. |

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

Declaring certain attributes brings more than the package: caching, egress
hosts, environment and, for a version manager like `mise`, the command that
installs what it pins. See [Registered tools](registered-tools.md) for the list
and what each one sets up.

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

A denied connection is closed without an answer, so the program inside the VM
reports a truncated download or a reset connection rather than a refusal. Kvarn
records the hostname and names it in whatever failure follows, so a job that
dies on "unexpected EOF" still tells you which host to add here.

Matching is per hostname, and a redirect is a new connection to a new host: a
download that starts at an allowed host and redirects to a CDN needs the CDN
allowed too.

## `cache`

[Registered tools](registered-tools.md) (language package managers and build
caches Kvarn knows about) are cached automatically and need no `cache:` block.
These fields exist for unregistered tools or custom keying.

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

## `modes`

```yaml
modes:
  review-pr:
    description: Review an open pull request and post the review as a comment.
    extends: review
    start: pull-request
    deliver:
      - pr-comment
    context:
      - pr-metadata
      - pr-diff
    prompt: |
      Hold the change to the house style: tests in the project's framework,
      comments that explain why rather than what changed.
```

Declares agent modes for this repository, keyed by the name a job selects with
`--mode`. They sit beside the six built-in modes (`auto`, `implement`, `fix`,
`feedback`, `review`, `research`) rather than replacing one: a definition
inherits from a mode via `extends` and overrides only the axes it names. Run
`kvarn modes list` in a checkout to see the resolved set.

A mode is a point in seven axes:

| Field | Purpose |
| --- | --- |
| `description` | One-line summary shown by `kvarn modes list`. |
| `extends` | Mode to inherit from — a built-in, or another mode in this file. Defaults to `auto`. |
| `prompt` | Instructions appended to the inherited prompt. It adds to the inherited guidance; there is no way to replace it. |
| `workspace` | `read-only` withholds the file-editing tools; `read-write` gives the full toolkit. Inherited. |
| `validation` | `skip`, `run`, or `require`. Defaults to `run` for a read-write mode and `skip` otherwise. |
| `deliver` | Where the result goes: `none`, `pr-comment`, `follow-up-commit`, `new-pull-request`. Inherited. |
| `start` | Where a run may begin: `branch`, `pull-request`, or `any`. Inherited. |
| `context` | Sections prepended to the task message: `none`, `original-task`, `pr-metadata`, `pr-diff`. Inherited. |

Every axis is inherited, so a mode extending `feedback` starts where `feedback`
starts — on a pull request — rather than widening to `any`. Narrow or widen it
by naming `start` explicitly. `validation` is the exception: it follows the
resolved `workspace` unless named, so overriding the workspace alone does not
leave a read-only mode running a write mode's validation policy.

`validation: run` feeds a failing required step back to the agent to fix, up to
the configured retry budget, and fails the job if it is still red. In a
read-only mode there is nothing to fix, so `run` records the outcome and lets
the run finish. `validation: require` fails the job on the first failing
required step with no retry — which is what makes a read-only "test this pull
request" mode report an honest verdict rather than a green one. A failing
`require` run still delivers to `pr-comment` before it fails, so the verdict
reaches the pull request; it does not deliver commits, since the run has just
established that what it produced does not pass.

Validation steps that declare `paths` are gated on the run's own diff. A
read-only mode has no diff, so every step runs: gating them on an empty diff
would skip each path-scoped step and report the pass those skips add up to.

`deliver` accepts more than one sink. They fire in a fixed order —
`new-pull-request`, then `follow-up-commit`, then `pr-comment` — so a comment
lands on the pull request the same run just opened. A commit sink already posts
its own comment carrying the summary and work log, so an explicit `pr-comment`
alongside one is skipped rather than posted twice. A delivery that fails, fails
the job: work that never left the host is not a success.

`new-pull-request` in a run started against an existing pull request commits
onto that pull request instead. Naming one asks for the work to land there, and
a second pull request would only target the first one's head branch.

A mode with `deliver: [none]` leaves its output in the session, where
`kvarn jobs result <session-id>` reads it. `context: [none]` is the same idea
for the context pack. Write those rather than an empty list: `deliver: []` is
rejected, because a definition supplied with a request cannot tell an empty list
from an absent one, and an absent one inherits.

Rejected at load time: a name that shadows a built-in, an `extends` that names
nothing, a cycle of `extends`, a value outside an axis's vocabulary, an empty
`deliver` or `context` list, `none` combined with another sink or block, both
commit sinks together, a commit sink in a read-only mode, and `follow-up-commit`
in a mode that can only start from a branch. Names must be lowercase
alphanumerics separated by single hyphens.

Rejected at submission, or as soon as the run resolves a mode the repository
defines: a mode that cannot deliver from where the run begins. A mode that
delivers a follow-up commit or a comment needs a pull request to act on, so
submitting one without a pull request reference is refused up front rather than
after a clone, a VM boot and a full agent run.

A mode definition is read from the repository the run works on, which for a job
continuing a pull request is that pull request's head branch. A branch can
therefore change what a mode it is reviewed under is allowed to do. This is the
same trust boundary `setup.steps` already sits on — both execute what the branch
says — so treat a named mode as a property of the branch under review, not a
guarantee the reviewer controls.

A caller that needs a mode this file does not define can supply one with the
request instead; see `kvarn jobs start --mode-spec` in
[CLI reference](cli.md).

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
