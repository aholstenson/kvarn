# `agents.toml`

Model classes for the coding agent, the per-agent overrides layered on them,
and the user-level defaults that projects inherit for cost and retry limits.

Default location `~/.config/kvarn/agents.toml`, overridable with
`--agents-file`. Re-read on every request.

## Model classes

Agents do not name models; they name a **class**, a capability tier configured
once and shared by everything that asks for it. Three classes exist:

| Class | Used for | Built-in default |
| --- | --- | --- |
| `coding-agent` | The main agent loop — the balanced default. | `anthropic/claude-sonnet-4-6`, effort `medium`, 16384 output tokens, 100 steps. |
| `coding-agent-fast` | High-volume, shallow work where breadth of search beats depth of reasoning (the `explore` sub-agent). | `anthropic/claude-haiku-4-5`, effort `none`, 8192 output tokens, 100 steps. |
| `coding-agent-reasoning` | Work worth thinking hard about and run rarely enough to pay for it (the `plan` sub-agent). | `anthropic/claude-sonnet-4-6`, effort `high`, 16384 output tokens, 50 steps. |

```toml
[models.coding-agent]
model = "anthropic/claude-sonnet-4-6"
reasoning_effort = "medium"
max_output_tokens = 16384

[models.coding-agent-fast]
model = "anthropic/claude-haiku-4-5"
max_output_tokens = 8192

[models.coding-agent-reasoning]
model = "anthropic/claude-opus-4-6"
reasoning_effort = "high"
```

| Key | Type | Notes |
| --- | --- | --- |
| `model` | string | Provider-qualified model ID, e.g. `anthropic/claude-sonnet-4-6`. |
| `reasoning_effort` | string | One of `none`, `low`, `medium`, `high`. |
| `max_output_tokens` | int | Cap on output tokens per request. |
| `max_steps` | int | Cap on tool-call steps per agent run. |

Each key you set replaces the built-in value for that class; keys you leave out
keep theirs. A `[models.<alias>]` block naming something other than the three
classes above is an error, so a typo is reported rather than ignored.

`--model` on `kvarn run` and `kvarn orchestrator` selects which model the
`coding-agent` class resolves to for that invocation.

## Per-agent overrides

A sub-agent runs on the class it declares — `explore` on `coding-agent-fast`,
`plan` on `coding-agent-reasoning`. An `[agents.<name>]` block changes that for
one agent without disturbing the class or any other agent using it:

```toml
# Investigate with the balanced model instead of the fast one.
[agents.explore]
class = "coding-agent"

# Plan on a specific model, and give it more room than the class allows.
[agents.plan]
model = "openai/gpt-5"
reasoning_effort = "high"
max_steps = 80
```

`class` picks the tier; the model keys from the table above are then applied on
top of whichever class was selected. Setting `model` bypasses the class's model
without bypassing the rest of its settings. Agent names are `explore` and
`plan`; any other name is an error.

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

`warn_threshold` resolves `defaults` → built-in.

`report_cost_on_pr` and `report_worklog_on_pr` used to live in this file's
`[defaults]` block. They now resolve with the pull-request content they gate;
see [`forges.toml`](forges-toml.md#pull-request-content). The spelling here is
still read as the lowest layer, and kvarn logs a warning naming this file when
it takes a value from it.

Each layer is consulted per field, so setting only `max_cost_usd` on a project
leaves its `max_validation_retries` inherited.
