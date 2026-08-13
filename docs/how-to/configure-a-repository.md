# Configure a repository

A repository tells Kvarn how to build itself with a `kvarn.yml` at its root. The
full field list is in the [`kvarn.yml` reference](../reference/kvarn-yml.md);
this guide is the path from empty file to one you trust.

## Pick an environment

Name the toolchain the project needs as Nix packages:

```yaml
dependencies:
  nixpkgs:
    - go
    - nodejs
```

They are installed into the VM before any step runs, so every step and the agent
find them on `PATH`. Anything else a project needs from a container — a database,
a service the tests talk to — a setup step can start itself with `podman` or
`docker compose`.

## Add setup and validation

```yaml
setup:
  steps:
    - name: Install
      run: npm ci
      timeout: 10m
      retry: 1

validation:
  required:
    - name: Tests
      run: npm test
  advisory:
    - name: Lint
      run: npm run lint
```

`setup.steps` run before the agent and stop at the first failure. `retry` is
worth setting on steps that fail for reasons unrelated to your code — a flaky
package mirror, say — and only applies to setup.

`validation.required` decides the job's outcome; `validation.advisory` is
reported but never fails a run. Every validation step runs even if an earlier
one failed, so one bad step doesn't hide the rest.

## Share the workspace with containers your steps start

Some projects bring up their own containers — a `docker-compose.yml` started by
a setup step, say — and bind-mount the workspace into them. Podman runs rootless
in the VM, so the user your steps run as appears as `root` inside those
containers, and every *other* user in them is mapped to an identity that owns
nothing in the workspace. A service running as `www-data` or `node` therefore
reaches your files through their group and other permission bits alone.

Kvarn keeps those bits open for everything it puts in the workspace, both the
files transferred from your checkout and the ones your steps create, so this is
usually invisible. It stops being invisible when a step tightens a mode itself:

```yaml
setup:
  steps:
    - name: Provision
      run: install -m 600 .env.example .env   # the container cannot read this
```

The same applies to directories, where the effect is easier to miss: a mode with
no group or other execute bit cannot be descended into, so nothing underneath it
is reachable either.

Files written from *inside* a container have the mirror-image problem. They
belong to that container's user, which the VM side cannot modify, so a later
step — or the agent — cannot change a path the container already wrote. Point
containers that generate their own state at a named volume rather than the
workspace.

## Iterate with `kvarn local test`

```sh
kvarn local test          # boot a VM, install deps, run setup + validation
kvarn local test -v       # include output from passing steps
kvarn local test --logs   # add VM and orchestration logs
```

`kvarn local test` never invokes the agent, so it is the fast loop for getting a
`kvarn.yml` right. It works against your current working tree with no
orchestrator, project entry or forge configured.

## Allow the network the build needs

Outbound traffic is blocked unless allowlisted, on top of the defaults needed to
fetch dependencies. If a step hangs or fails on a network call, that is usually
why:

```yaml
network:
  allowed_hosts:
    - api.example.com
    - "*.internal.example.com"
```

Entries are hostnames, IPs, or a `*.domain` wildcard — no scheme, path or port.

## Size the VM only when you need to

```yaml
vm:
  cpus: 4
  memory: 8G
  disk: 32G
```

Defaults are 2 vCPUs, 4G memory, 16G disk. These numbers are also what the job
is charged against the host's admission pool, so raising them reduces how many
jobs run at once. Raise them when a build genuinely needs it, not by habit.

## Cache what the build downloads

Tools Kvarn recognizes are cached automatically. Add a `cache:` block only for
tools it does not know about:

```yaml
cache:
  paths:
    - ~/.cache/my-tool
  entries:
    - path: ~/.cache/custom-tool
      lockfiles:
        - package-lock.json
      bucket: custom-tool
```

Use `lockfiles` when the cache should be invalidated by a dependency change;
plain `paths` when the content is append-only and never goes stale. The
workspace root and anything under `/nix` are rejected — both are handled by
their own mechanisms.

## Declare secrets the build needs

```yaml
secrets:
  - API_TOKEN
```

Declaring the name here is only half of it; the value is set on the host with
`kvarn secrets set`. See [Manage secrets](manage-secrets.md), which also covers
`managed` secrets — credentials the VM never actually receives.

To test a project that uses secrets locally, pass `--project` so the `kvarn
local` commands know whose secrets to look up:

```sh
kvarn local test --project my-project
```

## Try the agent against it

Once `kvarn local test` is green, exercise the agent locally before involving
the orchestrator:

```sh
kvarn local job --diff "Add a test for the retry path"    # print a diff, change nothing
kvarn local job --apply "Fix the failing tests"           # copy changes back
```

Read-only modes (`--mode review`, `--mode research`) ignore both flags and write
their answer to stdout.
