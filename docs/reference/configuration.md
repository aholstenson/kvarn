# Configuration overview

Kvarn has two configuration surfaces:

- **Per repository** — a `kvarn.yml` checked into the repository, describing how
  to build, run and validate it. See [`kvarn.yml`](kvarn-yml.md).
- **Per host** — TOML files the operator keeps on the machine that runs the
  orchestrator, describing which projects exist, where to push, and how much of
  the host jobs may use.

## Host configuration files

All host files live in `~/.config/kvarn/` by default. Each has a flag that
overrides its path.

| File | Flag | Contents |
| --- | --- | --- |
| [`projects.toml`](projects-toml.md) | `--projects-file` | Projects, their repositories, forge selection, and per-project overrides. |
| [`forges.toml`](forges-toml.md) | `--forges-file` | Named forge instances and pull-request behavior. |
| [`credentials.toml`](credentials-toml.md) | `--credentials-file` | Credentials referenced by forges, and LLM provider API keys. Mode `0600`. |
| [`secrets.toml`](../how-to/manage-secrets.md) | `--secrets-file` | Per-project runtime secrets. Edit with `kvarn secrets`. |
| [`agents.toml`](agents-toml.md) | `--agents-file` | Model aliases and user-level job defaults. |
| [`apikeys.toml`](apikeys-toml.md) | `--api-keys-file` | API keys that authenticate orchestrator clients. Mode `0600`; edit with `kvarn key`. |
| [`orchestrator.toml`](orchestrator-toml.md) | `--orchestrator-file` | Host-level settings: scheduler pool, caches, mirrors, session retention. |
| `sessions.db` | `--sessions-db` | SQLite database of sessions and their event logs. Owned by the orchestrator. |

Every file is optional. A missing file behaves the same as an empty one.

`projects.toml`, `forges.toml`, `credentials.toml`, `secrets.toml`,
`agents.toml` and `apikeys.toml` are **re-read on every request**, so edits take
effect immediately with no restart. `orchestrator.toml` and `--sessions-db` are
read once at startup.

A job reads what it needs when it starts and keeps that for its whole run, so an
edit applies to the next job rather than to jobs already in flight. This matters
most for `agents.toml`: the model, reasoning effort and step budget a job runs
with are fixed at the moment it starts, and its step budget spans every
validation retry, not each turn separately.

Writes go through a temp file and a rename, and writers hold a lock on
`<file>.lock` for the whole load → modify → save sequence, so a `kvarn key
create` racing the orchestrator (or another CLI invocation) can neither be read
half-written nor lose the other's edit.

## State and cache directories

| Path | Contents | Managed with |
| --- | --- | --- |
| `~/.cache/kvarn/images/<version>/<arch>/disk.qcow2` | Downloaded VM disk images. | `kvarn image` |
| `~/.cache/kvarn/repos/<project>/` | Per-project bare Git mirrors. | `kvarn repo` |
| `~/.cache/kvarn/image-cache/` | Pull-through OCI image cache. | `kvarn image-cache` |
| `~/.cache/kvarn/` (tool layers) | Per-project tool caches restored into each VM. | `kvarn cache` |
| `~/.config/kvarn/sessions.db` | Session history and event logs. | Pruned by `[sessions].retention` |

## Environment variables

| Variable | Effect |
| --- | --- |
| `ANTHROPIC_API_KEY` | Model provider key for the default agent, used for any provider absent from the `[llm]` block of [`credentials.toml`](credentials-toml.md). `OPENAI_API_KEY`, `OPENROUTER_API_KEY`, and `GEMINI_API_KEY` / `GOOGLE_API_KEY` are also recognized. |
| `KVARN_API_KEY` | API key used by client commands (`jobs`, `feedback`, `queue`). |
| `KVARN_NO_AUTH` | Disables API-key authentication on the orchestrator. Local development only. |
| `KVARN_IMAGE_VERSION` | Overrides the VM disk image version or semver range to resolve. |
| `KVARN_LOG_LEVEL` | `debug`, `info`, `warn`, or `error`. Defaults to `info` (`debug` under `DEVELOPMENT=true`). |
| `DEVELOPMENT` | `true` selects the colorized text log handler instead of JSON. |
| `KVARN_SCHED_*` | Scheduler pool overrides; see [`orchestrator.toml`](orchestrator-toml.md). |
| `KVARN_OTEL_METRICS_ENABLED`, `KVARN_OTEL_EXPORTER_OTLP_ENDPOINT`, `KVARN_OTEL_SERVICE_NAME` | OpenTelemetry metrics export. |

## Precedence

Where the same setting exists in more than one place, the general rule is
**command-line flag > TOML file > built-in default**, and within the TOML files
**project > forge or user defaults > built-in**. The exact cascade is documented
with each setting:

- Pull-request behavior: [`forges.toml`](forges-toml.md#resolution-order)
- Cost caps and validation retries: [`agents.toml`](agents-toml.md#resolution-order)
- Concurrency caps: [`orchestrator.toml`](orchestrator-toml.md#per-tenant-caps)
