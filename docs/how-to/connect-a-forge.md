# Connect a forge

A forge is where Kvarn pushes branches and opens pull requests. Forges are
defined once in [`forges.toml`](../reference/forges-toml.md) and referenced by
name from each project.

## GitHub with a personal access token

`~/.config/kvarn/credentials.toml`:

```toml
[credentials.github]
token = "ghp_..."
```

`~/.config/kvarn/forges.toml`:

```toml
[forges.github]
type = "github"
credential = "github"
```

The token needs to be able to push to the repository and to open pull requests
on it.

## GitHub with an App installation

Preferred for anything shared: the credential is scoped to an installation
rather than to a person, and its tokens are short-lived.

```toml
[credentials.github-app]
app_id = "12345"
private_key_path = "/path/to/private-key.pem"
installation_id = "67890"
```

```toml
[forges.github]
type = "github"
credential = "github-app"
```

An installation token expires after an hour, which is shorter than some jobs
run. Kvarn resolves the credential at each operation rather than capturing one
at job start, so the push at the end of a long run mints a fresh token. It still
mints one eagerly at startup, so a misconfigured App fails where it is
configured rather than an hour into a job.

## A plain Git remote

```toml
[credentials.git-creds]
username = "user"
password = "..."
# or: token = "..."
# or: ssh_key_path = "/path/to/key"
```

```toml
[forges.git]
type = "git"
credential = "git-creds"
```

Plain Git clones and pushes but **cannot open pull requests** — a job ends with
a pushed branch. Feedback runs, which continue work on an existing PR, need a
forge that understands pull requests.

Match the credential to the remote's scheme: a token against an `ssh://` URL, or
an SSH key against an `https://` URL, is rejected outright rather than silently
falling back.

## Point a project at it

```toml
[projects.my-project]
repo = "owner/repo"
default_branch = "main"
forge = "github"
```

A project with no `forge` still runs jobs; it just has nowhere to push the
result.

## Shape the pull requests

Branch prefix, labels and commit author resolve through four layers, highest
first: the project, the named forge, the `[defaults]` block, then Kvarn's
built-ins.

Put the values that are true everywhere in `[defaults]`:

```toml
[defaults]
branch_prefix = "agent"
labels = ["automated", "kvarn"]
commit_author_name = "Kvarn Bot"
commit_author_email = "bot@example.com"

[forges.github]
type = "github"
credential = "github"
```

And override per repository in `projects.toml`, since one forge serves many
repositories with different conventions:

```toml
[projects.my-project]
repo = "owner/repo"
forge = "github"
branch_prefix = "bot"
labels = ["automated", "needs-review"]
```

`labels` is replaced, not merged: a layer that sets its own list does not
inherit the one below it.

## Run several forges

Nothing stops you defining more than one — a GitHub App per organization, say,
or GitHub for most projects and a plain Git remote for an internal mirror:

```toml
[forges.github-acme]
type = "github"
credential = "acme-app"

[forges.github-personal]
type = "github"
credential = "personal-token"
```

Each project names the one it uses. Credentials are never shared implicitly:
mirrors are keyed by project name rather than repository URL precisely so two
projects pointing at one URL with different credentials cannot end up reading
each other's objects.
