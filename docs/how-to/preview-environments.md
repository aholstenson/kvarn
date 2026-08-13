# Preview environments

Kvarn jobs produce branches, but a branch is only a description of a change.
A preview environment is a way to *look at* one: a long-lived VM pinned to a
branch, reachable over HTTP at a stable hostname, booted the first time somebody
asks for it and stopped once nobody is.

It reuses the machinery a job already has. The same repository mirror clones it,
the same `kvarn.yml` describes it, the same sandbox boots it. What is different
is what happens after setup: instead of handing the workspace to an agent, kvarn
starts the servers the repository declares and routes HTTP into them.

> **Read [Restrict who can reach a preview](#restrict-who-can-reach-a-preview)
> before you expose one.** A preview runs unreviewed branch code with the
> project's real secrets available to it. Access control is the part kvarn
> deliberately leaves to your network, and it is not optional.

## Configure the host

Add a `[preview]` section to `orchestrator.toml`:

```toml
[preview]
domain = "preview.example.com"
listen = "100.64.0.1:8080"
idle_timeout = "30m"
max_lifetime = "8h"
max_concurrent = 3
```

`domain` is the base every preview hostname is formed under. `listen` is a
plain-HTTP listener with no authentication — bind it to an address only your
fronting layer can reach.

Both are required together, and the orchestrator refuses to start with one
without the other: a preview with no name is unaddressable, and one with no
listener is unreachable. Without the section entirely, previews are simply off.

Point DNS at the fronting layer with a wildcard, so a new branch needs no DNS
change:

```
*.preview.example.com.  A  <the address your TLS terminator listens on>
```

See [`orchestrator.toml`](../reference/orchestrator-toml.md#preview) for every
key.

## Terminate TLS in front

There is no ACME client in kvarn and no TLS on the ingress listener. Something
in front handles certificates and forwards plain HTTP.

With Caddy:

```caddyfile
*.preview.example.com {
    tls {
        dns cloudflare {env.CF_API_TOKEN}
    }
    reverse_proxy 100.64.0.1:8080
}
```

A wildcard certificate needs a DNS-01 challenge, since the names are not
individually resolvable from the public internet.

Kvarn reads `X-Forwarded-Proto` from the request and passes it through to the
app, so an app behind HTTPS sees `https` and generates the right absolute URLs.
Set it in whatever terminates TLS.

## Declare the preview in the repository

The operator owns the domain; the repository owns the shape. Add a `preview:`
block to `kvarn.yml`:

```yaml
preview:
  apps:
    web: { port: 3000 }
  serve:
    - { name: Web, run: npm start, app: web }
  ready:
    - { name: Web up, run: "curl -fsS http://localhost:3000/healthz" }
```

That is the whole single-app case. `apps` says what listens where, `serve` says
what to run, and `ready` says how to tell when it is up. Setup steps still run
first, so dependency installation and migrations belong in `setup.steps` exactly
as they do for a job.

A preview with several addressable services names a host pattern per app:

```yaml
preview:
  apps:
    web:    { port: 3000, host: "{ref}.{domain}" }
    assets: { port: 8080, host: "assets-{ref}.{domain}" }
  serve:
    - { name: Web, run: npm start, app: web }
    - { name: Assets, run: npm run assets, app: assets }
```

A pattern has to end in `{domain}`, so a branch cannot claim a name outside the
domain the operator configured. `{ref}` always expands to exactly one DNS label,
whatever the branch is called.

See [`kvarn.yml`](../reference/kvarn-yml.md#preview) for the full reference.

### Give the app its own URL

Before the serve commands run, each app's URL is exported as
`KVARN_PREVIEW_URL_<APP>`:

```yaml
preview:
  apps:
    web: { port: 3000 }
  serve:
    - name: Web
      run: npm start
      app: web
```

```js
// next.config.js
module.exports = {
  assetPrefix: process.env.KVARN_PREVIEW_URL_WEB,
};
```

Use it anywhere the app has to emit an absolute URL — asset prefixes, OAuth
redirect URIs, CORS origins, links in outgoing mail. An app that hardcodes
`http://localhost:3000` will render, and then break on the first absolute link.

## Try it locally first

A `preview:` block can be run against the working tree, on your own machine,
before any of the host configuration above exists:

```sh
kvarn local preview
```

It boots the same sandbox, runs setup, starts the same serve steps and waits for
the same ready checks. What it does not have is a domain, so each app is
forwarded to a loopback port instead of a hostname:

```
  api  http://localhost:8080
  web  http://localhost:3000

Press Ctrl-C to stop.
```

An app keeps the port it listens on inside the VM whenever that port is free on
the host; if it is taken, another is chosen and the printed URL is the one that
works. `--port web=4000` pins one explicitly. `KVARN_PREVIEW_URL_<APP>` is set
to these `localhost` URLs, so an app that reads it correctly works in both
places and one that hardcodes its own address fails here too — which is the
point of running it locally.

Once the preview is up, whatever the servers print streams to the terminal,
prefixed with the service that wrote it. If the boot fails instead, that output
is printed as the explanation.

## Bring one up

```sh
kvarn preview up my-project feat/new-checkout
```

The command follows the boot and prints each phase as it happens — cloning,
installing dependencies, running setup, starting services — then prints the URL
on stdout with everything else on stderr, so it pipes:

```sh
open "$(kvarn preview up my-project feat/new-checkout)"
```

A first boot takes a minute or more: it is a real clone, a real dependency
install and a real VM. Later boots of the same branch are faster because the
repository mirror and the tool caches are warm.

Pass `--no-wait` to return as soon as the boot has started.

## Live with it

```sh
kvarn preview ls                                   # what exists, and its state
kvarn preview logs my-project feat/new-checkout    # what the servers printed
kvarn preview down my-project feat/new-checkout    # stop the VM
kvarn preview down my-project feat/new-checkout --remove   # and forget it
```

`down` stops the VM but keeps the record and the hostname, so the next request
to that hostname boots the preview again. `--remove` is for when the branch is
gone and the name should be freed.

Logs are the last ~256 KiB of what the serve processes printed, kept in memory.
A server running for hours produces unbounded output, so it is not persisted;
what makes it to the session event log is the boot, the failures and the process
exits.

## What stops a preview

- **Idle.** No request for `idle_timeout` (default 30 minutes). The next request
  boots it again, so an idle preview costs a database row rather than a VM.
- **Age.** `max_lifetime` after it booted (default 8 hours), whatever its
  traffic — so a preview somebody keeps poking at is still re-derived from the
  branch eventually.
- **Capacity.** Reaching `max_concurrent`, or a full scheduler pool, evicts the
  least-recently-requested idle preview to make room. Only a host where
  everything running is in active use answers with a holding page instead.
- **Drain or restart.** A preview cannot migrate — it is a VM inside the
  orchestrator process, reachable only through the network that process owns —
  so `kvarn queue drain` stops previews outright, and a restart resets every
  record to stopped. In both cases the next request boots it again.

None of these lose anything, because a preview holds nothing: it is derived from
its branch and rebuilt from it.

## Restrict who can reach a preview

This is the section to act on rather than skim.

**Put the ingress behind a network boundary.** Tailscale, a VPN, or an IP
allowlist on the fronting proxy. Kvarn does not authenticate preview traffic and
will not: the point of a preview is that a colleague opens a link.

Three things make that boundary matter more than it would for an ordinary
staging deployment:

- **A preview runs unreviewed code.** Anyone who can open a branch decides what
  runs in the VM. The VM is isolated from the host, but it is not isolated from
  whoever can send it requests.
- **A preview resolves the project's real secrets.** Exactly as a job does —
  there is no separate preview secret store. Managed secrets are never handed to
  the VM, but the egress proxy will attach them to outbound requests the code
  inside makes, so branch code can drive a request that leaves with production
  credentials on it. Somebody who can reach a preview can, by extension, get it
  to act with those credentials.
- **A preview is guessable.** Its hostname comes from the branch name. Nothing
  about the URL is a secret, which is why kvarn sends `X-Robots-Tag: noindex` on
  every response but does not pretend that is protection.

**Put previews on a domain that shares no registrable parent with production.**
`preview.example.com` and `app.example.com` share `example.com`, and a cookie
that branch code sets with `Domain=example.com` is a cookie the production app
will send back. Use a separate domain — `example-preview.com`,
`example.dev` — and the browser keeps them apart for you.

**Leave `allow_forks` off** unless the network boundary already answers for it.
A fork's branch is written by somebody without push access to the repository,
and a preview would give that branch the project's secrets. See
[`projects.toml`](../reference/projects-toml.md#projectsnamepreview).

## Bring your own services

A preview is ephemeral and holds no state: the VM is destroyed when it stops,
and the next boot starts from the branch again. Anything the app needs — a
database, a queue, an object store — has to come up inside the VM.

The usual shape is a setup step that starts containers, and serve commands that
talk to them:

```yaml
setup:
  steps:
    - name: Database
      run: |
        podman run -d --name db -p 5432:5432 \
          -e POSTGRES_PASSWORD=preview postgres:17
        until pg_isready -h localhost; do sleep 1; done
    - name: Migrate
      run: npm run db:migrate

preview:
  apps:
    web: { port: 3000 }
  serve:
    - { name: Web, run: npm start, app: web }
  ready:
    - { name: Web up, run: "curl -fsS http://localhost:3000/healthz" }
```

Seed data belongs in a setup step too. Pointing a preview at a shared external
database works, but it means every branch shares one schema, and a branch whose
migrations are half-finished breaks the others — the thing previews exist to
avoid.

## When something does not work

**The hostname 404s.** Nothing is registered for it. Previews are not created
automatically from branches; run `kvarn preview up` first, and check
`kvarn preview ls` for the name it actually claimed — `{ref}` is slugged, so
`feat/login` is not `feat/login.preview.example.com`.

**The holding page never becomes the app.** The boot failed. The page reports
the reason; `kvarn preview ls` shows the state, and the boot's session has the
full output:

```sh
kvarn jobs list --project my-project --limit 5
kvarn jobs events <session-id>
```

**502 from the app.** The VM is up but the server is not answering on the port
the app declares. `kvarn preview logs` shows what it printed — usually a crash
on startup, or a server bound to `127.0.0.1` in a way the port declaration does
not match.

**Assets 404 or load from the wrong host.** The app is emitting absolute URLs it
built itself. Wire `KVARN_PREVIEW_URL_<APP>` into its asset prefix.

## Related

- [`kvarn.yml` reference: `preview`](../reference/kvarn-yml.md#preview)
- [`orchestrator.toml` reference: `[preview]`](../reference/orchestrator-toml.md#preview)
- [`projects.toml` reference: per-project preview policy](../reference/projects-toml.md#projectsnamepreview)
- [Manage secrets](manage-secrets.md) — what a preview inherits, and why it matters here
- [Take a host out of service](take-a-host-out-of-service.md) — what draining does to previews
