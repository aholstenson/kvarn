# `projects.toml`

Defines the projects the orchestrator will accept jobs for. A job names a
project; everything else — which repository, which forge, what it may spend, how
much of the host it may hold — is resolved from this file.

Default location `~/.config/kvarn/projects.toml`, overridable with
`--projects-file`. Re-read on every request, so edits apply without a restart.

```toml
[projects.my-project]
repo = "owner/repo"
default_branch = "main"
forge = "github"
```

## Identity

| Key | Type | Notes |
| --- | --- | --- |
| `repo` | string | Repository shorthand (`owner/repo`) or a full clone URL. Required. |
| `default_branch` | string | Branch jobs start from when the request does not name one. |
| `forge` | string | Name of an entry in [`forges.toml`](forges-toml.md). Without one, the job runs but cannot push or open a pull request. |

The table key (`my-project` above) is the project name clients pass to
`kvarn jobs start` and the name API-key scopes match against.

## Cost and retries

```toml
[projects.my-project]
repo = "owner/repo"
max_cost_usd = 5.0
report_cost_on_pr = true
max_validation_retries = 3

[projects.my-project.jobs.review]
max_cost_usd = 1.0
```

| Key | Type | Notes |
| --- | --- | --- |
| `max_cost_usd` | float | Hard budget for a job. The agent is warned as it approaches, and the run is cancelled when it is reached. |
| `report_cost_on_pr` | bool | Include a cost section in the work-log PR comment. |
| `max_validation_retries` | int | Additional agent passes allowed after a required validation step fails. `0` disables retries. |

`[projects.<name>.jobs.<mode>]` overrides `max_cost_usd`,
`max_validation_retries` and `priority` for one agent mode (`auto`,
`implement`, `fix`, `feedback`, `review`, `research`).

Omitting these keys does **not** mean unlimited — the built-in fallbacks are
`max_cost_usd = 5.00`, `max_validation_retries = 3`, `report_cost_on_pr = true`.
The full cascade, including the user-level defaults in `agents.toml`, is in
[`agents.toml`](agents-toml.md#resolution-order).

## Pull-request behavior

```toml
[projects.my-project]
repo = "owner/repo"
forge = "github"
branch_prefix = "agent"
labels = ["automated", "needs-review"]
commit_author_name = "Project Bot"
commit_author_email = "bot@example.com"
```

| Key | Type |
| --- | --- |
| `branch_prefix` | string |
| `labels` | list of strings |
| `commit_author_name` | string |
| `commit_author_email` | string |

These live on the project rather than the forge because one forge serves many
repositories and label sets and branch conventions vary between them. They are
the highest-precedence layer; see
[`forges.toml`](forges-toml.md#resolution-order). `labels` replaces the
inherited list rather than extending it.

## Cloning

| Key | Type | Notes |
| --- | --- | --- |
| `clone_depth` | int | History each job clones. Default `100`; `0` clones full history — needed by tooling that infers versions from tags. |
| `mirror_depth` | int | History the host-side mirror keeps for this project. Overrides `[repos].mirror_depth`; `0` is full history. |

The two are separate knobs: `clone_depth` bounds what a job and its VM see,
`mirror_depth` bounds what the host caches on their behalf. A mirror shallower
than `clone_depth` cannot serve it, so it is deepened to match.

## Concurrency caps

```toml
[projects.my-project]
repo = "owner/repo"
max_jobs = 4
max_cpu = 16
max_memory = "48G"
max_disk = "200G"
priority = 10
```

| Key | Type | Notes |
| --- | --- | --- |
| `max_jobs` | int | Concurrent jobs this project may hold. |
| `max_cpu` | int | Total vCPUs across its running jobs. |
| `max_memory` | size | Total memory, e.g. `"48G"`. |
| `max_disk` | size | Total VM disk, e.g. `"200G"`. |
| `priority` | int | Queue ranking against other projects, higher first. Default `0`. |

Each cap falls back to the `[scheduler.per_project]` default in
[`orchestrator.toml`](orchestrator-toml.md#per-tenant-caps) when unset; an
explicit `0` means unlimited even when a host-wide default exists. A job over
its project's cap waits without blocking other projects.

`priority` orders the queue and never reserves capacity. A waiting job gains
effective priority the longer it waits, so a high-priority project cannot starve
a low-priority one. Override it per mode with
`[projects.<name>.jobs.<mode>].priority`.
