# Kerge panel

The self-hosted monitoring panel of [Kerge](https://kerge.io): a
single-user, read-only dashboard for a handful of hosts. Each host runs
the [Kerge agent](https://github.com/kergeio/kerge-agent), which reports
to the panel over one outbound WebSocket connection. The panel shows live
and historical metrics, renewal reminders and traffic against monthly
limits; it never runs anything on a host.

## Status

Released. The current version is on the
[releases page](https://github.com/kergeio/kerge-panel/releases/latest);
changes are listed in [CHANGELOG.md](CHANGELOG.md). `get.kerge.io` serves
the installer of the current version.

## Install

The panel runs with Docker Compose on a Linux host and needs a domain.
Two questions decide how it is set up:

1. Is the domain proxied by Cloudflare (orange cloud)?
2. Does a web server already use ports 80 and 443 on the host?

| | Ports 80 and 443 are free | A web server already uses them |
|---|---|---|
| **No Cloudflare, or DNS only** | `curl -fsSL https://get.kerge.io \| sudo bash` | `curl -fsSL https://get.kerge.io \| sudo bash -s -- --external-proxy` |
| **Cloudflare proxy** | `curl -fsSL https://get.kerge.io \| sudo bash -s -- --cloudflare` | `curl -fsSL https://get.kerge.io \| sudo bash -s -- --cloudflare --external-proxy` |

With free ports the panel brings its own Caddy, which obtains the
certificate. With a web server in the way, the panel listens on
`127.0.0.1:3000` and your web server forwards to it; see
[Using a reverse proxy you already run](#using-a-reverse-proxy-you-already-run).
On a first install the script also notices by itself when the domain
resolves to Cloudflare.

### Before you start

- A domain for the panel (v1 cannot run on a bare IP address):
  - without Cloudflare, or set to DNS only: an A and/or AAAA record
    pointing at the host's public address;
  - proxied by Cloudflare: the host as origin, and the domain's SSL/TLS
    encryption mode set to **Full (strict)** (see [Cloudflare](#cloudflare)).
- With the panel's own Caddy: ports 80 and 443 reachable from the
  internet.

### Step 1: install Docker

Skip this if Docker with its Compose plugin is installed.

```
curl -fsSL https://get.docker.com | sudo sh
```

### Step 2: install Kerge

```
curl -fsSL https://get.kerge.io | sudo bash
```

The script asks for the domain. Options go after `bash -s --`, for
example:

```
curl -fsSL https://get.kerge.io | sudo bash -s -- --domain panel.example.com
```

| Option | Effect |
|---|---|
| `--domain <name>` | the panel's domain, instead of asking |
| `--version <v>` | install that release; the default is the latest |
| `--cloudflare` / `--no-cloudflare` | the domain is / is not proxied by Cloudflare |
| `--external-proxy` / `--no-external-proxy` | use a web server you already run / the panel's own Caddy |
| `--dir <path>` | installation directory; default `/opt/kerge` |

The script checks the signature and checksums of everything it
downloads before it changes anything, stops if ports 80 and 443 are
taken (and points to `--external-proxy`), and warns if the domain does
not point where it should. It then starts the panel and prints the
address and a one-time setup code:

```
  Kerge is running. Open https://panel.example.com in a browser
  and enter this setup code:

    ABCD-EFGH-...
```

Open the address, enter the code and create the admin account. Finally
the script reaches the panel through its public address, as browsers and
agents will, and explains what it finds if that fails. If the code
scrolled away: `docker logs kerge`.

The chosen mode is recorded in `.env`. Running the script again without
mode options keeps it; with them it switches, keeping data and
certificates.

### Check the script before running it

Download `install.sh` from the release, compare its sha256 with the table
below, then run it:

```
curl -fsSL -o kerge-install.sh https://github.com/kergeio/kerge-panel/releases/download/v<version>/install.sh
echo "<sha256>  kerge-install.sh" | sha256sum -c - && sudo bash kerge-install.sh
```

| Release | sha256 of `install.sh` |
|---|---|
| none yet | |

Each release also lists it in its release notes. The files the script
downloads are listed in the release's `checksums.txt`, signed with the
Kerge release key (namespace `kerge-release`, fingerprint
`SHA256:o8FwbEF+/tiVjyIxLZp+Pg5WlDcSu5M5dJgY5WgwMiM`); the script
carries the key and verifies the signature itself.

## Adding hosts

In the panel, choose **Add host**, give it a name, and run the command
the panel shows on that host. The agent's
[README](https://github.com/kergeio/kerge-agent#readme) describes what
the command does and how to configure the agent.

## Cloudflare

For a domain proxied by Cloudflare:

- The SSL/TLS encryption mode must be **Full (strict)**. With
  **Flexible**, Cloudflare connects to the host over plain HTTP, the host
  redirects that to HTTPS, and browsers end up in a redirect loop.
- Nothing may challenge requests to `/api/agent/ws`: agents cannot solve
  a challenge and stay disconnected. That rules out **Under Attack**
  mode and any WAF or custom rule that challenges that path. Bot Fight
  Mode, Rocket Loader and Email Address Obfuscation do not affect the
  panel or the agents. Web Analytics records nothing, since the panel's
  content security policy blocks its beacon (the browser console shows
  the blocked script).
- The panel's own Caddy believes the visitor address Cloudflare sends
  (`CF-Connecting-IP`) only from Cloudflare's address ranges. The script
  fetches the current ranges on every run, and falls back to the
  snapshot in the release when it cannot. Run it again now and then to
  refresh them.
- After switching the domain's proxy on or off in Cloudflare, run the
  script again with `--cloudflare` or `--no-cloudflare`.

## Using a reverse proxy you already run

With `--external-proxy` the script runs only the panel, listening on
`127.0.0.1:3000`, and no Caddy. Your web server then has to:

- terminate TLS for the panel's domain;
- forward to `http://127.0.0.1:3000`, including WebSocket upgrades (the
  live dashboard and the agents use them);
- set `X-Forwarded-For` to the client's address, replacing whatever the
  client sent.

Examples in [`deploy/examples/`](deploy/examples/):

| File | For |
|---|---|
| `nginx.conf` | nginx, domain not proxied by Cloudflare |
| `Caddyfile` | Caddy, domain not proxied by Cloudflare |
| `nginx-cloudflare.conf` | nginx, domain proxied by Cloudflare |
| `Caddyfile-cloudflare` | Caddy, domain proxied by Cloudflare |

The Cloudflare examples take the visitor's address from
`CF-Connecting-IP` only for requests from Cloudflare's ranges. They list
the ranges of [`deploy/cloudflare-ips.txt`](deploy/cloudflare-ips.txt);
keeping them current is up to you (Cloudflare publishes them at
https://www.cloudflare.com/ips/).

The panel takes `X-Forwarded-For` only from the addresses in
`KERGE_TRUSTED_PROXIES`. In this mode it defaults to `172.30.0.1`, the
gateway of the panel's Docker network, which is where connections from
the host's own web server arrive from. If you change the network with
`KERGE_NET_SUBNET`, or your proxy reaches the panel another way, set
`KERGE_TRUSTED_PROXIES` in `.env` to the address the panel sees, and run
`docker compose -f compose.external-proxy.yml up -d` in the
installation directory.

## Running the panel

Everything lives in the installation directory, `/opt/kerge` by default:

| Path | Contents |
|---|---|
| `.env` | domain, deployment mode and optional settings; no secrets. All variables are listed in [`.env.example`](.env.example). |
| `data/` | the database |
| `caddy/` | Caddy's certificates and configuration (own Caddy only) |
| `compose.yml` or `compose.external-proxy.yml` | the release's compose file |

- **Upgrade:** run the install command again. It installs the latest
  release (or `--version <v>`) and keeps data, settings and certificates.
- **Logs:** `docker logs kerge`, and `docker logs kerge-caddy` for Caddy.
- **Forgotten password:** `docker exec -it kerge /kerge-panel reset-admin`
  deletes the admin account; `docker restart kerge` then prints a new
  setup code (`docker logs kerge`) and the setup wizard opens again.
  Hosts and their data stay.

## Known limitations

- **Host addresses.** The address shown for a host is the one its agent
  connects from, as the panel sees it:
  - a dual-stack host shows the IPv4 or IPv6 address of its current
    connection;
  - an agent that connects through a proxy or VPN, or from the panel's
    own private network, shows that proxy's or private address;
  - with a domain proxied by Cloudflare, the panel has to be set up in
    Cloudflare mode, or it shows Cloudflare's addresses.
- **Bonded interfaces.** A bonded or teamed interface and its members
  count the same traffic twice. Exclude one side in the panel's
  interface rules (settings, or per host).
- **Disks.** Only the root filesystem is monitored.
- **Users.** One admin account; no further users or roles.

## Building from source

For development. [`compose.dev.yml`](compose.dev.yml) builds the image
from the [`Dockerfile`](Dockerfile) and publishes the panel on
`127.0.0.1:3000`:

```
docker compose -f compose.dev.yml up --build
```

Open http://localhost:3000 in Chrome or Firefox (Safari refuses the
panel's secure cookies over plain HTTP); the setup code is in
`docker compose -f compose.dev.yml logs`. Without Docker, `make build`
builds `bin/panel`; see [`CONTRIBUTING.md`](CONTRIBUTING.md) for the
checks.

A source build pins no agent release, so the install command it shows
is an example with placeholders.

## Repositories

| Repository | License | Contents |
|---|---|---|
| [kerge-protocol](https://github.com/kergeio/kerge-protocol) | Apache-2.0 | The agent protocol and its reference implementation |
| [kerge-agent](https://github.com/kergeio/kerge-agent) | Apache-2.0 | The monitoring agent |
| kerge-panel | AGPL-3.0 | This repository |

## License

GNU Affero General Public License v3.0. See `LICENSE` and `NOTICE`.

Running a modified version of this panel as a network service requires
offering its source to the users of that service; see section 13 of the
license.

The panel image contains third-party code under its own licenses: the Go
standard library, the modules from `go.mod` that the panel is built from,
and uPlot. Their texts are in the image at
`/usr/share/doc/kerge-panel/THIRD_PARTY_LICENSES`, next to `LICENSE` and
`NOTICE`.
