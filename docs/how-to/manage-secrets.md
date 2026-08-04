# Manage secrets

A build often needs credentials — a private package registry, a container
registry, an API the tests call. Kvarn splits this into two decisions:

- **Where the value lives**, set on the host with `kvarn secrets set --type`.
- **How it is used**, declared in the repository's `kvarn.yml`.

The repository never contains a value; the host store never contains a usage
site.

## Set a value on the host

```sh
printf '%s' "$API_TOKEN" | kvarn secrets set my-project API_TOKEN
kvarn secrets set my-project DOCKERHUB --type managed --value "$DOCKERHUB_TOKEN"
kvarn secrets list my-project
kvarn secrets remove my-project API_TOKEN
```

Values are stored in `~/.config/kvarn/secrets.toml` and are never printed by
`list`. Piping the value on stdin keeps it out of your shell history.

## Choose a type

| Type | The VM sees | Use for |
| --- | --- | --- |
| `env` (default) | The real value, as an environment variable. | Values the build itself must read — a registry token a tool needs in its own config, a test fixture. |
| `managed` | An unguessable placeholder. | Credentials that only ever travel to a known host in an outbound request. |

For a `managed` secret, the real value stays on the host. The job gets a
placeholder, and the egress proxy substitutes the real value into matching
outbound requests just before they leave the host. The credential itself never
enters the VM, so nothing running there — including the agent — can read, log or
exfiltrate it.

Prefer `managed` when it fits. It fits whenever the secret's whole job is to
authenticate a request to a host you can name.

## Declare it in the repository

```yaml
secrets:
  - API_TOKEN
  - name: DOCKERHUB
    scheme: basic
    hosts:
      - registry-1.docker.io
      - auth.docker.io
```

A bare name is shorthand for the `bearer` scheme over any allowlisted host.
Every declared secret appears inside the VM as an environment variable of that
name — the real value for `env`, the placeholder for `managed`.

`scheme` tells the proxy how to find and replace the placeholder in an
outbound request:

| Scheme | Substitutes into | Typical user |
| --- | --- | --- |
| `bearer` (default) | Header values, verbatim — e.g. `Authorization: Bearer <placeholder>`. | Most HTTP APIs. |
| `basic` | Inside `user:secret`, decoding and re-encoding the HTTP Basic blob. | Docker/podman, npm, git over HTTPS. |
| `oauth` | The request body — a form-encoded or JSON token exchange. | OAuth token endpoints. |

`hosts` narrows substitution to specific hosts, using the same syntax as
`network.allowed_hosts`. Set it: it is the difference between "this credential
can be used against Docker Hub" and "this credential can be used against
anything the build is allowed to reach".

The hosts must also be reachable, so they belong in `network.allowed_hosts` too
unless a dependency source already allowlisted them.

## Test locally

`kvarn run` and `kvarn test` resolve secrets from the same store. They infer the
project from the git remote, or take it explicitly:

```sh
kvarn test --project my-project
kvarn run --project my-project --secrets-file ./secrets.toml --diff "…"
```

## Forge credentials are separate

The credentials Kvarn uses to clone and push are a different axis, configured in
[`credentials.toml`](../reference/credentials-toml.md) and never exposed to a
job. The VM gets no git remote and no reflog, so there is no field for a clone
URL's credentials to hide in.
