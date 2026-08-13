# Run the orchestrator

Take a host from nothing to a first pull request.

## Before you start

- A machine with virtualization: **macOS** (Apple Virtualization) or **Linux**
  (KVM/QEMU). No other platform is supported.
- **git ≥ 2.26** on `PATH`. The orchestrator checks this at startup and refuses
  to boot without it.
- The `kvarn` binary on `PATH` — see [Install](../../README.md#install).
- A model-provider API key, either in `~/.config/kvarn/credentials.toml`:

  ```toml
  [llm.anthropic]
  api_key = "sk-ant-..."
  ```

  or in the environment of the process you will start:

  ```sh
  export ANTHROPIC_API_KEY=sk-ant-...
  ```

The VM disk image downloads automatically the first time it is needed. To do it
up front — or to check what will be used — run `kvarn image pull`.

## 1. Configure a forge and its credential

The forge is where branches are pushed and pull requests are opened. In
`~/.config/kvarn/credentials.toml`:

```toml
[credentials.github]
token = "ghp_..."
```

And in `~/.config/kvarn/forges.toml`:

```toml
[forges.github]
type = "github"
credential = "github"
```

GitHub Apps and plain Git remotes are covered in
[Connect a forge](connect-a-forge.md).

## 2. Add a project

In `~/.config/kvarn/projects.toml`:

```toml
[projects.my-project]
repo = "owner/repo"
default_branch = "main"
forge = "github"
```

The table name is what clients pass to `kvarn jobs start`.

## 3. Add a `kvarn.yml` to the repository

Kvarn needs to know how to build and validate the project:

```yaml
dependencies:
  nixpkgs:
    - go

setup:
  steps:
    - name: Download modules
      run: go mod download

validation:
  required:
    - name: Test
      run: go test ./...
```

Check it works before wiring anything else up — from a clone of that repository:

```sh
kvarn local test
```

This boots a VM, installs dependencies and runs setup, health checks and
validation, without invoking the agent. See
[Configure a repository](configure-a-repository.md).

## 4. Create an API key

Authentication is on by default, and with no keys configured every request is
denied. Create one before starting the server:

```sh
kvarn key create --name laptop --projects my-project
```

The token is printed once. See [Manage API keys](manage-api-keys.md).

## 5. Start the orchestrator

```sh
kvarn orchestrator --addr :8080
```

On startup it verifies git, opens the session database, reconciles any sessions
left non-terminal by a previous run, prunes expired session history, and begins
warming repository mirrors in the background.

The orchestrator speaks cleartext HTTP/2 and does not terminate TLS. If it is
reachable over a network, put it behind a TLS-terminating reverse proxy — a
bearer token in cleartext is a leaked token.

### The host-local control socket

Alongside the network listener the orchestrator serves a unix socket at
`~/.config/kvarn/orchestrator.sock`, mode `0600` inside a `0700` directory.

It serves the same API. What differs is how a caller proves who they are: on the
socket the filesystem decides, so no key is involved. Reaching it already means
holding the account that owns the orchestrator's config, its sessions database
and its API key file — someone with all of that cannot be meaningfully
restricted, so a local caller is treated as the host's operator and gets every
capability. The calling process's uid and pid are recorded on each audit line.

This is why stopping or inspecting your own orchestrator never requires minting
a key first. Commands prefer the socket automatically when one exists and no
`--addr` was given, so on the orchestrator's own host they work with no flags:

```sh
kvarn queue           # the socket
kvarn queue --addr http://kvarn.internal:8080   # the network listener
```

A loopback TCP port would not do: "the request came from 127.0.0.1" is not an
authentication claim, since any local process can make it and so can an SSRF or
a rebound browser tab. Socket permissions are enforced by the kernel and cannot
be reached from off the host at all.

`--local-socket` moves it; `--no-local-socket` turns it off, leaving the network
listener as the only way in. Clients read `KVARN_LOCAL_SOCKET` for a moved path.

## 6. Send a job

From any machine that can reach it:

```sh
export KVARN_API_KEY=kvarn_…
kvarn jobs start --addr http://kvarn.internal:8080 my-project "Fix the failing tests" --watch
```

On the orchestrator's own host, drop both — the local socket is used
automatically.

The orchestrator clones the repository, loads its `kvarn.yml`, runs setup in a
fresh VM, invokes the agent, validates the result, pushes a branch, and opens a
pull request. `--watch` streams the session to your terminal until it reaches a
terminal state; without it the command prints the session ID and returns.

To stop a run early:

```sh
kvarn jobs cancel <session-id> --reason "wrong branch"
```

## Next steps

- [Tune host capacity](tune-host-capacity.md) — before you point real traffic at it.
- [Control job costs](control-job-costs.md) — the built-in budget is $5 per job.
- [Speed up job startup](speed-up-job-startup.md) — mirrors and caches.
- [Manage secrets](manage-secrets.md) — if the project's build needs credentials.
- [Take a host out of service](take-a-host-out-of-service.md) — before the first time you restart it.
