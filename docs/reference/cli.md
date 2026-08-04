# CLI

Every command is a subcommand of the single `kvarn` binary. They fall into three
groups:

- **Server** — `orchestrator`, run on the host with virtualization.
- **Client** — `jobs`, `feedback`, `queue`, which talk to a
  running orchestrator over HTTP and can run from anywhere that can reach it.
- **Local** — everything else, which reads and writes files on the host
  (config stores, caches, mirrors, the VM image) with no orchestrator involved.

Run `kvarn <command> --help` for the authoritative flag list of any command.

Commands that talk to a running orchestrator resolve `--addr` in three steps:
the flag (or `KVARN_ADDR`) if set, then the host-local control socket if one
exists, then `http://localhost:8080`. On the orchestrator's own host they
therefore need neither an address nor a key. See
[Run the orchestrator](../how-to/run-the-orchestrator.md#the-host-local-control-socket).

## `kvarn orchestrator`

Runs the orchestrator service.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--addr` | `:8080` | Listen address. |
| `--model` | `coding-agent` | Model alias used as the main coding agent. |
| `--no-auth` | off | Disable API-key authentication. Local development only. Env: `KVARN_NO_AUTH`. |
| `--local-socket` | `~/.config/kvarn/orchestrator.sock` | Path of the host-local control socket. Env: `KVARN_LOCAL_SOCKET`. |
| `--no-local-socket` | off | Do not serve the host-local control socket. Env: `KVARN_NO_LOCAL_SOCKET`. |
| `--disk-image-path` | auto | VM disk image, when auto-discovery is not enough. |
| `--projects-file`, `--forges-file`, `--credentials-file`, `--secrets-file`, `--agents-file`, `--api-keys-file`, `--orchestrator-file` | `~/.config/kvarn/…` | Override individual config file paths. |
| `--sessions-db` | `~/.config/kvarn/sessions.db` | Session database path. |
| `--otel-metrics-enabled`, `--otel-exporter-endpoint`, `--otel-service-name` | off, —, `kvarn-orchestrator` | OpenTelemetry metrics export. |

Scheduler flags (`--sched-*`) mirror the `[scheduler]` table and are documented
with it in [`orchestrator.toml`](orchestrator-toml.md#flags-and-environment).

The orchestrator refuses to start without git ≥ 2.26 on `PATH`.

## `kvarn jobs start <project> <prompt>`

Starts a job: clones the project's default branch, runs the agent, validates,
pushes a branch, and opens a pull request where the forge supports it.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--addr` | local socket, else `http://localhost:8080` | Orchestrator address. Env: `KVARN_ADDR`. |
| `--branch` | project default | Branch to start from. |
| `--mode` | `auto` | `auto`, `implement`, `fix`, `feedback`, `review`, `research`. |
| `--watch` | off | Stream the session until it reaches a terminal state. Without it the session ID is printed and the command returns. |
| `--api-key` | — | API key. Env: `KVARN_API_KEY`. |

## `kvarn feedback <project> <pr-ref> <feedback>`

Continues work on an existing pull request: clones its head branch, runs the
agent with the feedback as the task, pushes a follow-up commit to the same
branch, and comments. No second PR is opened.

`<pr-ref>` is in the forge's own format — for GitHub, the PR number.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--addr` | local socket, else `http://localhost:8080` | Orchestrator address. Env: `KVARN_ADDR`. |
| `--mode` | `feedback` | Override the agent mode. |
| `--watch` | off | Stream the session. Without it the session ID is printed and the command returns. |
| `--api-key` | — | API key. Env: `KVARN_API_KEY`. |

## `kvarn jobs`

Starts, lists, inspects and manages the runs the orchestrator holds. Every
subcommand takes `--addr` and `--api-key` (env: `KVARN_API_KEY`); the listing
subcommands take `--json` for scripting, which emits proto field names.

Cancelling stops a run wherever it is: a job still waiting in the backlog is
cancelled where it sits; one already in flight has its context cancelled and the
command returns without waiting — the run unwinds on its own and tears its VM
down. Either way the session ends `cancelled`, not `failed`.

| Subcommand | Purpose |
| --- | --- |
| `start <project> <prompt>` | Start a job; flags are listed above. |
| `list` | Jobs newest first. Filters: `--project`, `--state` (repeatable/comma-separated), `--active`, `--mode`, `--pr-ref`, `--since 24h`, `--limit`, `--all` to follow pagination. |
| `show <session-id>` | One job in full, including its priority, attempt count and cost. |
| `watch <session-id>` | Stream events until the job finishes. `--from` resumes after an event sequence. |
| `events <session-id>` | Replay recorded history and return. `--after`, `--limit`. |
| `cancel [<session-id>]` | Cancel one job, or every job matching `--project`/`--state`/`--mode`/`--pr-ref`. `--dry-run` reports without stopping anything; an unfiltered sweep requires `--all`. |
| `retry <session-id>` | Resubmit a finished job as a new session. `--prompt` replaces the original text, `--watch` follows the new run. |
| `priority <session-id> <priority>` | Reorder a job still waiting in the backlog. Higher runs sooner. |

A retry leaves the original session as the record of what happened. A job that
already opened a pull request is refused — resubmitting it would open a second
one — so continue it with `kvarn feedback` instead.

## `kvarn queue`

Reads the backlog as a queue: whether the host is full, and what runs next.

| Subcommand | Purpose |
| --- | --- |
| `status` | Backlog depth and pipeline population against their bounds, waiters awaiting capacity, pool usage, the host disk guard, and a per-project split. |
| `list` | The backlog in dispatch order, with each entry's position, configured priority and the effective priority it is ordered by. `--project`, `--limit`. |
| `drain` | Stop dispatching so the host can be stopped safely. `--reason`, `--wait`, `--timeout` (default `30m`), `--interval` (default `5s`). Needs the `host` capability. |
| `resume` | Start dispatching from the backlog again. Needs the `host` capability. |

Effective priority is the configured priority plus one level per
`priority_age_step` waited, clamped to the highest priority in the backlog. A
filtered `list` keeps positions relative to the whole backlog, so an entry's
place is its place in the queue rather than among the rows shown.

`drain --wait` blocks until nothing is left running and exits non-zero if that
has not happened within `--timeout`, so it can be the first line of a stop
script. See [Take a host out of service](../how-to/take-a-host-out-of-service.md).

Both take `--addr`, `--api-key` and `--json`.

## `kvarn run <prompt>`

Runs the coding agent against the local working directory, with no
orchestrator, project, or forge. Write-capable modes require exactly one of
`--diff` or `--apply`.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--diff` | — | Write a unified diff of all changes to stdout. |
| `--apply` | — | Copy changed files from the VM back onto the working directory. |
| `--dir` | `.` | Project directory. |
| `--mode` | `auto` | Agent mode. |
| `--model` | `coding-agent` | Model alias. |
| `--max-validation-retries` | `0` | Additional agent passes after a required validation failure. |
| `--no-cache` | off | Disable cache persistence for this run. |
| `--cache-quota` | `5G` | Per-project tool-cache limit for the LRU sweep. |
| `-p`, `--project` | git remote → project store | Project name used for secret lookup. |
| `--secrets-file`, `--agents-file` | `~/.config/kvarn/…` | Config overrides. |
| `--disk-image-path` | auto | VM disk image. |
| `-v`, `--verbose` | off | Show output from passing steps too. |
| `--logs` | off | Show log output. |

## `kvarn test`

Boots a VM against the working tree and runs dependencies, setup, health checks
and validation **without invoking the agent** — the fastest way to check a
`kvarn.yml`.

Flags: `--dir`, `--no-cache`, `-v`/`--verbose`, `--logs`, `-p`/`--project`,
`--secrets-file`, `--disk-image-path`, as for `kvarn run`.

## `kvarn key`

Manages `apikeys.toml` directly; no running orchestrator is required, and the
orchestrator picks up changes on the next request.

```sh
kvarn key create --name ci --projects myproj
kvarn key create --name admin --projects '*' --expires 720h
kvarn key create --name ops --projects '*' --capability host
kvarn key list
kvarn key disable <key-id>
kvarn key revoke <key-id>
```

| Command | Flags |
| --- | --- |
| `create` | `--name` (required), `--projects` (default `*`, repeatable or comma-separated), `--capability` (repeatable; `host`), `--expires` (RFC3339 timestamp or Go duration), `--api-keys-file` |
| `list` | `--api-keys-file` |
| `disable <key-id>` | `--api-keys-file` |
| `revoke <key-id>` | `--api-keys-file` |

The full token is printed once at creation and never again.

## `kvarn secrets`

Manages per-project runtime secrets in `secrets.toml`.

```sh
printf '%s' "$API_TOKEN" | kvarn secrets set my-project API_TOKEN
kvarn secrets set my-project GITHUB_TOKEN --type managed --value "$GITHUB_TOKEN"
kvarn secrets list my-project
kvarn secrets remove my-project API_TOKEN
```

| Command | Flags |
| --- | --- |
| `set <project> <name>` | `--type` (`env` or `managed`, default `env`), `--value` (read from stdin when omitted), `--secrets-file` |
| `list <project>` | `--secrets-file`. Values are never printed. |
| `remove <project> <name>` | `--secrets-file` |

## `kvarn repo`

Manages the host-side bare mirrors. `list`, `gc` and `clear` read the on-disk
store directly with no running orchestrator; `pull` reaches the forge and so
resolves credentials the way a job would.

| Command | Flags |
| --- | --- |
| `pull <project>` | `--branch` (default: the project's default branch), `--depth` (`0` keeps everything), `--dir`, `--projects-file`, `--forges-file`, `--credentials-file` |
| `list` | `--dir` |
| `gc [<project>]` | `--older-than` (drop branch refs unused for longer, e.g. `720h`; empty keeps all), `--dir` |
| `clear <project>` | `--dir` |

`--dir` defaults to `~/.cache/kvarn/repos` everywhere.

## `kvarn cache`

Manages the per-project tool caches restored into each VM.

| Command | Flags |
| --- | --- |
| `list` | `-p`/`--project`, `--cache-dir` |
| `clear [<project>]` | `--all`, `--cache-dir` |
| `evict` | `--per-project`, `--global`, `--cache-dir` |

`--cache-dir` defaults to `~/.cache/kvarn`.

## `kvarn image-cache`

Manages the pull-through OCI image cache used by `image:` jobs.

| Command | Flags |
| --- | --- |
| `list` | `--dir` |
| `stats` | `--dir` — totals plus hit/miss counters. |
| `clear` | `--all`, `--repo` (e.g. `library/python`), `--dir` |
| `evict` | `--global` (required, e.g. `50G`), `--dir` |

`--dir` defaults to `~/.cache/kvarn/image-cache`.

## `kvarn image`

Resolves and pre-seeds the VM disk image.

| Command | Flags |
| --- | --- |
| `pull` | `--version` (a version or semver range; env `KVARN_IMAGE_VERSION`), `--arch` (`arm64` or `amd64`) |
| `path` | `--version`, `--arch`, `--no-download` (resolve a local or cached image only) |

```sh
kvarn image pull                              # download the current version
KVARN_IMAGE_VERSION=0.1.0 kvarn image pull    # a specific version
kvarn image path --no-download                # print a local path, never download
```
