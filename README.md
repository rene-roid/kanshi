# kanshi

A one-page, mobile-first glance at this homeserver: live CPU and RAM, a
Filelight-style storage treemap, and `docker stats` for every container — no
historical storage, no alerting, no external services.

Reachable from any device on the tailnet:

    http://homeserver.tail7ec1d9.ts.net:8100

## Install with npx

`npx` downloads and runs an npm package; it does not upload a project. This
repository includes a small npm launcher which starts the included Docker
Compose app, so Docker (with the Compose v2 plugin) is still required.

From a directory containing your `.env` file:

```sh
npx @yuuki824/kanshi
```

The command is equivalent to `docker compose up -d --build`. It reads a `.env`
file from the directory where you run it, so first copy the template and set a
safe `KANSHI_HOST` for the host being monitored:

```sh
cp .env.example .env
# edit .env, then:
npx @yuuki824/kanshi
```

Pass Docker Compose commands after the package name, for example:

```sh
npx @yuuki824/kanshi logs -f
npx @yuuki824/kanshi down
```

To publish the launcher, use:

```sh
npm login
npm run test
npm run pack:check
npm publish
```

The scoped package is configured to publish publicly. Use a unique package
name you control; `npm publish --dry-run` is a final check that uploads nothing.

## Run it

```sh
docker compose up -d --build
```

Copy `.env.example` to `.env` to override anything. Every setting has a
conservative default; the file documents each one.

## What it shows

| Card | Source | Refresh |
|---|---|---|
| Processor — hero %, per-core bars, load, temp, host net/disk throughput | `psutil` over `/proc` | every `KANSHI_POLL_INTERVAL` (5s) |
| Memory & volumes — RAM, swap, one meter per storage root | `psutil` + `statvfs` | same tick |
| Storage map — squarified treemap, tap to drill, table twin below | cached `os.scandir` walk | every `KANSHI_STORAGE_INTERVAL` (30m), or the Rescan button |
| Containers — CPU%, memory, network rates, health | Docker Engine API | same tick |

The browser holds **one SSE connection** (`/api/stream`) and the server pushes
each tick, so nothing polls and nothing needs a manual reload. There is also a
plain REST surface: `/api/vitals`, `/api/containers`, `/api/storage`,
`POST /api/storage/rescan`, `/healthz`.

## Design notes

**It stops working when you stop looking.** After `KANSHI_IDLE_TIMEOUT` with no
browser attached, the poller stops entirely and the Docker socket goes
untouched until someone loads the page. Idle cost is ~0.2% of one core.

**One-shot container stats.** `GET /containers/{id}/stats?stream=false` makes the
daemon block for a full collection cycle so it can populate `precpu_stats` —
measured at **8.3s per tick** across 31 containers. Kanshi uses `one-shot=true`
(**0.07s**) and computes CPU% against the previous tick itself. Same arithmetic,
and a 5s window is steadier to read than the daemon's 1s one.

**The disk walk is depth-bounded.** The tree is pruned to `KANSHI_TREE_DEPTH`
before it is served, so the walk only materialises a node for directories within
that depth — ~2.3k instead of ~96k on this host. Deeper directories are still
fully traversed and counted; their bytes roll up into the nearest kept ancestor.
Sizes come from `st_blocks` (so they match `du`, not apparent size) and
hardlinked files are counted once.

**The walk runs at `nice 10`** in a worker thread. A full pass over `/` takes
~47s, and the live cards keep their exact 5s cadence throughout.

**Unreadable directories are reported, not hidden.** If the walk cannot enter a
directory it is counted and the storage card says so — a silently truncated tree
that under-reports by 300 GB is worse than an obviously incomplete one.

## Access

The port is bound to the **Tailscale interface only** (`KANSHI_HOST`), so the
dashboard is not exposed on the LAN or the public interface and needs no auth
layer of its own. Set `KANSHI_HOST=0.0.0.0` to expose it on the LAN — but note
that anything that can reach this app can reach the Docker socket through it, so
add an auth proxy first if you do.

If Tailscale is down when Docker starts, the bind fails and `restart:
unless-stopped` retries until `tailscale0` is back.

## Permissions

Runs as root with `cap_drop: ALL` plus **`DAC_READ_SEARCH`** — read and traverse
bypass, but *not* write bypass (that would be `DAC_OVERRIDE`). Without it the
walk cannot enter `/home/yuuki` (mode 0750) and silently under-reports the root
filesystem by ~330 GB. The rootfs is read-only and every host mount is `:ro`.

## About kanshi's own memory number

The dashboard will show kanshi using far more memory than you'd expect for a
10 MB Python process. That figure is mostly **reclaimable kernel dentry cache**
charged to its cgroup — an unavoidable side effect of `stat`-ing ~150k files
during a walk. Actual anonymous memory is ~27 MB; `mem_limit` acts as a ceiling
on the cache and the kernel reclaims it under pressure. The number matches what
`docker stats` reports for any container, which is the point.

## Tuning

Slowest thing here is the walk of `/`, dominated by ~100k overlay2 files under
`/var/lib/docker`. To skip it:

```sh
KANSHI_STORAGE_EXCLUDE=/hostfs/var/lib/docker
```

Its ~48 GB then disappears from the `/` total, so only do this if you don't want
it counted.
