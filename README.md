<div align="center">

# kanshi

**A one-page, mobile-first glance at a machine: CPU, memory, a storage map, and every Docker container.**

One small program for Linux or Windows. No database, no agents, no history, no alerting —
open it, see what's going on, close it. When nobody is looking it does nothing at all.

[![CI](https://github.com/rene-roid/kanshi/actions/workflows/ci.yml/badge.svg)](https://github.com/rene-roid/kanshi/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/rene-roid/kanshi?sort=semver)](https://github.com/rene-roid/kanshi/releases/latest)
[![Platforms](https://img.shields.io/badge/platforms-Linux%20%7C%20Windows%20%7C%20Docker-0b7285)](#install)
[![Image](https://img.shields.io/badge/image-ghcr.io%2Frene--roid%2Fkanshi-2496ed?logo=docker&logoColor=white)](https://github.com/rene-roid/kanshi/pkgs/container/kanshi)
[![License: AGPL-3.0](https://img.shields.io/badge/license-AGPL--3.0-blue)](LICENSE)

</div>

<table>
<tr>
<td width="60%">

**Vitals & storage** — live CPU/RAM meters and the storage map
<img src="docs/screenshots/dashboard-vitals.png" alt="Processor, memory and storage map cards" />

</td>
<td width="40%">

**Mobile** — the same cards, stacked for a phone
<img src="docs/screenshots/dashboard-mobile.png" alt="Dashboard on a mobile viewport" />

</td>
</tr>
</table>

## Features

- 📊 **Processor** — overall and per-core utilisation, load average, temperature, network and disk throughput
- 🧠 **Memory & volumes** — RAM, swap, and how full each drive is
- 🗺 **Storage map** — a treemap of what is using your disk. Tap to drill in; a table below lists every folder and, on request, the largest files. Pin any folder by path
- 🐳 **Containers** — CPU, memory, network and health for every Docker container, with each published port as a link
- 🖥 **Linux and Windows** — one executable per platform, or a 10 MB Docker image
- 🔒 **Private by default** — only this computer can open it until you say otherwise; then pick your LAN, your Tailscale network, or both
- 😴 **Idles to zero** — with no browser open it stops reading stats, stops talking to Docker, and stops scanning disks
- 🪶 **Tiny** — ~8 MB of memory, no dependencies, nothing to install alongside it

## Install

Pick one. Every option ends with the dashboard at **<http://localhost:8100>**.

### Windows

1. Download `kanshi-windows-amd64.exe` from the [latest release](https://github.com/rene-roid/kanshi/releases/latest)
   (`kanshi-windows-arm64.exe` on an ARM laptop).
2. Double-click it. The dashboard opens in your browser.

Windows may say it *protected your PC* because the file is not code-signed: choose **More info → Run anyway**.
The storage map covers `C:\`, your user folder and every other fixed drive; run it as administrator to include
folders your account cannot open. Docker Desktop is picked up automatically if it is running.

### Linux

```sh
curl -fLo kanshi https://github.com/rene-roid/kanshi/releases/latest/download/kanshi-linux-amd64
chmod +x kanshi
./kanshi
```

Use `kanshi-linux-arm64` on a Raspberry Pi or other 64-bit ARM board. The storage map covers `/` and every
drive mounted under `/mnt`. To see container stats your user needs to be in the `docker` group.

To run it as a service, see [the installation guide](docs/installation.md#linux-as-a-service).

### Docker Compose (recommended on a Docker host)

```sh
curl -fLO https://raw.githubusercontent.com/rene-roid/kanshi/main/docker-compose.yml
docker compose up -d
```

The container reads the host's `/proc`, its Docker socket and its filesystem (all read-only), so what you see is
the host, not the container. The image is Linux-only by nature; on Windows use the `.exe`.

### npx

With Node.js 18+ and Docker installed, this runs the same Compose setup without a checkout:

```sh
npx @yuuki824/kanshi           # start
npx @yuuki824/kanshi update    # pull the newest image and restart
npx @yuuki824/kanshi down      # stop
```

The [installation guide](docs/installation.md) has the details for each: services, permissions, updating,
uninstalling and troubleshooting.

## Who can open it

Kanshi has no login, so who can reach it is decided entirely by where it listens. **This computer can always
open it at `localhost`.** On top of that, choose any of:

| Access | Who else can open it |
|---|---|
| `local` *(default)* | nobody — only this computer |
| `lan` | phones and computers on the same local network, wired or Wi-Fi |
| `tailscale` | your devices on your [Tailscale](https://tailscale.com) network, from anywhere |
| `all` | anyone who can reach any of this machine's addresses, public ones included |

Combine them with commas: `lan,tailscale`. There are three ways to set it:

- **From the dashboard** — open it on this computer, click **Network** at the bottom, tick what you want, **Save**.
  The choice is kept in `kanshi.env` and applied immediately, no restart. (Opened from any other device, the
  panel is read-only: only the machine itself can widen access.)
- **A flag** — `kanshi --access lan,tailscale`
- **The environment or a settings file** — `KANSHI_ACCESS=tailscale` in `.env` (Compose) or `kanshi.env`

Kanshi notices network changes on its own: if Tailscale connects after it starts, or the laptop joins a new
Wi-Fi network, it starts listening there within 30 seconds. At startup it prints every address it can be opened at.

> **Prefer `tailscale` over `all`.** `all` puts your container list and disk layout in front of anyone who can
> reach the port. Only use it behind a firewall, or a reverse proxy that asks for a login.

## Settings

Every setting is an environment variable, and can also go in a **`kanshi.env`** file using the same
`KEY=VALUE` lines ([`.env.example`](.env.example) lists them all). Kanshi reads `kanshi.env` from next to the
executable, or from your config folder (`%APPDATA%\kanshi\` on Windows, `~/.config/kanshi/` on Linux).
Precedence is: flag, then environment, then file, then the default.

| Variable | Flag | Default | What it does |
|---|---|---|---|
| `KANSHI_ACCESS` | `--access` | `local` | Who can open the dashboard ([above](#who-can-open-it)) |
| `KANSHI_PORT` | `--port` | `8100` | Port to listen on |
| `KANSHI_CONFIG` | `--config` | *(see above)* | Path of the settings file |
| | `--open` | on when double-clicked on Windows | Open the dashboard in the browser at startup |
| `KANSHI_STORAGE_ROOTS` | | `auto` | Folders in the storage map: `auto`, `path`, or `label=path`, comma-separated. `auto,/srv` adds to the defaults |
| `KANSHI_STORAGE_EXCLUDE` | | *(none)* | Folders to skip entirely, e.g. `/var/lib/docker` |
| `KANSHI_STORAGE_INTERVAL` | | `21600` | Seconds a folder's size is trusted before opening the folder above it measures it again |
| `KANSHI_STORAGE_CPU` | | `25` | Share of one core, in %, measuring folders may use. `100` = unthrottled |
| `KANSHI_TREE_DEPTH` | | `4` | Folder levels remembered from each measurement; deeper folders are measured when you open them |
| `KANSHI_STORAGE_CACHE` | | `~/.cache/kanshi/storage.cache`, `%LocalAppData%\kanshi\storage.cache`; `/data/storage.cache` in the image | Where folder sizes are cached, so a restart does not measure them again |
| `KANSHI_POLL_INTERVAL` | | `5` | Seconds between live updates |
| `KANSHI_IDLE_TIMEOUT` | | `30` | Seconds with no browser before everything stops |
| `KANSHI_DOCKER_CONCURRENCY` | | `8` | Parallel container stat requests |
| `DOCKER_HOST` | | platform default | Docker endpoint: `unix://…`, `npipe://…` or `tcp://…` |
| `KANSHI_HOST_ROOT` | | *(none; `/hostfs` in the image)* | Where the host's `/` is mounted when running in a container |
| `KANSHI_WEB_DIR` | | *(embedded)* | Serve the frontend from disk, for frontend development |

`kanshi --help` lists the flags; `kanshi --version` prints the version.

### What the storage map covers

With `KANSHI_STORAGE_ROOTS=auto`:

- **Linux:** `/`, plus every real filesystem mounted at or under `/mnt` (`/mnt/data`, `/mnt/usb`, a NAS share…).
  Drives mounted later appear on their own.
- **Windows:** the system drive (`C:\`), your user folder, and every other fixed drive (`D:\`, `E:\`…).
  Your user folder is inside `C:\`, and both come out of a single pass over the drive.

Nothing is scanned up front. Opening a folder lists it on the spot, and each subfolder's size comes from a small
cache file; the ones it has never seen, or not for six hours, are measured in the background, at the lowest CPU
and disk priority and throttled to a quarter of one core. The first visit to `/` still measures most of the disk
once. After that, opening a folder is instant, restarts included, and only folders someone opens are measured
again. **Rescan** re-measures the folders on screen.

## Updating

- **Windows / Linux binary:** download the new file from [Releases](https://github.com/rene-roid/kanshi/releases) and replace the old one.
- **Compose:** `docker compose pull && docker compose up -d`
- **npx:** `npx @yuuki824/kanshi update`

## Building from source

Go 1.25 or newer is the only requirement: there are no third-party modules.

```sh
go build .            # the binary for this machine
go test ./...         # the tests
GOOS=windows go build -o kanshi.exe .   # cross-compile for Windows from anywhere
docker compose up -d --build            # build the image from this checkout
```

The frontend is plain HTML, CSS and JavaScript with no build step, embedded into the binary at compile time.
Set `KANSHI_WEB_DIR=./web` to serve it from disk while editing.

## Releasing

Releases are made by the **Release** workflow, run by hand: *Actions → Release → Run workflow*, enter a version
such as `v0.2.0`. It runs the full test suite on Linux and Windows, builds the Windows and Linux executables
(x64 and ARM64), pushes a multi-arch image to `ghcr.io/rene-roid/kanshi`, and publishes a GitHub release with
the executables, checksums and install notes. After the very first release, set the package's visibility to
**Public** under the repository's *Packages* settings so `docker pull` works without logging in.

## API

The page holds one Server-Sent Events connection, `/api/stream`, and the server pushes every update over it —
nothing polls. The stream is gzip-compressed for browsers that accept it: on a host with 30 containers each
update is ~16 KB of JSON but about 2 KB on the wire.

| Endpoint | What it returns |
|---|---|
| `GET /api/stream` | Live updates: vitals, containers and which folder is being measured |
| `GET /api/vitals` | CPU, memory, swap, network, disk and volume figures |
| `GET /api/containers` | Per-container stats |
| `GET /api/storage` | One summary line per storage root |
| `GET /api/storage/dir?root=0&path=home/you` | One folder, listed live; subfolders not measured yet come back `pending` and are queued |
| `POST /api/storage/rescan?root=0&path=home/you` | Re-measures that folder's subfolders; needs the `X-Kanshi: 1` header |
| `GET /api/access`, `POST /api/access` | The access mode; changing it only works from this computer |
| `GET /api/config` | Version, OS and intervals |
| `GET /healthz` | `{"ok":true}` |

## How it works

[docs/design.md](docs/design.md) explains the choices behind it: why it stops when nobody is looking, how the disk
scan stays fast and polite on both platforms, why container stats use Docker's one-shot mode, and what the
memory number in `docker stats` actually means.

## License

[AGPL-3.0](LICENSE) © rene-roid
