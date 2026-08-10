# Registered tools

Some nixpkgs attributes need more than a binary on `PATH` to be useful in a
sandbox: a package manager writes to a cache directory that is worth keeping
between jobs, fetches from hosts the egress proxy denies by default, or reads an
environment variable to find its state. Kvarn keeps a registry of those details
and applies them when a project declares the attribute in
[`dependencies`](kvarn-yml.md#dependencies).

Nothing here is automatic in the sense of guessing: the trigger is always the
declaration. A repository with a `mise.toml` gets no mise handling until
`dependencies` names `mise`.

```yaml
dependencies:
  nixpkgs:
    - go        # brings the GOPATH and build caches, and the module proxy hosts
    - mise      # brings the shims, the tool cache, and `mise install`
```

Declaring an unregistered attribute is perfectly fine — it installs the package
and nothing else happens. Use [`cache`](kvarn-yml.md#cache) and
[`network.allowed_hosts`](kvarn-yml.md#network) to supply the same details by
hand.

## What an entry can carry

| Property | Effect |
| --- | --- |
| Cache paths | Guest directories saved after the job and restored before the next one. |
| Cache key | The files whose contents decide whether a restored cache is still valid. A cache whose key files are absent still works; it just keys on the tool and nixpkgs channel instead. |
| Hosts | Added to the egress allowlist before the VM boots. |
| Environment | Exported in every step and every shell the agent opens. |
| `PATH` | Directories prepended for every step and shell. |
| Provisioning | A command run once, after the environment is in place and before the first setup step. |

## The registry

| Attribute | Cached | Keyed by | Hosts | Environment and `PATH` |
| --- | --- | --- | --- | --- |
| `go` | `~/go`, `~/.cache/go-build` | `go.sum`, `go.mod` | `proxy.golang.org`, `sum.golang.org`, `storage.googleapis.com` | `GOPATH`; `~/go/bin` on `PATH` |
| `golangci-lint` | `~/.cache/golangci-lint` | `.golangci.{yml,yaml,toml}` | `proxy.golang.org`, `sum.golang.org` | |
| `nodejs` | `~/.npm` | `package-lock.json`, `npm-shrinkwrap.json` | `registry.npmjs.org`, `nodejs.org` | |
| `pnpm` | `~/.local/share/pnpm/store` | `pnpm-lock.yaml` | `registry.npmjs.org` | `PNPM_HOME`; `~/.local/share/pnpm` on `PATH` |
| `yarn` | `~/.cache/yarn` | `yarn.lock` | `registry.yarnpkg.com`, `registry.npmjs.org` | |
| `bun` | `~/.bun/install/cache` | `bun.lockb`, `bun.lock`, `package-lock.json` | `bun.sh`, `registry.npmjs.org` | |
| `deno` | `~/.cache/deno` | | `deno.land`, `jsr.io` | `DENO_DIR` |
| `cargo`, `rustc` | `~/.cargo` | `Cargo.lock` | `crates.io`, `static.crates.io`, `index.crates.io` | `CARGO_HOME`; `~/.cargo/bin` on `PATH` |
| `python`, `python3` | `~/.cache/pip` | `requirements*.txt`, `poetry.lock`, `uv.lock` | `pypi.org`, `files.pythonhosted.org` | |
| `uv` | `~/.cache/uv`, `~/.local/share/uv` | `uv.lock`, `pyproject.toml`, `requirements*.txt` | `pypi.org`, `files.pythonhosted.org`, `api.github.com`, `objects.githubusercontent.com` | `UV_CACHE_DIR`, `UV_PYTHON_INSTALL_DIR` |
| `ruby` | `~/.gem` | `Gemfile.lock` | `rubygems.org` | |
| `openjdk` | `~/.gradle`, `~/.m2` | `gradle.lockfile`, `pom.xml` | `repo.maven.apache.org`, `repo1.maven.org`, `services.gradle.org` | |
| `gradle` | `~/.gradle` | `gradle.lockfile`, `gradle-wrapper.properties`, `build.gradle{,.kts}`, `settings.gradle{,.kts}`, `libs.versions.toml` | `services.gradle.org`, `plugins.gradle.org`, `repo.maven.apache.org`, `repo1.maven.org` | |
| `maven` | `~/.m2` | `pom.xml` | `repo.maven.apache.org`, `repo1.maven.org` | |
| `pre-commit` | `~/.cache/pre-commit` | `.pre-commit-config.yaml` | `pypi.org`, `files.pythonhosted.org`, `registry.npmjs.org` | |
| `mise` | `~/.local/share/mise`, `~/.cache/mise` | `mise.toml`, `.mise.toml`, `mise.lock`, `.tool-versions` | `api.github.com`, `raw.githubusercontent.com`, `mise-versions.jdx.dev`, `dl.google.com`, `nodejs.org` | `MISE_TRUSTED_CONFIG_PATHS`, `MISE_YES`; shims on `PATH`. Runs `mise install`. |
| `buf` | | | `buf.build` | |

Key patterns match at any depth below the repository root, so a monorepo's
per-module lockfiles all contribute. Versioned attributes fold into their base
entry where that makes sense: `go_1_22` is keyed and cached as `go`, `python312`
as `python`.

## `mise`

`mise` is the one entry that runs something. Declaring it installs the binary,
puts `~/.local/share/mise/shims` on `PATH`, trusts the workspace config, and
runs `mise install` before the first setup step — so the toolchain a repository
pins in `mise.toml` is on `PATH` for every setup step, every validation step and
every command the agent runs.

The shims sit **in front of** every other directory the registry adds, including
the Nix profile. That is what makes the useful combination work:

```yaml
dependencies:
  nixpkgs:
    - mise
    - go     # declared for the module and build caches, not for the binary
```

Here `mise.toml` decides which Go compiler runs, while the `go` entry still
supplies `GOPATH`, the build cache and the module proxy hosts. Drop `mise` and
the Nix-provided Go takes over; the caches are unaffected either way.

mise resolves most tools through GitHub releases, which the default allowlist
already covers, and fetches a few from the language's own distribution site. Go
and Node are listed above; anything else a `mise.toml` pins — Rust from
`static.rust-lang.org`, say — needs a line in `network.allowed_hosts`. A refused
connection reaches mise as a truncated download, so the job's error names the
host that was blocked. Enabling
mise's lockfile (`mise lock`) is worth doing: it pins tool versions, skips most
GitHub API calls, and gives the cache a key that changes only when the pins do.
For a repository that resolves many tools through the GitHub API, declare a
`GITHUB_TOKEN` [secret](kvarn-yml.md#secrets) to avoid unauthenticated rate
limits.

## Overlapping claims

Two entries can want the same directory. `openjdk` caches `~/.gradle` and
`~/.m2` so that a project declaring only a JDK and building through `gradlew`
still gets a warm cache; `gradle` and `maven` claim the same directories with
keys derived from the build files that actually change. When both are declared
the more specific entry wins: cache layers are derived in sorted dependency
order — by flake source, then by attribute — and the first claim on a path takes
it, so `gradle` and `maven` come before `openjdk`.

## `PATH` order

Everything the registry adds is written to `/etc/profile.d`, which every step
and agent shell sources as a login shell. `PATH` entries go in a file named to
sort after the Nix profile's own, so a tool that shims binaries — `mise` — wins
over the directories those binaries would otherwise come from. Among registry
entries, order is by attribute name, except that a version manager is
deliberately placed in front.

## Adding a tool

The registry lives in `internal/sandbox/tools.go`. An entry is data: cache
paths, key patterns, hosts, environment, `PATH`, and optionally a provisioning
command that must be safe to run twice and safe to run in a repository that does
not use the tool. A test fails if an entry is added without a row in this table.
