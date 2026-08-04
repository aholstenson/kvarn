# Kvarn documentation

These docs follow [Diátaxis](https://diataxis.fr): material is split by whether
you are trying to *get something done* (how-to) or *look something up*
(reference). The [project README](../README.md) is the overview — what Kvarn is,
how isolation works, and how to install it.

## How-to guides

Goal-oriented recipes for an operator running the orchestrator.

- [Run the orchestrator](how-to/run-the-orchestrator.md) — from an empty host to a first pull request.
- [Configure a repository](how-to/configure-a-repository.md) — write and verify a `kvarn.yml`.
- [Connect a forge](how-to/connect-a-forge.md) — GitHub tokens, GitHub Apps, plain Git remotes.
- [Manage API keys](how-to/manage-api-keys.md) — authenticate clients and scope them to projects.
- [Manage secrets](how-to/manage-secrets.md) — give a job credentials without giving them to the agent.
- [Control job costs](how-to/control-job-costs.md) — budgets, warnings, and validation retries.
- [Tune host capacity](how-to/tune-host-capacity.md) — the admission pool, queue, per-tenant caps, priority.
- [Speed up job startup](how-to/speed-up-job-startup.md) — repository mirrors, tool caches, the OCI image cache.
- [Take a host out of service](how-to/take-a-host-out-of-service.md) — drain it, let running jobs finish, then stop it.
- [Follow up on a pull request](how-to/follow-up-on-a-pull-request.md) — feedback runs, watching and cancelling.

## Reference

Descriptions of every configuration surface, organized by the file it lives in.

- [Configuration overview](reference/configuration.md) — which file holds what, where they live, environment variables.
- [`kvarn.yml`](reference/kvarn-yml.md) — the per-repository build, validation, cache and network config.
- [`projects.toml`](reference/projects-toml.md) — projects, their repositories, and per-project overrides.
- [`forges.toml`](reference/forges-toml.md) — forge instances and pull-request behavior.
- [`credentials.toml`](reference/credentials-toml.md) — forge credentials.
- [`agents.toml`](reference/agents-toml.md) — model aliases and user-level job defaults.
- [`orchestrator.toml`](reference/orchestrator-toml.md) — scheduler pool, caches, mirrors, session retention.
- [`apikeys.toml`](reference/apikeys-toml.md) — API keys and their scopes and caps.
- [CLI](reference/cli.md) — every command and flag.
