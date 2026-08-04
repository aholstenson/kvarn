# Configure a repository

A repository tells Kvarn how to build itself with a `kvarn.yml` at its root. The
full field list is in the [`kvarn.yml` reference](../reference/kvarn-yml.md);
this guide is the path from empty file to one you trust.

## Pick an environment

Either Nix packages:

```yaml
dependencies:
  nixpkgs:
    - go
    - nodejs
```

or an OCI image:

```yaml
image: node:22-alpine
```

The two are mutually exclusive — in an `image:` job, steps run via `podman exec`
where host-installed Nix binaries are invisible. Choose Nix when you want a
declarative toolchain; choose an image when the project already has one and you
want the same environment CI uses.

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

## Iterate with `kvarn test`

```sh
kvarn test          # boot a VM, install deps, run setup + validation
kvarn test -v       # include output from passing steps
kvarn test --logs   # add VM and orchestration logs
```

`kvarn test` never invokes the agent, so it is the fast loop for getting a
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

To test a project that uses secrets locally, pass `--project` so `kvarn test`
and `kvarn run` know whose secrets to look up:

```sh
kvarn test --project my-project
```

## Try the agent against it

Once `kvarn test` is green, exercise the agent locally before involving the
orchestrator:

```sh
kvarn run --diff "Add a test for the retry path"    # print a diff, change nothing
kvarn run --apply "Fix the failing tests"           # copy changes back
```

Read-only modes (`--mode review`, `--mode research`) ignore both flags and write
their answer to stdout.
