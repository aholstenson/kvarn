# `apikeys.toml`

API keys that authenticate clients of the orchestrator's external API. Written
and read with mode `0600`.

Default location `~/.config/kvarn/apikeys.toml`, overridable with
`--api-keys-file`. Re-read on every request, so creating, disabling or revoking
a key takes effect immediately with no restart.

**Create keys with [`kvarn key`](cli.md#kvarn-key)** rather than by hand — the
file stores a hash, not the token, and only the CLI can print the token. This
page documents the format for the fields the CLI does not set, and for reading
an existing file.

```toml
[abc123keyid]
name = "ci"
hash = "…"
projects = ["my-project"]
capabilities = []
created = 2026-01-15T09:41:02Z
expires = "2026-07-15T09:41:02Z"
disabled = false
max_jobs = 2
```

The TOML table name is the key ID.

| Key | Type | Notes |
| --- | --- | --- |
| `name` | string | Human-readable label, shown by `kvarn key list`. |
| `hash` | string | `sha256` of the token's secret part. Set by the CLI. |
| `projects` | list of strings | Projects this key may act on. `["*"]` means all. |
| `capabilities` | list of strings | Host-level actions this key may take. Empty for almost every key. |
| `created` | datetime | Set by the CLI. |
| `expires` | string | RFC3339 timestamp. Omitted means no expiry. |
| `disabled` | bool | Rejects the key while keeping it on record. |
| `max_jobs` | int | Concurrent jobs this key may hold. |
| `max_cpu` | int | Total vCPUs across its running jobs. |
| `max_memory` | size | e.g. `"32G"`. |
| `max_disk` | size | e.g. `"200G"`. |

The four caps default to `[scheduler.per_key]` in
[`orchestrator.toml`](orchestrator-toml.md#per-tenant-caps); an explicit `0`
means unlimited even when a host-wide default is set. They are independent of
the per-project caps — a job must fit under both.

## Capabilities

`projects` and `capabilities` are separate axes, and the split is deliberate.
`projects` says which work a key may reach; `capabilities` says whether it may
act on the orchestrator itself. `["*"]` grants the first and never the second —
a wildcard exists so one key can drive every project, which is what a CI bot
needs, and is not a claim to speak for the host.

| Capability | Grants |
| --- | --- |
| `host` | Requests that have no project to scope them to: draining the orchestrator, and a bulk cancel that names no project. |

An unrecognized name fails the read of that key rather than being ignored, so a
typo denies the key loudly instead of silently withholding authority its file
appears to grant. There is no wildcard: a key created today would otherwise gain
whatever authority is defined tomorrow.

`SetDrain` always needs it: taking the host out of service is not something any
project scope can express. A bulk cancel needs it only when the request names no
project *and* the key's scope is unbounded — a key scoped to named projects
reaches only its own work whatever filter it passes, so it claims nothing new.

## Token format

`kvarn_<keyid>_<secret>`.

The `kvarn_` prefix lets secret scanners recognize a leak. `keyid` is the lookup
handle and matches the table name here. `secret` is 160 bits from a CSPRNG. Both
components are lowercase unpadded base32, so neither can contain the `_`
delimiter. Only `sha256(secret)` is persisted.

## Scope enforcement

Clients send the token as `Authorization: Bearer <token>`. Every project-scoped
RPC (`StartJob`, `CancelJob`, `GetSession`, `WatchSession`, `ListSessions`)
checks the key's `projects` list.

`SetDrain` checks `capabilities` instead: it has no project to scope it to.
`CancelJobs` checks both — its project scope as usual, plus the capability when
the request names no project and the key's scope is unbounded.

Authentication is enforced by default. With auth on and **no keys configured,
every request is denied** — bootstrap with `kvarn key create`. `--no-auth` (or
`KVARN_NO_AUTH`) turns authentication off for local development.

The orchestrator speaks cleartext HTTP/2 (h2c) and does not terminate TLS. A
bearer token is only safe over an encrypted channel, so run it behind a
TLS-terminating reverse proxy whenever it is reachable over a network.

The orchestrator also serves a host-local control socket, which needs no key at
all; see [Run the orchestrator](../how-to/run-the-orchestrator.md#the-host-local-control-socket).
