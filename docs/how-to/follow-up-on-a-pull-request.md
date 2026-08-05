# Follow up on a pull request

A job that opened a pull request is finished, but the review that follows is
not. `kvarn jobs start --pr-ref` continues work on an **existing** PR: it clones
that PR's head branch, runs the agent with your prompt as the task, pushes a
follow-up commit to the same branch, and posts a comment. No second PR is opened
and the title and body are left alone.

```sh
kvarn jobs start my-project --pr-ref 1234 "Handle the empty-input case and add a test for it"
```

Continuing a pull request is not a separate command, because it is not a
separate kind of work: the same job, run against a starting point that already
exists. Without `--pr-ref` the same command clones a branch and opens a new PR.
Everything else — modes, cost caps, watching, cancelling, retrying, the queue —
behaves the same either way.

## The PR reference

`--pr-ref` is in the forge's own format. For GitHub that is the PR number; Kvarn
itself treats it as opaque, and each forge interprets it.

## What the agent is given

The run's task is a context pack: the original task (omitted if that session has
been pruned), the current pull request, a best-effort diff (capped at 256 KiB),
and your prompt. The session stores your raw prompt, so `GetSession` shows what
was actually asked rather than the assembled pack.

`--pr-ref` selects the `feedback` mode by default, and nothing more: `--mode`
overrides it if you want, say, a `review` pass over the PR instead. Cost caps
and validation retries come from whichever mode ends up running — see
[Control job costs](control-job-costs.md):

```toml
[projects.my-project.jobs.feedback]
max_cost_usd = 3.0
```

## What gets rejected, and why

All of these are refused **before a session is created**, so a rejected request
leaves no trace:

| Rejection | Reason |
| --- | --- |
| No forge configured, or the ref is unreadable | Nothing to push to. |
| The PR is not open | |
| The PR head is in a fork | The head branch lives in another repository, so it is not in the clone kvarn makes; pushing to it would additionally need the maintainer-edit flag an org-scoped App installation token cannot use. |
| Another run is in flight for the same PR | Named in the error. Applies to modes that push, so two follow-up commits cannot race on one branch; two read-only runs on one PR are fine. |

Just before pushing, the PR head is re-read; if it moved underneath the run, the
run fails rather than pushing over someone else's work.

Startup reconciliation fails any session left non-terminal by a crash, so the
one-run-per-PR lock can never be stuck.

## Lineage

Continuing a pull request creates a **new session** — terminal states stay
terminal. It records `parent_session_id`, resolved by finding the oldest session
on the same PR. Nothing depends on that session still existing; retention may
have pruned it.

## Watching a run

`jobs start` prints the session ID and returns. Pass `--watch` to stream the
session to your terminal until it reaches a terminal state.

A session you did not follow at the time can be picked up later:
`kvarn jobs watch <session-id>` attaches to a run in progress, and
`kvarn jobs events <session-id>` replays the recorded history of one that has
already finished. `kvarn jobs list --pr-ref <ref>` shows every run against one
pull request, the original job and its follow-ups together.

A watcher that falls behind is disconnected rather than silently dropped, and
reconnects from the last sequence number it saw, replaying the gap from the
durable event log. That is why a slow terminal cannot produce a session
transcript with an invisible hole in it.

## Cancelling

```sh
kvarn jobs cancel <session-id> --reason "superseded by a manual fix"
```

The call cancels the job's context and returns; the run unwinds wherever it is —
queued, cloning, mid-agent — and its VM is torn down on the way out. You are not
made to wait for a VM to stop.

The outcome is `cancelled`, not `failed`, and the reason lands in the session's
message with the error left empty — a cancelled session is not an error. A
shutdown or a tripped cost cap stays a failure. A cancel that arrives after the
PR was submitted loses the race and the run is reported `completed`, because the
pull request exists.

## Session history

Sessions and their event logs are persisted by the orchestrator in SQLite at
`~/.config/kvarn/sessions.db` (`--sessions-db` to override). Terminal sessions
older than `[sessions].retention` — 30 days by default — are pruned at startup
and hourly, and their events cascade:

```toml
[sessions]
retention = "2160h"   # 90 days; "0" keeps them forever
```

Durable history covers state changes, agent messages and tool use, step results,
cost, the created pull request, and VM info. High-volume telemetry — VM console
output, step stdout/stderr, transfer and cache progress — is streamed live to
watchers but not persisted.
