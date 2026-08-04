# Manage API keys

Clients of the orchestrator authenticate with an API key sent as
`Authorization: Bearer <token>`. Authentication is on by default, and **with no
keys configured every request is denied** — so creating the first key is part of
bringing a host up.

Keys live in [`apikeys.toml`](../reference/apikeys-toml.md) and are managed with
`kvarn key`, which edits the file directly. No running orchestrator is needed,
and a running one picks up the change on its next request.

## Create a key

```sh
# Scoped to one project.
kvarn key create --name ci --projects my-project

# Several projects.
kvarn key create --name team --projects proj-a,proj-b

# Every project, expiring in 30 days.
kvarn key create --name admin --projects '*' --expires 720h

# Every project, and allowed to act on the orchestrator itself.
kvarn key create --name ops --projects '*' --capability host
```

The token is printed **once**. Only its hash is stored, so a lost token cannot
be recovered — revoke it and create another.

`--expires` takes an RFC3339 timestamp or a Go duration.

## Use a key

```sh
export KVARN_API_KEY=kvarn_…
kvarn jobs start my-project "Fix the failing tests"
```

Or pass `--api-key` explicitly. Both work for `jobs start`, `feedback` and
`cancel`.

## Scope

A key's `projects` list is checked on every project-scoped RPC — starting a job,
submitting feedback, cancelling, and reading or watching sessions. Scope keys to
what their holder actually needs: a CI system that only files jobs for one
repository has no reason to be able to read another project's session history.

## Capabilities

A key's projects say which work it may reach. Its capabilities say whether it
may act on the orchestrator itself — and `--projects '*'` grants only the first.
A wildcard exists so one key can drive every project, which is what a CI bot
needs; it is not a claim to speak for the host.

```sh
kvarn key create --name ops --projects '*' --capability host
```

`host` is the only capability today. It covers draining the orchestrator and a
bulk cancel that names no project. Most keys should have none.

Two things do not need a capability. A key scoped to named projects reaches only
its own work whatever filter it passes. And a caller on the orchestrator's own
host uses the [local control socket](run-the-orchestrator.md#the-host-local-control-socket),
where the filesystem authenticates instead of a key — so operating a host you
already own never requires minting a key for yourself.

## Retire a key

```sh
kvarn key list
kvarn key disable <key-id>   # reject it, keep the record
kvarn key revoke <key-id>    # delete it entirely
```

Prefer `disable` when you may still want to know the key existed; `revoke` when
you want it gone. Both take effect on the next request.

## Cap what a key may consume

A key can carry its own concurrency limits, which apply on top of the
per-project ones — useful when one client (a bulk importer, an experiment)
should not be able to fill the host on its own:

```toml
[abc123keyid]
name = "bulk-importer"
hash = "…"
projects = ["my-project"]
created = 2026-01-15T09:41:02Z
max_jobs = 2
max_memory = "16G"
```

Unset fields inherit `[scheduler.per_key]` from
[`orchestrator.toml`](../reference/orchestrator-toml.md#per-tenant-caps). See
[Tune host capacity](tune-host-capacity.md).

## Turning auth off

`--no-auth` (or `KVARN_NO_AUTH=1`) disables authentication entirely. Use it for
local development only. There is no middle setting: with auth off, anyone who
can reach the port can start jobs against any project.

The orchestrator does not terminate TLS. A bearer token sent over cleartext is a
leaked token, so anything reachable beyond localhost belongs behind a
TLS-terminating reverse proxy.
