# Control job costs

Every job has a hard USD budget. The agent is warned as it approaches the cap
and the run is cancelled when it reaches it, so a task the agent cannot finish
costs a bounded amount rather than an open-ended one.

The defaults are already conservative: **$5.00 per job**, a warning at **80%**
of that, and **3** validation retries. Nothing is unlimited by default.

## Raise or lower the budget for a project

In [`projects.toml`](../reference/projects-toml.md):

```toml
[projects.my-project]
repo = "owner/repo"
max_cost_usd = 15.0
```

## Set a different budget per mode

Read-only modes are cheap and rarely need the full budget; an `implement` run on
a large codebase may need more:

```toml
[projects.my-project]
repo = "owner/repo"
max_cost_usd = 15.0

[projects.my-project.jobs.review]
max_cost_usd = 1.0

[projects.my-project.jobs.research]
max_cost_usd = 2.0
```

The mode is any name a job can run in: one of the six built-ins (`auto`,
`implement`, `fix`, `feedback`, `review`, `research`) or a mode the
repository defines in its [`kvarn.yml`](../reference/kvarn-yml.md#modes). A
mode of its own is how a workflow gets a cap of its own — a nightly
dependency bump kept to a dollar, say, without touching what `implement`
may spend.

## Set a house default for every project

In [`agents.toml`](../reference/agents-toml.md), so new projects inherit it
instead of silently taking the built-in $5:

```toml
[defaults]
max_cost_usd = 10.0
warn_threshold = 0.75

[defaults.jobs.review]
max_cost_usd = 1.0
```

`warn_threshold` is the fraction of the budget at which the agent receives its
soft warning. It is user-level only — there is no per-project knob.

The five-layer cascade (project mode → project → defaults mode → defaults →
built-in) is documented in
[`agents.toml`](../reference/agents-toml.md#resolution-order). Each field
resolves independently, so overriding one on a project leaves the rest inherited.

## Control validation retries

When a required validation step fails, Kvarn can hand the failure back to the
agent for another pass. Each pass costs money, and the budget is what actually
bounds the total, but retries are worth tuning per mode:

```toml
[projects.my-project]
max_validation_retries = 2

[projects.my-project.jobs.research]
max_validation_retries = 0
```

`0` disables retries. The built-in default is `3`.

For local runs, `kvarn run --max-validation-retries N` does the same; it
defaults to `0` there, since you are watching.

## Report cost on the pull request

```toml
[projects.my-project]
report_cost_on_pr = true
```

Adds a cost section to the work-log comment Kvarn posts on the PR. On by
default. Turn it off for repositories where the comment is noise, or where the
figure would be visible to people who shouldn't see it.

## What a cap trip looks like

A run cancelled by its budget ends as **`failed`**, not `cancelled` — the
distinction is deliberate. `cancelled` means a human asked for it via
`kvarn jobs cancel`; a tripped budget means the job did not achieve what it was asked
to. Both write a final cost snapshot to the session before it goes terminal.

## Choosing a model

Cost per job is also a function of which models run. Aliases are configured in
[`agents.toml`](../reference/agents-toml.md):

```toml
[models.coding-agent]
model = "anthropic/claude-sonnet-4-6"
reasoning_effort = "medium"
max_output_tokens = 16384
```

`coding-agent` is the main loop; `coding-agent-small` serves cheaper sub-agents
that do not need top-tier reasoning. Overriding `coding-agent-small` with a
smaller, faster model is usually the lowest-risk cost reduction available.
