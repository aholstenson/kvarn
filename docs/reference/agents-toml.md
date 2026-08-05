# `agents.toml`

Model aliases for the coding agent, and the user-level defaults that projects
inherit for cost and retry limits.

Default location `~/.config/kvarn/agents.toml`, overridable with
`--agents-file`. Re-read on every request.

## Model aliases

```toml
[models.coding-agent]
model = "anthropic/claude-sonnet-4-6"
reasoning_effort = "medium"
max_output_tokens = 16384

[models.coding-agent-small]
model = "anthropic/claude-haiku-4-5"
max_output_tokens = 8192
```

| Key | Type | Notes |
| --- | --- | --- |
| `model` | string | Provider-qualified model ID, e.g. `anthropic/claude-sonnet-4-6`. |
| `reasoning_effort` | string | One of `none`, `low`, `medium`, `high`. |
| `max_output_tokens` | int | Cap on output tokens per request. |

Two aliases are meaningful to the agent:

| Alias | Used for | Built-in default |
| --- | --- | --- |
| `coding-agent` | The main agent loop. | `anthropic/claude-sonnet-4-6`, effort `medium`, 16384 output tokens. |
| `coding-agent-small` | Cheaper sub-agents that don't need top-tier reasoning (e.g. exploration). | `anthropic/claude-haiku-4-5`, 8192 output tokens. |

An alias defined here replaces the built-in entry entirely. Aliases you leave
out keep their built-in configuration.

`--model` on `kvarn run` and `kvarn orchestrator` selects which alias serves as
the main coding agent for that invocation (default `coding-agent`).

Provider API keys are never read from this file. They live in the `[llm]`
block of [`credentials.toml`](credentials-toml.md), falling back to the
environment of the process running the agent — `ANTHROPIC_API_KEY`,
`OPENAI_API_KEY`, `OPENROUTER_API_KEY`, or `GEMINI_API_KEY` /
`GOOGLE_API_KEY`.

## Defaults

User-level cost and retry limits, inherited by every project that does not set
its own:

```toml
[defaults]
max_cost_usd = 10.0
warn_threshold = 0.8
report_cost_on_pr = true
max_validation_retries = 3

[defaults.jobs.review]
max_cost_usd = 1.0

[defaults.jobs.research]
max_validation_retries = 0
```

| Key | Type | Built-in | Notes |
| --- | --- | --- | --- |
| `max_cost_usd` | float | `5.00` | Hard budget per job; the run is cancelled when reached. |
| `warn_threshold` | float | `0.80` | Fraction of the budget at which the agent gets a soft warning. **User-level only** — there is no per-project knob. |
| `report_cost_on_pr` | bool | `true` | Include the cost section in the work-log PR comment. |
| `max_validation_retries` | int | `3` | Additional agent passes after a required validation step fails. |

`[defaults.jobs.<mode>]` narrows `max_cost_usd` and `max_validation_retries` to
one agent mode: a built-in (`auto`, `implement`, `fix`, `feedback`, `review`,
`research`) or one a repository defines under `modes:` in its
[`kvarn.yml`](kvarn-yml.md#modes).

## Resolution order

`max_cost_usd` and `max_validation_retries` resolve in five steps, highest
precedence first:

1. `projects.<name>.jobs.<mode>` in [`projects.toml`](projects-toml.md)
2. `projects.<name>`
3. `defaults.jobs.<mode>` in this file
4. `defaults` in this file
5. the built-in fallback

`report_cost_on_pr` resolves project → `defaults` → built-in.
`warn_threshold` resolves `defaults` → built-in.

Each layer is consulted per field, so setting only `max_cost_usd` on a project
leaves its `max_validation_retries` inherited.
