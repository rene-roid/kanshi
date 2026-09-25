# How kanshi works

Kanshi is deliberately small: a glance-and-go dashboard, not a monitoring platform. There is no history, no
alerting and no login — that is Grafana or netdata territory. What it does do, it tries to do at close to zero
cost, because it usually runs on the same small box it is watching.

## It stops working when you stop looking

After `KANSHI_IDLE_TIMEOUT` (30 s) with no browser attached, the live poller stops entirely: no `/proc` reads,
no Docker API calls. It sleeps until a page connects again, then takes a one-second baseline so the first numbers
it shows are real rather than zeros.

The storage map follows the same rule, one folder at a time. Nothing is walked at startup. Opening a folder reads
that one directory, and each subfolder's size comes from a small cache file. Only a subfolder the cache has
never seen, or has not measured for `KANSHI_STORAGE_INTERVAL` (six hours), is walked, and only its own subtree.
Only the folder someone opened is ever refreshed, so a disk nobody is looking at is never read.

This matters more than it sounds. On the homeserver this was written for, the previous version scanned every
30 minutes whether or not anyone was watching: each scan read ~2.8 GB of directory metadata from disk (the 96 MB
memory limit lets the kernel drop that cache between scans) and used ~42 CPU-seconds. That added up to ~130 GB of
disk reads and half an hour of CPU a day, for nobody. Now an unwatched kanshi reads nothing from disk, and used
88 ms of CPU over two measured minutes.

Moving from one whole-disk walk to per-folder walks took the rest of the waste out. Every restart or image update
used to cost a full 100-second walk before the map was back. Now the cache survives it: after a restart, the
1.6 TB `/mnt/data` listing came back fully sized in 2 ms, with nothing walked.

## Folder sizes live in a small cache file

A folder's size needs everything under it read, so a plain directory listing is not enough on its own. The listing
is live (names, files and their sizes are always current), and the subfolder sizes are the cached part: one entry
per folder, grouped by parent so a folder's subfolders are a single map lookup. Opening a folder is one directory
read and that lookup, ~1 ms on the homeserver.

A walk records every folder within `KANSHI_TREE_DEPTH` levels of where it started, so drilling further in is
usually instant. It first drops everything cached below its starting point, so nothing older than the walk
survives under it. The exception is another configured root: `/mnt/data` sits under `/` but is its own filesystem,
and a walk of `/mnt` must not wipe it. A folder deeper than any walk reached has no entry. It is walked when it is
opened, so there is no depth limit on drilling in. A listing that finds a cached subfolder gone forgets it and
everything under it.

Walks go through one queue on one low-priority thread. The folders a listing is waiting on go to the front, stale
ones to the back. A folder inside one that is already queued or being walked is left to that walk, and the page
shows it as "sizing…" until then. Each finished walk bumps a timestamp on the live stream, which is the page's cue
to re-read the folder on screen.

The whole cache sits in memory (a few thousand entries for all of `/` on the homeserver) and goes to disk as one
`encoding/gob` file after each walk: written to a temporary file, then renamed over the old one, so a crash
mid-write leaves the previous cache intact. Nothing here needs a database. A folder's own subfolders are the only
query, and one walk at a time is the only writer, so a map and a file do the job with no dependency. A file that is
missing, unreadable or from another version is simply started over. It lives in the user's cache folder, `/data`
in the image (a volume, since the root filesystem is read-only), or the systemd unit's `CacheDirectory`. If it
cannot be written, kanshi logs it and keeps sizes in memory instead.

## Listening decides who can connect

With no login, where kanshi listens is its access control. Loopback (`127.0.0.1` and `::1`) is always on. The
access mode adds listeners on specific addresses rather than on `0.0.0.0` and filtering requests afterwards, so a
port scan from a network you did not choose finds nothing.

- **Tailscale** addresses are recognised by range (`100.64.0.0/10`, `fd7a:115c:a1e0::/48`), which works whether
  the adapter is called `tailscale0` or `Tailscale`.
- **LAN** addresses are private addresses (RFC 1918, IPv6 unique-local) on real adapters. Docker and libvirt
  bridges, veths, VPN tunnels and Hyper-V/WSL switches are skipped: listening there would only expose the
  dashboard to containers and VMs.

The set is re-checked every 30 seconds. An address that is not there yet — Tailscale still connecting at boot —
is a warning and a retry, not a crash. Only a failure to bind loopback stops kanshi, since that means the port is
taken.

Changing the mode from the dashboard is the one security-sensitive action in the API, so it is only accepted
over loopback, with a loopback `Host` header (a hostile page can point its own domain at `127.0.0.1`, but it
cannot change the `Host` its requests carry), from the page's own origin, and with a custom header that a form
or `<img>` on another site cannot send. It is refused outright when a flag or environment variable sets the mode,
because that would silently undo the change on the next start.

## One-shot container stats

`GET /containers/{id}/stats?stream=false` makes the Docker daemon block for a full collection cycle so it can
fill in `precpu_stats` — measured at **8.3 s per tick** across 31 containers. Kanshi uses `one-shot=true`
(**0.07 s**) and computes CPU% against the previous tick itself: the same arithmetic, over a five-second window
rather than one, which is also steadier to read.

On Windows the daemon is reached over Docker Desktop's named pipe. The pipe is opened for overlapped I/O and
handed to `os.NewFile`, which since Go 1.25 puts such handles on the runtime's poller: the HTTP client gets
cancellation and deadlines without a third-party pipe library. When no daemon answers at all, kanshi only looks
again once a minute.

## Reading the host cheaply

Each live tick reads a handful of kernel counters. On Linux the `/proc` files stay open and are re-read with
`pread` at offset zero, which makes the kernel regenerate them; parsing is done in place on one reused buffer. The
CPU temperature sensor is located once, not searched for every tick, and the sets of physical disks and network
adapters are refreshed once a minute. A full sample takes about **0.14 ms and 10 small allocations**, down from
3.1 ms and 600.

Throughput only counts physical hardware. Summing every line of `/proc/net/dev` counts a container's traffic up
to three times (its veth, the Docker bridge, the real NIC) plus loopback; summing every block device counts LVM
and dm-crypt I/O again on the disk underneath. Only adapters and disks that sysfs ties to a real device are summed.
Windows does the same with the adapters' `HardwareInterface` flag and `IOCTL_DISK_PERFORMANCE` on each physical
drive.

## The disk walk is breadth-first and depth-bounded

It goes one level at a time, so the folders that get cached are all finished before the deep, bulky part of the
tree starts. Only directories within `KANSHI_TREE_DEPTH` of where the walk started get a node of their own — ~2.3k
instead of ~96k for all of `/` on the homeserver. Deeper directories are still fully traversed and counted; their
bytes roll up into the nearest kept ancestor. Sizes are space on disk (`st_blocks` on Linux, the allocation size on
Windows), so they match `du` rather than apparent size, and hard-linked files are counted once per walk.

**The walk barely allocates.** On Linux, entries are parsed straight out of the `getdents64` buffer and stat'ed with
`fstatat` relative to the open directory, so the kernel resolves one path component rather than the whole path. On
Windows, `GetFileInformationByHandleEx` returns names, attributes, sizes and file IDs for a whole batch of entries
per call, so no file is opened or stat'ed individually. Each BFS level's paths are packed into one buffer, and the
heap goes back to the OS as soon as the walk ends.

**It never follows links.** Symlinks on Linux, and junctions, symlinks and mounted folders on Windows (every
reparse point with the name-surrogate bit), are skipped, so nothing loops and nothing is counted twice. OneDrive
placeholders are not name surrogates and are walked normally; files that live only in the cloud take no space and
count as such. On Windows, `C:\Windows`' component store hard-links most of `System32`, so files there are counted
once by file ID; elsewhere hard links are rare enough that tracking every ID would cost more memory than it saves.

**It is throttled and runs at the lowest priority.** The walk runs on a dedicated, locked OS thread at `nice 19` and
the lowest best-effort I/O class on Linux, or in `THREAD_MODE_BACKGROUND` on Windows (which lowers CPU, I/O and
memory priority together). Both are per-thread settings, and the runtime retires that thread when the walk ends
rather than handing it to the poller. Low priority only matters when something else wants the CPU, so the walk
also measures its own thread's CPU time and sleeps between directories to average `KANSHI_STORAGE_CPU` (25% of one
core by default). Time spent waiting on the disk counts as idle.

**Unreadable directories are reported, not hidden.** If the walk cannot enter a directory it is counted and the
storage card says so — a silently truncated tree that under-reports by 300 GB is worse than an obviously incomplete
one.

## Serving the page

The frontend is plain HTML, CSS and JavaScript, embedded in the binary. At startup every asset is hashed and
gzipped once. `index.html` refers to each as `/static/app.js?v=<hash>`, and those URLs are served as immutable, so
a returning browser downloads nothing but a 304 for the page itself.

Live updates are one Server-Sent Events stream per browser. It is gzip-compressed for the life of the connection:
consecutive updates are nearly identical and deflate's window reaches back into the previous one, so a ~16 KB update
costs about 2 KB on the wire — a minute of watching went from 214 KB to 28 KB on the homeserver. The containers table updates rows in place rather than rebuilding itself every
tick, and a background tab drops its stream, which is what lets the server go idle.

## Permissions

In the container kanshi runs as root with `cap_drop: ALL` plus **`DAC_READ_SEARCH`** — read and traverse bypass,
but *not* write bypass (that would be `DAC_OVERRIDE`). Without it the walk cannot enter home directories (mode 0750)
and silently under-reports the root filesystem. The root filesystem is read-only and every host mount is `:ro`. The
systemd unit grants the same single capability to a throwaway user; on Windows the equivalent is
`SeBackupPrivilege`, which kanshi enables when run as administrator.

## About kanshi's own memory number

`docker stats` shows kanshi using far more memory than the process actually has. That figure is mostly
**reclaimable kernel dentry and inode cache** charged to its cgroup — an unavoidable side effect of stat'ing every
file during a walk. Anonymous memory is **~8 MB at rest and ~20 MB at the peak of a full walk**; `mem_limit` acts as
a ceiling on the cache, and the kernel reclaims it under pressure. `GOMEMLIMIT` sits below `mem_limit` so an
unusually large filesystem makes the collector work harder instead of getting the container OOM-killed. The
standalone binaries set the same kind of budget on themselves (two scheduler threads, a 128 MB soft memory limit)
unless `GOMAXPROCS` or `GOMEMLIMIT` say otherwise.
