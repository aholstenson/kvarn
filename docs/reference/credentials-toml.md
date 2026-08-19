# `credentials.toml`

The host secrets Kvarn authenticates outbound work with, in two blocks:

- `[credentials.<name>]` — what forges use to clone, push, and talk to a forge
  API. Each entry is a named bag of fields; which fields matter depends on the
  `type` of the forge that references it by name.
- `[llm.<provider>]` — the API keys the coding agent calls model providers
  with. See [LLM providers](#llm-providers).

Default location `~/.config/kvarn/credentials.toml`, overridable with
`--credentials-file`. Written with mode `0600` when managed by the store, and
re-read on every request.

```toml
[credentials.github]
token = "ghp_..."
```

Reference it from [`forges.toml`](forges-toml.md) with `credential = "github"`.

The two blocks have separate namespaces, so a forge credential may be named
after a provider without being mistaken for one.

## GitHub

Either a personal access token:

| Key | Notes |
| --- | --- |
| `token` | Personal access token. Used for both Git operations and the API. |

Or a GitHub App installation:

| Key | Notes |
| --- | --- |
| `app_id` | Numeric App ID, as a string. |
| `private_key_path` | Path to the App's PEM private key on the host. |
| `installation_id` | Numeric installation ID, as a string. |

```toml
[credentials.github-app]
app_id = "12345"
private_key_path = "/path/to/private-key.pem"
installation_id = "67890"
```

All three App fields are required together. An App installation token expires
after an hour, so Kvarn resolves it at each operation rather than once at job
start — a push at the end of a long run mints a fresh token. Tokens are cached
until five minutes before expiry.

## Plain Git

| Key | Notes |
| --- | --- |
| `token` | Sent as the password with username `x-access-token`. HTTPS remotes. |
| `username` / `password` | HTTP Basic credentials. HTTPS remotes. |
| `ssh_key_path` | Path to a private key. SSH remotes. |
| `ssh_key_pass` | Passphrase for that key, if it has one. |

```toml
[credentials.git-creds]
username = "user"
password = "..."
# or: token = "..."
# or: ssh_key_path = "/path/to/key", ssh_key_pass = "..."
```

The credential must match the remote's scheme: a token or password against an
`ssh://` URL, or an SSH key against an `https://` URL, is rejected with an
explicit error rather than silently falling back.

## LLM providers

The API keys the coding agent authenticates model calls with. The table key is
the provider name — the segment before the slash in a model ID such as
`anthropic/claude-sonnet-4-6` — so one of `anthropic`, `openai`, `openrouter`
or `google`. A key stored under any other name is never consulted.

```toml
[llm.anthropic]
api_key = "sk-ant-..."

[llm.openrouter]
api_key = "sk-or-..."
headers = { x-title = "kvarn" }
```

| Key | Notes |
| --- | --- |
| `api_key` | Provider API key. Sent in the provider's native auth header. |
| `headers` | Extra headers set on every request to that provider, applied after `api_key`. For a gateway token, a tenant ID, or provider-specific attribution. |

A provider absent from this block falls back to the environment, so an
env-only setup keeps working and a one-off `ANTHROPIC_API_KEY=... kvarn local job`
still overrides nothing else:

| Provider | Variable |
| --- | --- |
| `anthropic` | `ANTHROPIC_API_KEY` |
| `openai` | `OPENAI_API_KEY` |
| `openrouter` | `OPENROUTER_API_KEY` |
| `google` | `GEMINI_API_KEY`, then `GOOGLE_API_KEY` |

An entry present but carrying neither `api_key` nor `headers` counts as absent
and falls back the same way.

Credentials are resolved per outbound model request from the file, so editing
it rotates a key under a running orchestrator with no restart. A file that
fails to parse fails the request rather than quietly falling back to the
environment — a typo must not silently swap which key is in use.

## How credentials reach git

Worth knowing when debugging, because it constrains what will and will not work:

- Credentials are passed as per-invocation `git -c` config and process
  environment, never written to a config file. The inline credential helper
  names two environment variables rather than carrying the secret, so a token
  never appears in `argv`.
- A leading empty `credential.helper=` resets the chain, so a helper configured
  on the host can neither supply credentials Kvarn did not choose nor capture
  the ones it did.
- SSH runs with `IdentitiesOnly=yes` and `IdentityAgent=none`, so a running
  `ssh-agent` cannot silently substitute a different identity. Host keys are
  verified against keys pinned in the binary plus the operator's own
  `known_hosts`.
- A passphrase-protected key is decrypted in process and written without the
  passphrase into a `0700` directory that exists for the length of one command.
- Immediately after a clone, the remote and the reflog are removed, so no
  credentialed URL is shipped into the VM inside `.git`.
- Repository URLs are redacted in logs — the userinfo of a credentialed URL is
  replaced before anything is written.
