# `kvarn.yml`

The per-repository configuration file: how to build the project, what it may
reach on the network, and how to tell whether a change is good. It is read from
the repository root under the first of these names that exists: `kvarn.yml`,
`kvarn.yaml`, `.kvarn.yml`, `.kvarn.yaml`.

A machine-readable schema lives at [`kvarn.schema.json`](../../kvarn.schema.json)
in the repository root; point your editor at it for completion and validation.

Inside the VM, steps run as the unprivileged `kvarn` user with the repository
checked out at `/home/kvarn/workspace` (the working directory for every step).
The home directory is `/home/kvarn`.

## Top-level keys

| Key | Type | Purpose |
| --- | --- | --- |
| `dependencies` | map | Nix flake sources mapped to attribute names to install. |
| `vm` | object | VM sizing overrides. |
| `network` | object | Outbound egress allowlist and in-VM hostname aliases. |
| `cache` | object | Extra guest paths to persist across runs. |
| `environment` | map | Environment variables injected into every step. |
| `secrets` | list | Runtime secrets the project needs. |
| `setup` | object | Steps and health checks run before the agent. |
| `validation` | object | Steps run after the agent. |
| `modes` | map | Agent modes this repository defines, beside the built-in ones. |
| `pull_request` | object | What the pull requests, commits and comments a run produces should say. |
| `preview` | object | Preview environments: which hostnames are served from which ports, and what to run to bring them up. |

All keys are optional; a repository with no `kvarn.yml` gets a bare VM with no
setup and no validation.

## `dependencies`

```yaml
dependencies:
  nixpkgs:
    - go
    - nodejs
  nixpkgs/nixos-25.11:
    - python312
  github:owner/repo:
    - some-attr
```

Each key is a flake source, each value the list of attributes to install from
it. Accepted source forms:

| Form | Notes |
| --- | --- |
| `nixpkgs` | Resolves to the built-in default channel (`nixos-26.05`). |
| `nixpkgs/<channel>` | Pins a different channel, e.g. `nixpkgs/nixos-25.11` or `nixpkgs/nixos-unstable`. |
| `github:owner/repo` | |
| `gitlab:owner/repo` | |
| `git+https://…`, `git+ssh://…` | |
| `https://…`, `tarball+https://…` | |

Attribute names are validated conservatively (they are concatenated into a shell
command). The host of each non-`nixpkgs` source is added to the egress allowlist
automatically.

Declaring certain attributes brings more than the package: caching, egress
hosts, environment and, for a version manager like `mise`, the command that
installs what it pins. See [Registered tools](registered-tools.md) for the list
and what each one sets up.

## `vm`

```yaml
vm:
  cpus: 4
  memory: 8G
  disk: 32G
```

| Key | Default | Minimum |
| --- | --- | --- |
| `cpus` | `2` | `1` |
| `memory` | `4G` | `2G` |
| `disk` | `16G` | `4G` |

Sizes accept `M`, `MiB`, `G`, `GiB` suffixes and whole numbers only. These
figures are also what the job is charged against the host's admission pool, so
raising them lowers how many jobs run concurrently — see
[Tune host capacity](../how-to/tune-host-capacity.md).

## `network`

```yaml
network:
  allowed_hosts:
    - api.example.com
    - "*.internal.example.com"
    - 10.0.0.5
```

Outbound TCP (ports 80 and 443) is denied unless the host matches the
allowlist, on top of the defaults needed to fetch dependencies. Entries are
hostnames, IP addresses, or a `*.domain` wildcard that matches any subdomain.
Schemes, paths and ports are rejected.

A denied connection is closed without an answer, so the program inside the VM
reports a truncated download or a reset connection rather than a refusal. Kvarn
records the hostname and names it in whatever failure follows, so a job that
dies on "unexpected EOF" still tells you which host to add here.

Matching is per hostname, and a redirect is a new connection to a new host: a
download that starts at an allowed host and redirects to a CDN needs the CDN
allowed too.

### `network.host_aliases`

```yaml
network:
  host_aliases:
    dev-shop.example.local: 127.0.0.1
    "*.dev.example.local": 127.0.0.1
```

Names a project's development hostnames resolve to inside the VM, in place
before any step runs. A server listening on `127.0.0.1` in the VM is then
reachable as `http://dev-shop.example.local`, which is what a project needs when
its dev tooling routes by hostname (virtual hosts, per-tenant subdomains,
cookies scoped to a domain).

A key is either one literal hostname or a `*.domain` wildcard matching any
subdomain of that suffix — `*.dev.example.local` covers `shop.dev.example.local`
and `a.b.dev.example.local`, but not `dev.example.local` itself. Values must be
IP addresses. Where two entries could answer the same name the more specific one
wins: an exact entry beats any wildcard, and a longer wildcard suffix beats a
shorter one, so a single subdomain can be pointed elsewhere without giving up
the wildcard covering its siblings.

Whether a name is served from `/etc/hosts` or by kvarn's DNS forwarder is an
implementation detail — exact names get both, wildcards can only be a DNS answer
— but it explains one thing worth knowing: a program that resolves names itself
rather than through the C library still sees the wildcards, because they are a
real DNS answer.

Loopback addresses are the point of the feature and stay entirely inside the VM;
that traffic never reaches the egress proxy. Mapping a name to a non-loopback
address is allowed but changes nothing about egress control — those packets
still go through the proxy, which judges them by hostname, so the name also
needs an `allowed_hosts` entry.

A container a step starts on the VM's network namespace inherits the same
resolver, so a dev server on loopback is reachable by name whether it was
started inside that container or beside it.

## `cache`

[Registered tools](registered-tools.md) (language package managers and build
caches Kvarn knows about) are cached automatically and need no `cache:` block.
These fields exist for unregistered tools or custom keying.

```yaml
cache:
  paths:
    - /home/kvarn/.cache/go-build
  entries:
    - path: ~/.cache/custom-tool
      lockfiles:
        - package-lock.json
      bucket: custom-tool
```

`cache.paths` are unkeyed (write-once) guest paths. `cache.entries` give one
path an explicit key:

| Field | Purpose |
| --- | --- |
| `path` | Guest path to cache. Required. |
| `lockfiles` | Files hashed into a content-addressed key; the cache is invalidated when they change. |
| `key` | A fixed key string. You own invalidation. |
| `bucket` | Groups related entries under one name. |

Setting neither `lockfiles` nor `key` gives an unkeyed cache, the same as a
`cache.paths` entry.

Path rules, applied to both forms:

- Absolute paths are used as-is.
- `~` and `~/foo` expand against `/home/kvarn`.
- Relative paths resolve under `/home/kvarn/workspace` and may not escape it
  with `..`.
- The workspace root itself is rejected — it is transferred separately.
- Anything under `/nix` is rejected; the Nix store is cached by its own
  mechanism.

## `environment`

```yaml
environment:
  CI: "true"
```

Injected into every step. Keys must be valid POSIX environment variable names;
values may not contain NUL or newline.

## `secrets`

```yaml
secrets:
  - API_TOKEN
  - name: DOCKERHUB
    scheme: basic
    hosts:
      - registry-1.docker.io
      - auth.docker.io
```

Declares the secrets the project needs; each is exposed inside the VM as an
environment variable of that name. The value comes from the orchestrator's
secret store (`kvarn secrets set`) — this file never contains one.

| Field | Purpose |
| --- | --- |
| `name` | Secret name. Must be a valid POSIX env-var name. |
| `scheme` | How a `managed` secret is applied to an outbound request: `bearer` (default), `basic`, or `oauth`. |
| `hosts` | Restricts substitution to these hosts. Same syntax as `network.allowed_hosts`. Empty means any allowlisted host. |

A bare string entry is shorthand for `{name: NAME}` — the `bearer` scheme over
any allowlisted host. `scheme` and `hosts` only matter for `managed` secrets,
whose real value stays on the host; see
[Manage secrets](../how-to/manage-secrets.md).

Names must be unique and must not collide with an `environment` key.

## `setup`

```yaml
setup:
  steps:
    - name: Install
      run: npm ci
      working_dir: frontend
      timeout: 10m
      retry: 1
  health_checks:
    - name: Check service
      run: curl -f http://localhost:3000/health
```

`steps` run in order before the agent starts and short-circuit on the first
failure. `health_checks` run only if every setup step passed.

## `validation`

```yaml
validation:
  required:
    - name: Tests
      run: npm test
  advisory:
    - name: Lint
      run: npm run lint
      paths:
        - "**/*.ts"
```

Runs after the agent finishes. Every step runs regardless of individual
failures. A failing `required` step fails the job (and may trigger a retry
pass — see [Control job costs](../how-to/control-job-costs.md)); a failing
`advisory` step is reported but does not affect the outcome.

## `modes`

```yaml
modes:
  review-pr:
    description: Review an open pull request and post the review as a comment.
    extends: review
    start: pull-request
    deliver:
      - pr-comment
    context:
      - pr-metadata
      - pr-diff
    prompt: |
      Hold the change to the house style: tests in the project's framework,
      comments that explain why rather than what changed.
```

Declares agent modes for this repository, keyed by the name a job selects with
`--mode`. They sit beside the six built-in modes (`auto`, `implement`, `fix`,
`feedback`, `review`, `research`) rather than replacing one: a definition
inherits from a mode via `extends` and overrides only the axes it names. Run
`kvarn modes list` in a checkout to see the resolved set.

A mode is a point in seven axes:

| Field | Purpose |
| --- | --- |
| `description` | One-line summary shown by `kvarn modes list`. |
| `extends` | Mode to inherit from — a built-in, or another mode in this file. Defaults to `auto`. |
| `prompt` | Instructions appended to the inherited prompt. It adds to the inherited guidance; there is no way to replace it. |
| `workspace` | `read-only` withholds the file-editing tools; `read-write` gives the full toolkit. Inherited. |
| `validation` | `skip`, `run`, or `require`. Defaults to `run` for a read-write mode and `skip` otherwise. |
| `deliver` | Where the result goes: `none`, `pr-comment`, `follow-up-commit`, `new-pull-request`. Inherited. |
| `start` | Where a run may begin: `branch`, `pull-request`, or `any`. Inherited. |
| `context` | Sections prepended to the task message: `none`, `original-task`, `pr-metadata`, `pr-diff`. Inherited. |

Every axis is inherited, so a mode extending `feedback` starts where `feedback`
starts — on a pull request — rather than widening to `any`. Narrow or widen it
by naming `start` explicitly. `validation` is the exception: it follows the
resolved `workspace` unless named, so overriding the workspace alone does not
leave a read-only mode running a write mode's validation policy.

`validation: run` feeds a failing required step back to the agent to fix, up to
the configured retry budget, and fails the job if it is still red. In a
read-only mode there is nothing to fix, so `run` records the outcome and lets
the run finish. `validation: require` fails the job on the first failing
required step with no retry — which is what makes a read-only "test this pull
request" mode report an honest verdict rather than a green one. A failing
`require` run still delivers to `pr-comment` before it fails, so the verdict
reaches the pull request; it does not deliver commits, since the run has just
established that what it produced does not pass.

Validation steps that declare `paths` are gated on the run's own diff. A
read-only mode has no diff, so every step runs: gating them on an empty diff
would skip each path-scoped step and report the pass those skips add up to.

`deliver` accepts more than one sink. They fire in a fixed order —
`new-pull-request`, then `follow-up-commit`, then `pr-comment` — so a comment
lands on the pull request the same run just opened. A commit sink already posts
its own comment carrying the summary and work log, so an explicit `pr-comment`
alongside one is skipped rather than posted twice. A delivery that fails, fails
the job: work that never left the host is not a success.

`new-pull-request` in a run started against an existing pull request commits
onto that pull request instead. Naming one asks for the work to land there, and
a second pull request would only target the first one's head branch.

A mode with `deliver: [none]` leaves its output in the session, where
`kvarn jobs result <session-id>` reads it. `context: [none]` is the same idea
for the context pack. Write those rather than an empty list: `deliver: []` is
rejected, because a definition supplied with a request cannot tell an empty list
from an absent one, and an absent one inherits.

Rejected at load time: a name that shadows a built-in, an `extends` that names
nothing, a cycle of `extends`, a value outside an axis's vocabulary, an empty
`deliver` or `context` list, `none` combined with another sink or block, both
commit sinks together, a commit sink in a read-only mode, and `follow-up-commit`
in a mode that can only start from a branch. Names must be lowercase
alphanumerics separated by single hyphens.

Rejected at submission, or as soon as the run resolves a mode the repository
defines: a mode that cannot deliver from where the run begins. A mode that
delivers a follow-up commit or a comment needs a pull request to act on, so
submitting one without a pull request reference is refused up front rather than
after a clone, a VM boot and a full agent run.

A mode definition is read from the repository the run works on, which for a job
continuing a pull request is that pull request's head branch. A branch can
therefore change what a mode it is reviewed under is allowed to do. This is the
same trust boundary `setup.steps` already sits on — both execute what the branch
says — so treat a named mode as a property of the branch under review, not a
guarantee the reviewer controls.

A caller that needs a mode this file does not define can supply one with the
request instead; see `kvarn jobs start --mode-spec` in
[CLI reference](cli.md).

## `pull_request`

Steers what a run writes: the commit subject, the shared body that becomes both
the commit message body and the pull request description, and the comment a
delivery posts.

```yaml
pull_request:
  title:
    instructions: |
      Use Conventional Commits: type(scope): subject.
      Types: feat, fix, chore, docs, refactor, test, build.
    max_length: 72
  body:
    instructions: Write for a reviewer who has not seen the task.
    sections:
      - name: Testing
        description: Which commands ran and what they reported. Say "not run"
                     and why if none were.
        required: true
      - name: Risks
        description: Anything a reviewer should look at twice.
  comment:
    instructions: Lead with the verdict, then the detail.
```

| Key | Type | Notes |
| --- | --- | --- |
| `title.instructions` | string | Added to the conventions the summary is written under. |
| `title.max_length` | int | Character budget for the subject line. Defaults to 72. |
| `body.instructions` | string | As above, for the body. |
| `body.sections` | list | Headings the body must carry; see below. |
| `comment.instructions` | string | How the written result should read when it is posted as a comment. |
| `comment.sections` | list | Headings that result should use. |
| `modes` | map | Overrides the block above for one job mode. |

### Sections

Each entry has a `name` (the level-2 heading, required and unique within its
list), an optional `description` of what belongs under it, and `required`.

`body.sections` are filled in by a structured call, so kvarn can check them: a
required section that comes back empty is asked for a second time, and if it is
still empty the heading is rendered as `_(not provided)_` rather than dropped.
Sections render in the order they are declared. Anything the agent returns that
was not asked for is discarded.

`comment.sections` are a request in the prompt rather than a checked structure.
A comment is written as the agent's own reply, not through a structured call, so
kvarn cannot verify the headings came back or reorder them.

### Per-mode overrides

`pull_request.modes.<mode>` layers on top of the block above it, so a mode adds
to the repository's conventions instead of restating them:

```yaml
pull_request:
  body:
    instructions: Write for a reviewer who has not seen the task.
  modes:
    implement:
      body:
        instructions: Also list any new dependency and why it is needed.
        sections:
          - name: Migration
            description: What an operator must do when deploying this.
```

Instructions concatenate, the top-level ones first. Sections merge by name: an
entry replaces the top-level entry it shares a name with, keeping that entry's
position, and a name the mode introduces is appended.

### What this block cannot set

Body footers, the comment headers, commit trailers, whether a comment carries the
work log or the cost, and how much of the request that started the run it quotes
back (`quote_task`) are configured by the operator in
[`forges.toml`](forges-toml.md) and [`projects.toml`](projects-toml.md). This
file is read from the branch a job runs on, the same trust boundary `modes` and
`setup.steps` sit on, so a run that could set them would be able to rewrite its
own attribution — or suppress the record of what it was asked to do.

Operator instructions are not replaced by anything here. Both sets reach the
summary, labelled by origin, and the repository's read last.

## `preview`

Declares a [preview environment](../how-to/preview-environments.md): a
long-lived VM pinned to a branch, reachable over HTTP at a stable hostname,
booted on demand and stopped when it goes idle.

The operator owns the domain and the repository owns the shape. This block says
which hostnames are served from which ports and what to run; the base domain
comes from the orchestrator's [`[preview]` section](orchestrator-toml.md#preview)
or the project's override.

```yaml
preview:
  sites:
    web:    { port: 3000, host: "{ref}.{domain}" }
    assets: { port: 8080, host: "assets-{ref}.{domain}" }
  serve:
    - { name: Web, run: npm start }
    - { name: Assets, run: npm run assets }
  ready:
    - { name: Web up, run: "curl -fsS http://localhost:3000/healthz" }
```

`host` defaults to `{ref}.{domain}`, so the single-site case is just:

```yaml
preview:
  sites:
    web: { port: 3000 }
  serve:
    - { name: Web, run: npm start }
  ready:
    - { name: Web up, run: "curl -fsS http://localhost:3000/healthz" }
```

### `preview.sites`

A map of site name to the address it names: one hostname, and the port behind
it. Site names are lowercase alphanumerics separated by single hyphens.

| Field | Type | Notes |
| --- | --- | --- |
| `port` | int | Required. The guest port the server listens on. Several sites may share one. |
| `host` | string | Hostname pattern. Defaults to `{ref}.{domain}`. Must be unique across sites. |

Hostnames are what route, so they are what must be unique; ports need not be.
Several sites naming one port is one virtual-hosting server answering under
several names — ingress passes it the hostname the browser asked for, so it can
tell the requests apart.

Three placeholders are available:

- `{ref}` — the branch, reduced to exactly one DNS label: lowercased, with
  anything that is not a letter or digit collapsed to a hyphen. When that is not
  a faithful rendering of the branch — `feat/login` and `feat-login` would
  otherwise collide, and a long branch has to be shortened — a short digest of
  the original is appended, so the label stays deterministic, stays under 63
  bytes and never names two branches the same thing.
- `{pr}` — the pull request the preview is of. A preview that is not of a pull
  request has nothing to put here, so a site using it fails to resolve; give the
  number with `kvarn preview up --pr` when starting one by hand.
- `{domain}` — the base domain configured for the project.

Naming sites by `{pr}` is what lets previews start themselves: the operator's
[`auto_start`](projects-toml.md#auto_start) patterns match the same names, so a
request for `pr-12.preview.example.com` boots a preview of pull request 12.

```yaml
preview:
  sites:
    web: { port: 3000, host: "pr-{pr}.{domain}" }
    api: { port: 4000, host: "api-pr-{pr}.{domain}" }
```

**A pattern must end in `{domain}` or `.{domain}`.** Anything else is rejected
when the file is read. Without that rule a `kvarn.yml` on any branch could write
`host: "admin.example.com"` and have the orchestrator serve a name in the
operator's zone, which is not the repository's to claim.

### `preview.setup`

Ordinary [steps](#step-fields), run to completion in order before the serve
steps, with the preview's own site URLs already in their environment. This is
where anything that has to know the hostnames the preview answers on belongs —
registering domains with an application, seeding a tenant, pointing a container
stack that setup already started at its URLs — because `setup.steps` runs long
before a hostname exists.

They run in the same shell as the ready checks, so environment they export
carries into those. A step that fails, after its `retry` attempts, fails the
boot: a preview whose domains were never configured is not one worth serving.

```yaml
preview:
  sites:
    web: { port: 3000 }
  setup:
    - { name: Domains, run: ./bin/configure-domains }
```

### `preview.serve`

The long-lived commands that bring the preview up. They are started in order
after `preview.setup`, each in its own process group under the same unprivileged
user every step runs as, and supervised for the preview's whole life; stopping
the preview signals the group.

A repository whose servers are already running by the time setup finishes — a
container stack brought up by `setup.steps`, say — declares none at all, and
`ready` is what decides whether they answer.

| Field | Type | Notes |
| --- | --- | --- |
| `name` | string | Required. Identifies the process in logs and events. Unique across the serve steps. |
| `run` | string | Required. Executed via `sh -c`. Should stay in the foreground. |
| `working_dir` | string | Relative to the workspace root. |
| `env` | list | Additional environment variable names to forward into the process. |

Which command binds which port is the repository's business — kvarn does not
ask, and a step is free to start something that binds nothing at all. What the
sites declare is where traffic is sent; `ready` is what decides whether anything
is listening there.

The bind address is the repository's business too. Traffic is carried into the
VM from inside it, so a server on the guest's own loopback is reached as
readily as one on all interfaces: a container published on `127.0.0.1:3000` and
a dev server bound to `localhost` both work unchanged.

One command may therefore serve every site, whether they share a port or not:

```yaml
preview:
  sites:
    web:    { port: 80 }
    assets: { port: 80, host: "assets-{ref}.{domain}" }
  serve:
    - { name: Web, run: npm start }
```

Before any preview step runs, each site's resolved URL is exported as
`KVARN_PREVIEW_URL_<SITE>` — `KVARN_PREVIEW_URL_WEB`,
`KVARN_PREVIEW_URL_ADMIN_UI` — with the site name uppercased and hyphens turned
into underscores. Every setup step, serve step and ready check gets all of them,
so a server hosting several sites has the names it needs to route between them.
Read them for anything that has to be an absolute URL: asset prefixes, OAuth redirect URIs, CORS origins. A
server that hardcodes `http://localhost:3000` instead is the most common way a
preview ends up half-broken.

### `preview.ready`

Ordinary [steps](#step-fields) that decide when the preview may take traffic.
They run in order after the serve commands start, and each is retried for about
two minutes — a server takes a moment to bind its port, and failing on the first
attempt would fail a preview that is merely still starting.

Requests arriving before the checks pass get a holding page rather than a
connection error.

## Step fields

Used by `setup.steps`, `setup.health_checks`, `validation.required`,
`validation.advisory`, `preview.setup` and `preview.ready`.

| Field | Type | Notes |
| --- | --- | --- |
| `name` | string | Required. Identifies the step in output and session events. |
| `run` | string | Required. Executed via `sh -c`. |
| `working_dir` | string | Relative to the workspace root. |
| `timeout` | int or duration | Seconds as an integer, or a Go duration string (`30s`, `10m`, `1h30m`). Omitted or `0` means no timeout. |
| `retry` | int | Additional attempts on failure, max 10. **Setup steps only.** |
| `paths` | list | Doublestar globs. **Validation steps only** — the step runs only when a changed file matches. |

## Checking a file

`kvarn local test` boots a VM against the current working tree and runs
dependencies, setup, health checks and validation without invoking the agent.
That is the fastest way to confirm a `kvarn.yml` is correct; see
[Configure a repository](../how-to/configure-a-repository.md).
