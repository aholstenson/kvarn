# Kvarn

Kvarn is a server that runs coding-agent jobs in isolated VMs. You host the orchestrator on a machine with virtualization — a Linux server with KVM/QEMU, or a Mac — point it at your projects, and send it jobs over its API. For each job it clones the repository, boots a fresh VM sandbox, runs setup and validation, and opens a pull request through a configured forge.

⚠️ This is an experiment in using LLMs to automate software development. Coding agents, and Kvarn itself, has been used to build this software.

## Features

- **Isolated by default.** Every job runs in its own fresh, ephemeral VM (Apple Virtualization on macOS, KVM/QEMU on Linux), so the agent's commands never touch your host.
- **Coding agent with modes.** Pick `implement`, `fix` (test-first), `review`, and `research`, or let `auto` choose the best action.
- **Reproducible environments from one file.** `kvarn.yml` declares dependencies (Nix flakes or an OCI image), setup steps, health checks, and required/advisory validation, with per-project caches reused across runs.
- **Locked-down networking.** Outbound traffic is blocked unless you allowlist the host, enforced by a host egress proxy the VM can't bypass.
- **Secrets the agent never sees.** Bearer tokens stay on the host and are injected into outbound requests by the proxy; inside the VM the agent only ever holds an unguessable placeholder.
- **Cost guardrails.** Live per-model token and USD tracking, a soft warning threshold, and a hard budget cap that stops the run before it overspends.
- **From prompt to pull request.** Send the orchestrator a project name and a prompt; it clones the repository, runs the agent, validates the result, pushes a branch, and opens a PR on GitHub. (For local development, `kvarn run --diff` / `--apply` does the same against your working tree.)

## Requirements

- **macOS** (Apple Virtualization framework) or **Linux** (KVM/QEMU) to run the VM sandboxes. Other platforms are unsupported.
- An API key for your model provider, in the environment. The default agent uses Anthropic, so export `ANTHROPIC_API_KEY`; `OPENAI_API_KEY`, `OPENROUTER_API_KEY`, and `GEMINI_API_KEY` (or `GOOGLE_API_KEY`) are also recognized.

## Install

### Prebuilt binaries

Download the archive for your platform from the [latest release](https://github.com/aholstenson/kvarn/releases/latest), verify the checksum, and put `kvarn` on your `PATH`:

```sh
# Linux amd64 example - adjust platform, arch, and version to taste.
base=https://github.com/aholstenson/kvarn/releases/latest/download
curl -fSLO "$base/kvarn_v0.1.0_linux_amd64.tar.gz"
curl -fSLO "$base/kvarn_v0.1.0_linux_amd64.tar.gz.sha256"
sha256sum -c kvarn_v0.1.0_linux_amd64.tar.gz.sha256
tar xzf kvarn_v0.1.0_linux_amd64.tar.gz
sudo mv kvarn /usr/local/bin/
```

The VM disk image is **downloaded automatically** the first time a VM-backed command (`run`, `test`, `orchestrator`) needs it. See [The VM disk image](#the-vm-disk-image).

### macOS

The macOS binaries are ad-hoc signed with the virtualization entitlement, but binaries downloaded from the web are quarantined by Gatekeeper. Clear the quarantine flag before the first run:

```sh
xattr -d com.apple.quarantine ./kvarn
```

## Quick start

You run the orchestrator on a host with virtualization and send it jobs. Install `kvarn` first (see [Install](#install)); the VM disk image downloads automatically the first time the orchestrator needs it.

Export your model-provider API key on the host that runs the orchestrator:

```sh
export ANTHROPIC_API_KEY=sk-ant-...
```

Tell the orchestrator about a project and where to open pull requests. In `~/.config/kvarn/projects.toml`:

```toml
[projects.my-project]
repo = "owner/repo"
default_branch = "main"
forge = "github"
```

Then define that forge and its credential in `forges.toml` and `credentials.toml` — see [Orchestrator configuration](#orchestrator-configuration) for all the files and fields.

Add a `kvarn.yml` to the repository so Kvarn knows how to build and validate it:

```yaml
dependencies:
  nixpkgs:
    - go

setup:
  steps:
    - name: Download modules
      run: go mod download

validation:
  required:
    - name: Test
      run: go test ./...
```

Start the orchestrator:

```sh
kvarn orchestrator --addr :8080
```

From any machine, send it a job and watch the session stream to completion:

```sh
kvarn startjob --addr http://localhost:8080 my-project "Fix the failing tests"
```

It looks up the project, clones the repository, loads its `kvarn.yml`, runs setup, invokes the agent, validates the result, pushes a branch, and opens a PR when the forge supports it.

### Local development

`run` and `test` work against your current working tree without the orchestrator, projects, or a forge — useful while iterating on a project's `kvarn.yml` or trying the agent before wiring everything up.

Validate the config in a VM. This boots the sandbox, installs dependencies, and runs setup, health checks, and validation — without invoking the agent:

```sh
kvarn test
```

Run the coding agent against the current working tree:

```sh
kvarn run --diff "Fix the failing tests"           # print a unified diff
kvarn run --apply "Implement the requested change" # copy changed files back
```

Write-capable modes require one of `--diff` or `--apply`. Useful options include:

- `--mode auto|implement|fix|review|research` to steer agent behavior (see [Agent modes](#agent-modes)).
- `--model <alias-or-provider-model>` to override the main coding model.
- `--dir <path>` to run against a different project directory.
- `--logs` and `-v` to show more VM and step output.
- `--no-cache` to disable cache persistence for a run.
- `--project` and `--secrets-file` when the project declares secrets in `kvarn.yml`.

## Agent modes

`--mode` (default `auto`) selects how the agent approaches the task:

- **`auto`** — inspect the task and pick the most suitable approach.
- **`implement`** — build a feature or refactor: plan, then write the code.
- **`fix`** — fix a bug test-first: reproduce it with a failing test, then make it pass.
- **`review`** — read-only audit of the working tree; reports findings, changes nothing.
- **`research`** — read-only investigation; answers an open-ended question, changes nothing.

`review` and `research` never modify files, so they ignore `--diff`/`--apply` and never open a PR — they write their answer to stdout instead.

## How isolation works

Every job runs in a throwaway VM, and the only path off that VM is a host-side egress proxy:

- **Per-job VM.** Each run boots a fresh VM (Apple Virtualization on macOS, KVM/QEMU on Linux) and tears it down afterward. The agent's shell commands run inside the guest, never on your host.
- **Egress allowlist.** All outbound traffic flows through a proxy on the host. Requests to hosts that are not in `network.allowed_hosts` (plus the defaults needed to fetch dependencies) are blocked. The proxy terminates TLS using an ephemeral CA whose private key never leaves the host; only its public certificate is trusted inside the guest.
- **Bearer secrets stay on the host.** A `bearer` secret is exposed to the job as an unguessable placeholder. The proxy swaps the placeholder for the real value just before the request leaves the host, so the credential itself never enters the VM.

## Project configuration

Each repository is configured with `kvarn.yml` (or `kvarn.yaml`, `.kvarn.yml`, `.kvarn.yaml`). The schema is in `kvarn.schema.json`. A full example using Nix dependencies:

```yaml
dependencies:
  nixpkgs:
    - go
    - nodejs

vm:
  cpus: 4
  memory: 8G
  disk: 32G

network:
  allowed_hosts:
    - api.example.com

cache:
  paths:
    - /home/kvarn/.cache/go-build
  entries:
    - path: /home/kvarn/.cache/custom-tool
      lockfiles:
        - package-lock.json
      bucket: custom-tool

environment:
  CI: "true"

secrets:
  - API_TOKEN

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

To use an OCI container image instead of Nix for the build environment, replace `dependencies:` with `image:`:

```yaml
image: node:22-alpine

setup:
  steps:
    - name: Install
      run: npm ci
      timeout: 10m

validation:
  required:
    - name: Tests
      run: npm test
```

Important constraints:

- `vm.cpus` defaults to `2`, `vm.memory` to `4G`, and `vm.disk` to `16G`. Set them only when a job needs more (or less) than the defaults.
- `image` and `dependencies` are mutually exclusive.
- `dependencies` keys may be `nixpkgs`, `nixpkgs/<channel>` (e.g. `nixpkgs/nixos-24.11`), `github:owner/repo`, `gitlab:owner/repo`, `git+https://…`, `git+ssh://…`, `https://…`, or `tarball+https://…`.
- `network.allowed_hosts` entries are hostnames or IP addresses only; do not include a scheme, path, or port.
- `cache.paths` entries must be absolute guest paths and must not be under `/home/kvarn/workspace` or `/nix`. The same rule applies to `cache.entries[*].path`.
- `environment` keys and `secrets` names must be valid POSIX environment variable names.
- Step `working_dir` values must be relative to the workspace root.
- Step `timeout` accepts either seconds or a Go duration string such as `30s`, `10m`, or `1h30m`.
- `retry` applies to setup steps and is capped at 10 additional attempts.

## Orchestrator configuration

The orchestrator reads its configuration from `~/.config/kvarn/` by default. You can override each file with a flag:

| File | Flag | Purpose |
| --- | --- | --- |
| `projects.toml` | `--projects-file` | Project names, repositories, default branches, forge selection, and cost limits. |
| `credentials.toml` | `--credentials-file` | Forge credentials. Written with mode `0600` when managed by the store. |
| `secrets.toml` | `--secrets-file` | Per-project runtime secrets. Prefer `kvarn secrets` to edit this file. |
| `forges.toml` | `--forges-file` | Named forge instances such as GitHub or plain Git. |
| `agents.toml` | `--agents-file` | Model aliases for the coding agent. |
| `apikeys.toml` | `--api-keys-file` | API keys that authenticate orchestrator clients. Mode `0600`; edit with `kvarn key`. |

Common orchestrator flags:

- `--addr`, default `:8080`, chooses the listen address.
- `--model`, default `coding-agent`, overrides the main coding-agent model alias.
- `--disk-image-path` points at the VM disk image when auto-discovery is not enough.
- `--no-auth` disables API-key authentication (local dev only — never expose an unauthenticated orchestrator to an untrusted network).

Once the orchestrator is running, the `startjob` command acts as a client that talks to it over HTTP at `--addr` (default `http://localhost:8080`), so it can run from anywhere that can reach the host. (`kvarn secrets` and `kvarn key` are separate — they edit the orchestrator's local TOML files, so they run on the host where those files live.)

### Authentication

External clients authenticate with an API key sent as `Authorization: Bearer <token>`. Authentication is **enforced by default**; pass `--no-auth` to disable it for local development.

The orchestrator speaks cleartext HTTP/2 (h2c) and does not terminate TLS itself. A bearer token is only safe over an encrypted channel, so **always run the orchestrator behind a TLS-terminating reverse proxy** when it is reachable over a network.

Keys are stored hashed (`sha256` of the secret part); the full token is shown once at creation and never again. Each key is scoped to a set of projects — every project-scoped RPC (`StartJob`, `GetSession`, `WatchSession`, `ListSessions`) checks that scope. With auth enabled and no keys configured, all requests are rejected until you create one.

Manage keys with `kvarn key`, which edits `~/.config/kvarn/apikeys.toml` directly (no running orchestrator required). The orchestrator re-reads the file on every request, so creating, disabling, or revoking a key takes effect immediately — no restart.

```sh
# Create a key scoped to one project (prints the token once).
kvarn key create --name ci --projects myproj

# Create a key valid for every project, expiring in 30 days.
kvarn key create --name admin --projects '*' --expires 720h

kvarn key list
kvarn key disable <key-id>   # keep it on record but reject it
kvarn key revoke <key-id>    # delete it entirely
```

Pass the token to `startjob` with `--api-key` or the `KVARN_API_KEY` environment variable:

```sh
KVARN_API_KEY=kvarn_… kvarn startjob myproj "fix the failing test"
```

### Projects and forges

Configure projects by editing `~/.config/kvarn/projects.toml`:

```toml
[projects.my-project]
repo = "owner/repo"
default_branch = "main"
forge = "github"
```

Optionally cap spend per job and report cost on the PR:

```toml
[projects.my-project]
repo = "owner/repo"
default_branch = "main"
forge = "github"
max_cost_usd = 5.0       # hard budget; the job is cancelled once it is reached
report_cost_on_pr = true # post a cost summary on the opened PR

[projects.my-project.jobs.review]
max_cost_usd = 1.0       # override the cap for a specific mode
```

The agent receives a soft warning as it approaches `max_cost_usd` and is cancelled once it reaches it. Omit these keys to leave spending unlimited.

Configure the referenced forge in `~/.config/kvarn/forges.toml`:

```toml
[forges.github]
type = "github"
credential = "github"
branch_prefix = "kvarn"
labels = ["kvarn"]
commit_author_name = "kvarn"
commit_author_email = "kvarn@noreply"
```

Only `type` and `credential` are required. `branch_prefix`, `labels`, `commit_author_name`, and `commit_author_email` are optional; any you omit fall back to the `[defaults]` block (below) and then to Kvarn's built-in values (`branch_prefix = "kvarn"`, `labels = ["kvarn"]`, `commit_author_name = "kvarn"`, `commit_author_email = "kvarn@noreply"`).

For a plain Git remote instead of GitHub:

```toml
[forges.git]
type = "git"
credential = "git-creds"
```

Plain Git supports cloning and pushing but cannot create pull requests.

#### Forge defaults

A `[defaults]` block sets behavior shared by every forge, so you only repeat a value in the forges that need to differ:

```toml
[defaults]
branch_prefix = "agent"
labels = ["automated", "kvarn"]
commit_author_name = "Kvarn Bot"
commit_author_email = "bot@example.com"

[forges.github]
type = "github"
credential = "github"
labels = ["agent"]   # overrides the default for this forge only

[forges.git]
type = "git"
credential = "git-creds"
# inherits branch_prefix, labels, and commit author from [defaults]
```

Each field resolves independently, from highest precedence to lowest: a per-project override (see [Per-project PR behavior](#per-project-pr-behavior)), then the named forge, then the `[defaults]` block, then Kvarn's built-in. `labels` are replaced wholesale, not merged — a layer that sets its own `labels` does not inherit the list from below. The `[defaults]` block is optional; omit it to use the built-ins.

#### Per-project PR behavior

A single forge is shared by many projects, but some settings vary per repository — different repos often use different label sets and branch conventions. Set them on the project in `projects.toml` to override the forge for that project only:

```toml
[projects.my-project]
repo = "owner/repo"
default_branch = "main"
forge = "github"
branch_prefix = "agent"
labels = ["automated", "needs-review"]
commit_author_name = "Project Bot"
commit_author_email = "bot@example.com"
```

These are the highest-precedence layer in the cascade above: a value set here wins over the forge, the `[defaults]` block, and the built-ins. Omit any field to inherit it. As elsewhere, `labels` set on the project replace the inherited list rather than extending it.

### Credentials

Forge credentials live in `~/.config/kvarn/credentials.toml`. For a GitHub personal access token:

```toml
[credentials.github]
token = "ghp_..."
```

GitHub App credentials are also supported:

```toml
[credentials.github-app]
app_id = "12345"
private_key_path = "/path/to/private-key.pem"
installation_id = "67890"
```

For a plain Git forge, use any credential fields appropriate for the remote:

```toml
[credentials.git-creds]
username = "user"
password = "..."
# or: token = "..."
# or: ssh_key_path = "/path/to/key"
```

### Secrets

Use the CLI to manage per-project runtime secrets. Values are stored in `~/.config/kvarn/secrets.toml` by default and are not printed by `list`:

```sh
printf '%s' "$API_TOKEN" | kvarn secrets set my-project API_TOKEN
kvarn secrets set my-project GITHUB_TOKEN --type bearer --value "$GITHUB_TOKEN"
kvarn secrets list my-project
kvarn secrets remove my-project API_TOKEN
```

Secret types:

- `env` secrets are injected as environment variables in the VM.
- `bearer` secrets are exposed to the job as placeholders; the host egress proxy replaces placeholders with the real bearer value before outbound requests leave the host.

A project must declare the names it needs in `kvarn.yml`:

```yaml
secrets:
  - API_TOKEN
  - GITHUB_TOKEN
```

### Model aliases

Model aliases live in `~/.config/kvarn/agents.toml`. These aliases override the built-in defaults:

```toml
[models.coding-agent]
model = "anthropic/claude-sonnet-4-6"
reasoning_effort = "medium"
max_output_tokens = 16384

[models.coding-agent-small]
model = "anthropic/claude-haiku-4-5"
max_output_tokens = 8192
```

`--model` on `kvarn run` and `kvarn orchestrator` overrides the main `coding-agent` alias for that invocation.

## The VM disk image

VM-backed commands (`run`, `test`, `orchestrator`) need a disk image for the host architecture. kvarn resolves it in this order:

1. `--disk-image-path`, if given.
2. A local build: `dist/<arch>/disk.qcow2` next to the `kvarn` binary, then `/usr/local/share/kvarn/dist/<arch>/disk.qcow2`, then `/opt/kvarn/dist/<arch>/disk.qcow2`.
3. The download cache: `~/.cache/kvarn/images/<version>/<arch>/disk.qcow2`.
4. Otherwise, for released builds, the matching image is downloaded from the GitHub release and cached. Downloads are verified against the published `.sha256`.

The version defaults to the CLI's build version. Override it with the `KVARN_IMAGE_VERSION` environment variable (or `--version` on the `kvarn image` subcommands). A `dev` build (built from source without a release tag) does not auto-download — build an image with `task image:build` or pass `--disk-image-path`.

Pre-seed the cache for offline use, or inspect the resolved path:

```sh
kvarn image pull                              # download the current version into the cache
KVARN_IMAGE_VERSION=v0.1.0 kvarn image pull   # download a specific version
kvarn image path                              # print the resolved path (downloading if needed)
kvarn image path --no-download                # print a local/cached path only, never download
```

## Building from source

Required tools:

- **Go**, [Task](https://taskfile.dev), and [buf](https://buf.build) — only needed to build `kvarn` from source; prebuilt binaries are published with each release.
- **Docker** — only needed to build the VM disk image yourself; released builds download a prebuilt image automatically.

`task build` is the preferred path. The underlying steps are:

```sh
buf generate          # regenerate protobuf + ConnectRPC code into gen/
go build ./cmd/kvarn  # build the kvarn binary
```

To build a disk image locally for development, run `task image:build` (writes `dist/<arch>/disk.qcow2`). See [The VM disk image](#the-vm-disk-image) for how the image is resolved at runtime, including `--disk-image-path` for an image kept elsewhere.
