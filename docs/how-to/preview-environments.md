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
  sites:
    web: { port: 3000 }
  serve:
    - { name: Web, run: npm start }
  ready:
    - { name: Web up, run: "curl -fsS http://localhost:3000/healthz" }
```

That is the whole single-site case. `sites` says which hostname is served from
which port, `serve` says what to run, and `ready` says how to tell when it is up.
Setup steps still run first, so dependency installation and migrations belong in
`setup.steps` exactly as they do for a job.

A preview with several addressable services names a host pattern per site:

```yaml
preview:
  sites:
    web:    { port: 3000, host: "{ref}.{domain}" }
    assets: { port: 8080, host: "assets-{ref}.{domain}" }
  serve:
    - { name: Web, run: npm start }
    - { name: Assets, run: npm run assets }
```

A pattern has to end in `{domain}`, so a branch cannot claim a name outside the
domain the operator configured. `{ref}` always expands to exactly one DNS label,
whatever the branch is called.

Several sites may name the same port when one virtual-hosting server answers
under several names. Only the hostnames have to be unique:

```yaml
preview:
  sites:
    web:    { port: 80 }
    assets: { port: 80, host: "assets-{ref}.{domain}" }
  serve:
    - { name: Web, run: npm start }
```

Kvarn passes the server the hostname the browser asked for, so its own
virtual-host matching decides which name got the request. Configure that matching
from `KVARN_PREVIEW_URL_<SITE>` rather than hardcoding the names — that is also
what makes the shared-port case work under `kvarn local preview`, where each site
is reached on its own loopback port unless you give the run a base domain.

See [`kvarn.yml`](../reference/kvarn-yml.md#preview) for the full reference.

### Servers that setup already started

If setup brings the application up on its own — a container stack, a process
manager — there is no foreground command for kvarn to supervise, and `serve` is
left out entirely. What is usually still needed is a one-shot command per boot
that tells the application the names it now answers on. That is `preview.setup`:
run to completion before the serve steps, with the site URLs already exported.

```yaml
preview:
  sites:
    web: { port: 3000 }
  setup:
    - { name: Domains, run: ./bin/configure-domains }
  ready:
    - { name: Web up, run: "curl -fsS http://localhost:3000/healthz" }
```

Setup steps run before any hostname exists, which is why this list is separate:
`KVARN_PREVIEW_URL_<SITE>` is only known once the preview has been resolved
against a ref and a domain. A step here that fails fails the boot.

### Give the server its own URLs

Before any preview step runs, each site's URL is exported as
`KVARN_PREVIEW_URL_<SITE>`, and every setup step, serve step and ready check
gets all of them:

```yaml
preview:
  sites:
    web: { port: 3000 }
  serve:
    - name: Web
      run: npm start
```

```js
// next.config.js
module.exports = {
  assetPrefix: process.env.KVARN_PREVIEW_URL_WEB,
};
```

Use them anywhere the server has to emit an absolute URL — asset prefixes, OAuth
redirect URIs, CORS origins, links in outgoing mail — and to configure which
hostname each virtual host answers to. A server that hardcodes
`http://localhost:3000` will render, and then break on the first absolute link.

## Try it locally first

A `preview:` block can be run against the working tree, on your own machine,
before any of the host configuration above exists:

```sh
kvarn local preview
```

It boots the same sandbox, runs setup, starts the same serve steps and waits for
the same ready checks. What it does not have is a domain, so each site is
forwarded to a loopback port instead of a hostname:

```
  api  http://localhost:8080
  web  http://localhost:3000

Press Ctrl-C to stop.
```

A site keeps the port it listens on inside the VM whenever that port is free on
the host; if it is taken, another is chosen and the printed URL is the one that
works. Sites sharing a guest port get a host port each, since locally the port is
what tells them apart. `--port web=4000` pins one explicitly.
`KVARN_PREVIEW_URL_<SITE>` is set to these `localhost` URLs, so a server that
reads them correctly works in both places and one that hardcodes its own address
fails here too — which is the point of running it locally.

Once the preview is up, whatever the servers print streams to the terminal,
prefixed with the service that wrote it. If the boot fails instead, that output
is printed as the explanation.

### Serve them under real hostnames

Some things only happen under a domain: virtual-host matching, cookie scopes,
absolute links between sites, an OAuth redirect the provider has on file. A
loopback port cannot exercise any of them, so give the preview a domain of your
own choosing:

```sh
kvarn local preview --base-domain sws.local
```

The site hostnames are expanded from the same `host` patterns by the same
resolver the orchestrator uses, and one listener serves all of them, routing on
the Host header:

```
  api  http://api-local.sws.local:3000  →  guest port 3000
  web  http://local.sws.local:3000      →  guest port 3000

Those names do not resolve here yet. Add to /etc/hosts:

  127.0.0.1	api-local.sws.local local.sws.local

Press Ctrl-C to stop.
```

Add that line once and the names keep working: `{ref}` expands to `local` rather
than the checked-out branch, so switching branches does not change them (pass
`--ref` to choose another label). Pick a domain that is yours to make up —
`.local` reaches mDNS on some systems, and a real domain you do not control will
resolve past your machine. Inside the VM the same names resolve to the guest
itself, so a ready check or one site calling another reaches the preview rather
than the internet.

The listener takes the port the sites share inside the VM when it can, which is
what makes one URL mean the same thing inside the VM and outside it. Sites on
different guest ports cannot both have that, so the listener falls back to `8080`
and the command says which sites are affected; `--ingress-port` chooses
explicitly. It is plain HTTP — nothing here terminates TLS — so an application
that insists on `https` still needs the hosted preview.

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

## Start previews on demand

A preview can also start because somebody asked for it. Give the project the
hostnames it should answer to, in `projects.toml`:

```toml
[projects.my-project.preview]
auto_start = ["pr-{pr}.{domain}"]
```

and name the repository's sites the same way, in `kvarn.yml`:

```yaml
preview:
  sites:
    web: { port: 3000, host: "pr-{pr}.{domain}" }
```

Now the first request for `pr-12.preview.example.com` reads `12` out of the
hostname, asks the forge which branch pull request 12 is, registers a preview of
that branch and boots it. The person who opened the link waits on the same
holding page a stopped preview shows, and the tab reloads into the app.

Nothing has to run at pull-request time. The name works because the pattern
says it will, so a link in a pull request template or a forge check is enough —
there is no webhook to wire up and no preview sitting idle for a branch nobody
opened.

The two patterns have to agree. `auto_start` is what turns a hostname into a
pull request, and `kvarn.yml` is what the preview actually answers on; if they
resolve to different names the boot fails rather than coming up under a name
nobody asked for. The page then says the preview did not start, and
`kvarn preview list` says why.

**What it refuses.** Only open pull requests start anything. A pull request
whose head branch lives in a fork needs `allow_forks` for that project, because
a preview runs that branch's code with the project's real secrets. Hostnames
that resolve to nothing are remembered as such for a minute, resolutions that
succeed are remembered for a minute too, and first-time resolutions are rate
limited, so walking pull request numbers at the ingress does not turn into a
walk of your forge's API.

A boot that fails is not repeated for two minutes, so a branch that cannot come
up at all costs one attempt rather than one per page load. `kvarn preview up`
retries immediately, which is what to run after fixing the cause.

Why a hostname was refused stays in the orchestrator's log except for the two
answers a developer can act on — the pull request is not open, and previews of
forks are off for this project. The ingress has no authentication in front of
it, so it does not repeat project names, repository names or anything the forge
said about the credentials.

**What tidies up.** A preview started this way is removed — record, hostname and
all — once its pull request closes or merges. Until then it stops and restarts
on the ordinary idle and lifetime rules. A preview started with `kvarn preview
up` is never removed on its own, whoever else asks for it.

### Put the link in the pull request

A reviewer still has to learn the hostname. The comment kvarn posts on every
pull request it opens or commits onto can carry it, through the `.PreviewURL`
field of a [comment header](../reference/forges-toml.md#preview-addresses):

```toml
[defaults.pull_request.comment_headers]
new_pull_request = "{{ with .PreviewURL }}**Preview:** {{ . }}{{ end }}"
follow_up_commit = "{{ with .PreviewURL }}**Preview:** {{ . }}{{ end }}"
```

The address is formed from the branch's own site patterns, so the comment
carries it whether or not anything has started — with an `auto_start` route,
following the link is what brings the preview up. Keep the `with` guard: a run
on a branch with no `preview:` block has no address to give, and the guard is
what leaves the comment saying nothing rather than saying "Preview:".

## Live with it

```sh
kvarn preview list                                   # what exists, and its state
kvarn preview logs my-project feat/new-checkout    # what the servers printed
kvarn preview down my-project feat/new-checkout    # stop the VM
kvarn preview down my-project feat/new-checkout --remove   # and forget it
```

`down` stops the VM but keeps the record and the hostname, so the next request
to that hostname boots the preview again. `--remove` is for when the branch is
gone and the name should be freed; it also drops whatever state the preview had
saved, since nothing would ever restore it.

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
- **A closed pull request**, for a preview that
  [started itself](#start-previews-on-demand). That one is removed rather than
  stopped: the record and its hostname go too, since nobody registered it and
  nobody should have to unregister it.
- **`kvarn preview down`**, when somebody is finished with it before any of the
  above gets there.

A preview that declares no state loses nothing when any of these happen: it is
derived from its branch and rebuilt from it. A preview that
[keeps state](#keeping-state-between-boots) has that state written out first on
every one of them except the last two — a removed preview and a closed pull
request are somebody saying the preview is finished with, and both discard what
it was holding.

## Keeping state between boots

By default a preview holds nothing. It is stopped when it goes idle, destroyed,
and re-derived from the branch on the next request — so a seeded tenant, an
uploaded file, or rows a reviewer entered walking through a pull request are gone
when they come back from lunch.

A repository fixes that by naming what should survive:

```yaml
preview:
  sites:
    web: { port: 3000 }
  state:
    paths:
      - ~/.local/share/containers/storage/volumes/pgdata
    max_size: 4GiB
```

`$KVARN_PREVIEW_STATE_DIR` (`/home/kvarn/state`) is kept with nothing declared at
all, so the simplest form is a compose stack that bind-mounts a volume out of it.
It is exported everywhere the site URLs are: the preview's shell, the VM's
environment, and each serve process. It sits outside the workspace on purpose —
the workspace is a fresh clone on every boot.

On every graceful stop kvarn runs the repository's `state.save` steps, shuts the
serve processes down, and tars the state directory and any declared paths onto
the host. The next boot unpacks them before `preview.setup` runs and then runs
`state.restore`. **A restore that fails fails the boot**, with the reason on the
holding page: a preview that quietly comes up empty after somebody spent an
afternoon on it is worse than one that refuses and says why.

```sh
kvarn preview ls                                        # DATA shows size and age
kvarn preview up my-project feat/checkout --fresh       # come up empty
kvarn preview down my-project feat/checkout --no-state  # stop without saving
kvarn preview reset my-project feat/checkout            # drop what it saved
kvarn preview prune --older-than 720h                   # sweep by hand
```

`state.save` and `state.restore` are for a stack that would rather keep a
logical dump than a raw data directory — `pg_dump` into the state directory on
the way out, `pg_restore` on the way back in. That is also the honest answer to
engine and schema drift: a snapshot is routinely restored onto a newer commit
than the one that wrote it, and migrations are the repository's business.

Archives are dropped after `state_retention` (default 30 days) without being
used; restoring one restarts its clock. An expired archive is not an error — the
next boot comes up fresh and says so. A SIGKILLed orchestrator loses whatever
its VMs held: every graceful path is covered, an abrupt one is not.

See [`preview.state`](../reference/kvarn-yml.md#previewstate) for the fields and
[`[preview]`](../reference/orchestrator-toml.md#preview) for the operator's
timeout, retention and size ceiling.

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
- **Saved state outlives the VM.** A repository that
  [keeps state](#keeping-state-between-boots) has an archive written to the
  orchestrator's disk, unencrypted, for as long as the retention window. Its
  contents are whatever unreviewed branch code put on disk — which can include
  secrets that code wrote there itself. It is readable by whoever can read the
  host's cache directory, and by anyone who can reach the preview, since the next
  boot puts it back. This is why a preview of a fork's pull request never keeps
  state, and why `state_retention` is a security setting as much as a disk one.

**Put previews on a domain that shares no registrable parent with production.**
`preview.example.com` and `app.example.com` share `example.com`, and a cookie
that branch code sets with `Domain=example.com` is a cookie the production app
will send back. Use a separate domain — `example-preview.com`,
`example.dev` — and the browser keeps them apart for you.

**Leave `allow_forks` off** unless the network boundary already answers for it.
A fork's branch is written by somebody without push access to the repository,
and a preview would give that branch the project's secrets. Even with it on, a
fork's preview never keeps state: whatever its code writes to disk goes with the
VM. See [`projects.toml`](../reference/projects-toml.md#projectsnamepreview).

## Bring your own services

Anything the app needs — a database, a queue, an object store — has to come up
inside the VM. The VM is destroyed when the preview stops, and the next boot
starts from the branch again; only what the repository
[declares as state](#keeping-state-between-boots) survives that.

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
  sites:
    web: { port: 3000 }
  serve:
    - { name: Web, run: npm start }
  ready:
    - { name: Web up, run: "curl -fsS http://localhost:3000/healthz" }
```

Seed data belongs in a setup step too. Pointing a preview at a shared external
database works, but it means every branch shares one schema, and a branch whose
migrations are half-finished breaks the others — the thing previews exist to
avoid.

For the container's data to survive a stop, put its volume under
`$KVARN_PREVIEW_STATE_DIR` and declare `preview.state` — see
[keeping state between boots](#keeping-state-between-boots).

## When something does not work

**The hostname 404s.** Nothing is registered for it. Previews are not created
automatically from branches; run `kvarn preview up` first, and check
`kvarn preview list` for the name it actually claimed — `{ref}` is slugged, so
`feat/login` is not `feat/login.preview.example.com`.

**The holding page never becomes the app.** The boot failed. The page says so
but not why — it answers anybody who can reach the ingress. `kvarn preview list`
shows the state and the reason, and the boot's session has the full output:

```sh
kvarn jobs list --project my-project --include-previews --limit 5
kvarn jobs events <session-id>
```

A preview's boot session is left out of `kvarn jobs list` unless you ask for it
with `--include-previews`: nobody submitted it as work, so it does not belong in
a listing of jobs.

**502 from the app.** The VM is up but the server is not answering on the port
the site declares. `kvarn preview logs` shows what it printed — usually a crash
on startup, or a server bound to `127.0.0.1` in a way the port declaration does
not match.

**One hostname serves another site's content.** Several sites share that port and
the server is not matching on the `Host` header, so every name reaches whichever
virtual host it defaults to. Configure its virtual hosts from
`KVARN_PREVIEW_URL_<SITE>`.

**Assets 404 or load from the wrong host.** The server is emitting absolute URLs
it built itself. Wire `KVARN_PREVIEW_URL_<SITE>` into its asset prefix.

## Related

- [`kvarn.yml` reference: `preview`](../reference/kvarn-yml.md#preview)
- [`orchestrator.toml` reference: `[preview]`](../reference/orchestrator-toml.md#preview)
- [`projects.toml` reference: per-project preview policy](../reference/projects-toml.md#projectsnamepreview)
- [Manage secrets](manage-secrets.md) — what a preview inherits, and why it matters here
- [Take a host out of service](take-a-host-out-of-service.md) — what draining does to previews
