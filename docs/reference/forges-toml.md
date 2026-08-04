# `forges.toml`

Named forge instances — where branches are pushed and pull requests are opened —
plus the pull-request behavior shared across projects that use them.

Default location `~/.config/kvarn/forges.toml`, overridable with
`--forges-file`. Re-read on every request.

```toml
[forges.github]
type = "github"
credential = "github"
branch_prefix = "kvarn"
labels = ["kvarn"]
commit_author_name = "kvarn"
commit_author_email = "kvarn@noreply"
```

A project selects one by name with `forge = "github"` in
[`projects.toml`](projects-toml.md).

## Forge entries

| Key | Type | Notes |
| --- | --- | --- |
| `type` | string | `github` or `git`. Required. |
| `credential` | string | Name of an entry in [`credentials.toml`](credentials-toml.md). |
| `branch_prefix` | string | Prefix for generated branch names. |
| `labels` | list of strings | Labels applied to opened pull requests. |
| `commit_author_name` | string | Author on commits Kvarn creates. |
| `commit_author_email` | string | |

### `type = "github"`

Clones, pushes, opens pull requests, applies labels, and posts the work-log
comment. Requires a credential carrying either a personal access token or a
GitHub App installation.

### `type = "git"`

A plain Git remote. Clones and pushes but **cannot create pull requests** —
a job against a `git` forge ends with a pushed branch.

## Defaults

A `[defaults]` block sets values shared by every forge, so a value is only
repeated in the forges that differ:

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

The block accepts `branch_prefix`, `labels`, `commit_author_name` and
`commit_author_email`. It is optional.

## Resolution order

Each field resolves independently, highest precedence first:

1. The project's own value in [`projects.toml`](projects-toml.md#pull-request-behavior).
2. The named forge entry in this file.
3. The `[defaults]` block in this file.
4. The built-in values: `branch_prefix = "kvarn"`, `labels = ["kvarn"]`,
   `commit_author_name = "kvarn"`, `commit_author_email = "kvarn@noreply"`.

`labels` is replaced wholesale, never merged — a layer that sets its own
`labels` does not inherit the list below it.
