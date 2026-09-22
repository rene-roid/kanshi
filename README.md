<div align="center">

# kanshi

**A one-page, mobile-first glance at this homeserver.**

Live CPU and RAM, a Filelight-style storage treemap, and `docker stats` for
every container — no historical storage, no alerting, no external services.

[![Go](https://img.shields.io/badge/Go-1.25-00add8?logo=go&logoColor=white)](https://go.dev/)
[![Image](https://img.shields.io/badge/image-9.6MB%20scratch-0b7285)](https://hub.docker.com/_/scratch)
[![Docker](https://img.shields.io/badge/Docker-Compose-2496ed?logo=docker&logoColor=white)](https://docs.docker.com/compose/)
[![npm](https://img.shields.io/badge/npx-%40yuuki824%2Fkanshi-cb3837?logo=npm&logoColor=white)](https://www.npmjs.com/package/@yuuki824/kanshi)

</div>

---

## Requirements

- **Linux host.** Metrics come from `/proc`, `/sys` and `statfs(2)` — macOS and Windows are not supported.
- **Docker Engine** with the **Compose v2 plugin** (`docker compose version` must work).
- **Access to `/var/run/docker.sock`** — run as root or as a user in the `docker` group.
- **Node.js 18+** — only if you install with `npx`.
- Nothing else. No Go toolchain, no database, no build step: the image compiles the binary itself.

## Run it

From a clone of this repository:

```sh
docker compose up -d --build
```

Or from anywhere, without cloning:

```sh
npx @yuuki824/kanshi
```

Either way the dashboard is at <http://localhost:8100>. The first run builds the
image, so give it a minute before the health check passes.

## Expose it on a network

Kanshi binds to `127.0.0.1` by default — private to the host. Set `KANSHI_HOST`
to change that:

```sh
# Tailnet only (recommended). Reachable from your other Tailscale devices.
KANSHI_HOST="$(tailscale ip -4)" docker compose up -d

# Local LAN only, on this machine's LAN address.
KANSHI_HOST=192.168.1.50 docker compose up -d

# Every interface. Only behind an authenticated reverse proxy.
KANSHI_HOST=0.0.0.0 docker compose up -d
```

The same variables work with `npx`:

```sh
KANSHI_HOST="$(tailscale ip -4)" npx @yuuki824/kanshi
```

> **`0.0.0.0` publishes Docker information to anyone who can reach the port.**
> Prefer the Tailscale address.

To make it permanent, copy `.env.example` to `.env` and edit it. A `.env` in the
directory you run `npx` from is picked up automatically.

## Common commands

```sh
docker compose logs -f          # follow logs
docker compose restart          # restart it
docker compose down             # stop and remove
docker compose up -d --build    # rebuild after changing the source

npx @yuuki824/kanshi logs -f    # same, via the launcher
npx @yuuki824/kanshi down
npx @yuuki824/kanshi@latest     # update to the newest published version
```

Not starting? Check the container state and the last lines of its log:

```sh
docker ps --filter name=kanshi
docker logs --tail=100 kanshi
```

## Settings

Every value below is the built-in default; override it in `.env` or the
environment.

| Variable | Default | What it does |
|---|---|---|
| `KANSHI_HOST` | `127.0.0.1` | Bind address (see above). Set by Compose; the bare binary defaults to `0.0.0.0` |
| `KANSHI_PORT` | `8100` | Bind port |
| `KANSHI_POLL_INTERVAL` | `5` | Seconds between live ticks |
| `KANSHI_IDLE_TIMEOUT` | `30` | Seconds with no browser before polling stops |
| `KANSHI_DOCKER_CONCURRENCY` | `8` | Parallel container stat requests |
| `KANSHI_STORAGE_ROOTS` | `/=/hostfs,/mnt/data=/mnt/data` | `label=path` pairs for the storage map |
| `KANSHI_STORAGE_INTERVAL` | `1800` | Seconds between disk walks |
| `KANSHI_STORAGE_EXCLUDE` | *(empty)* | Comma-separated paths to skip, container-side |
| `KANSHI_STORAGE_CPU` | `25` | Share of one core, in %, the walk may average; `100` turns the throttle off |
| `KANSHI_TREE_DEPTH` | `4` | Folder depth you can drill into; deeper bytes roll up into their ancestor |
| `KANSHI_WEB_DIR` | *(embedded)* | Serve `web/` from disk instead of the binary |

The walk of `/` is the slowest thing here, dominated by ~100k overlay2 files.
To skip them, at the cost of their ~48 GB no longer being counted:

```sh
KANSHI_STORAGE_EXCLUDE=/hostfs/var/lib/docker
```

## Screenshots

<table>
<tr>
<td width="60%">

**Vitals & storage** — live CPU/RAM meters and the storage treemap
<img src="docs/screenshots/dashboard-vitals.png" alt="Processor, memory and storage map cards" />

</td>
<td width="40%">

**Mobile** — the same cards, stacked for a phone screen
<img src="docs/screenshots/dashboard-mobile.png" alt="Dashboard on a mobile viewport" />

</td>
</tr>
</table>

## Features

- 📊 **Processor** — hero utilisation %, per-core bars, load average, temperature, host net/disk throughput
- 🧠 **Memory & volumes** — RAM, swap, and one meter per storage root
- 🗺 **Storage map** — squarified treemap you can tap to drill into, with a table twin below that lists every folder (show more / show all) and, on request, the largest files; depth-bounded so it stays fast on large filesystems
- 🐳 **Containers** — per-container CPU%, memory, network rate and health straight from the Docker Engine API, plus each published `host → internal` port mapping as a link that opens on whatever address you reached the dashboard at
- ⚡ **One SSE connection** — the server pushes every tick over `/api/stream`; nothing polls, nothing needs a manual reload
- 😴 **Idles to near-zero** — the poller and the Docker socket both go quiet after `KANSHI_IDLE_TIMEOUT` with nobody watching
- 🪶 **9.6 MB image, ~8 MB resident** — one static Go binary on `scratch`: no interpreter, no shell, no package manager
- 🔒 **Tailscale-friendly** — binds to `127.0.0.1` by default; point it at a Tailscale IP to share it on a tailnet instead of the open LAN

## Tech stack

| Layer | Choice |
|---|---|
| Backend | Go 1.25, standard library only — zero third-party dependencies |
| Host metrics | `/proc` and `/sys` parsed directly; `statfs(2)` for volumes |
| Live updates | Server-Sent Events (`/api/stream`) |
| Frontend | Vanilla JS, hand-rolled SVG treemap — no build step, embedded in the binary |
| Container metrics | Docker Engine API (one-shot stats, not the streaming daemon default) |
| Packaging | `scratch` image via multi-stage build, Docker Compose, `npx` launcher |

## API

| Card | Source | Refresh |
|---|---|---|
| Processor — hero %, per-core bars, load, temp, host net/disk throughput | `/proc/stat`, `/proc/net/dev`, `/proc/diskstats`, `/sys` hwmon | every `KANSHI_POLL_INTERVAL` |
| Memory & volumes — RAM, swap, one meter per storage root | `/proc/meminfo` + `statfs(2)` | same tick |
| Storage map — squarified treemap, tap to drill, table twin below | cached breadth-first `getdents`+`fstatat` walk | every `KANSHI_STORAGE_INTERVAL`, or the Rescan button |
| Containers — CPU%, memory, network rates, health, port mappings | Docker Engine API | same tick |

The browser holds **one SSE connection** (`/api/stream`) and the server pushes
each tick, including the storage scan's status and progress. The storage map
itself is never pushed: `/api/storage` is one summary line per root, and each
folder is fetched from `/api/storage/dir?root=0&path=home/yuuki` as it is
opened, answered from the cached walk without touching the disk. A background
tab drops its stream, so the server idles while nobody is looking. The rest of
the REST surface is `/api/vitals`, `/api/containers`,
`POST /api/storage/rescan`, `/healthz`.

## Building

Docker is the only build dependency; the image compiles the binary itself.

```sh
docker compose up -d --build
```

With a local Go toolchain (1.22+), `go build .` and `go vet ./...` work from the
repository root with no module downloads — there are no third-party imports. The
frontend is embedded with `//go:embed`, so a rebuild is needed after editing
anything under `web/`; set `KANSHI_WEB_DIR=./web` to serve it from disk instead
while iterating. Note that a binary run outside Compose binds `0.0.0.0` unless
you set `KANSHI_HOST` yourself.

Publishing the npm launcher:

```sh
npm login
npm run test
npm run pack:check
npm publish
```

## Design notes

**It stops working when you stop looking.** After `KANSHI_IDLE_TIMEOUT` with no
browser attached, the poller stops entirely and the Docker socket goes
untouched until someone loads the page. Idle cost is ~0.2% of one core.

**One-shot container stats.** `GET /containers/{id}/stats?stream=false` makes the
daemon block for a full collection cycle so it can populate `precpu_stats` —
measured at **8.3s per tick** across 31 containers. Kanshi uses `one-shot=true`
(**0.07s**) and computes CPU% against the previous tick itself. Same arithmetic,
and a 5s window is steadier to read than the daemon's 1s one.

**The disk walk is breadth-first and depth-bounded.** It goes one level at a
time, so the folders you can drill into are all finished before the deep, bulky
part of the tree starts. Only directories within `KANSHI_TREE_DEPTH` get a node
of their own — ~2.3k instead of ~96k on this host. Deeper directories are still
fully traversed and counted; their bytes roll up into the nearest kept ancestor.
Sizes come from `st_blocks` (so they match `du`, not apparent size) and
hardlinked files are counted once.

**The walk barely allocates.** Directory entries are parsed straight out of the
`getdents64` buffer and stat'ed with `fstatat` relative to the open directory,
so the kernel resolves one path component rather than the whole path, and no
string is built per file. Each BFS level's paths are packed into one buffer.
Compared with the previous depth-first walk this uses ~30% less CPU and ~5× fewer
garbage collections. The trade-off is that a whole level is queued at once,
which raises the walk's peak memory by a few MB. The heap goes back to the OS as
soon as the walk ends.

**The walk is throttled and runs at `nice 19`.** It runs on a locked, dedicated OS
thread with the lowest best-effort I/O priority. Linux applies both settings per
thread, and the runtime retires that thread when the walk ends rather than
handing it back to the poller. `nice` only matters when something else wants the
CPU, so the walk also measures its own thread's CPU time and sleeps between
directories to average `KANSHI_STORAGE_CPU` (25% of one core by default). Time
spent waiting on the disk counts as idle. On a warm cache that makes a pass
about 4× longer but never more than a quarter of a core; on a cold cache the
disk is the bottleneck anyway.

**Unreadable directories are reported, not hidden.** If the walk cannot enter a
directory it is counted and the storage card says so — a silently truncated tree
that under-reports by 300 GB is worse than an obviously incomplete one.

**Permissions.** Runs as root with `cap_drop: ALL` plus **`DAC_READ_SEARCH`** —
read and traverse bypass, but *not* write bypass (that would be `DAC_OVERRIDE`).
Without it the walk cannot enter `/home/yuuki` (mode 0750) and silently
under-reports the root filesystem by ~330 GB. The rootfs is read-only and every
host mount is `:ro`.

**About kanshi's own memory number.** The dashboard will show kanshi using far
more memory than the process actually has. That figure is mostly **reclaimable
kernel dentry cache** charged to its cgroup — an unavoidable side effect of
`lstat`-ing ~150k files during a walk. Measured anonymous memory is **~8 MB at
rest and ~20 MB at the peak of a full walk**; `mem_limit` acts as a ceiling on
the cache and the kernel reclaims it under pressure. `GOMEMLIMIT` sits below
`mem_limit` so an unusually large filesystem makes the collector work harder
instead of getting the container OOM-killed. The number matches what
`docker stats` reports for any container, which is the point.
