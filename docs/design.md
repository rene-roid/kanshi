# How kanshi works

Kanshi is deliberately small: a glance-and-go dashboard, not a monitoring platform. There is no history, no
alerting and no login — that is Grafana or netdata territory. What it does do, it tries to do at close to zero
cost, because it usually runs on the same small box it is watching.

## It stops working when you stop looking

After `KANSHI_IDLE_TIMEOUT` (30 s) with no browser attached, the live poller stops entirely: no `/proc` reads,
no Docker API calls. It sleeps until a page connects again, then takes a one-second baseline so the first numbers
it shows are real rather than zeros.

The storage scan follows the same rule. It walks every root once at startup, so the first visit has a map. After
that, a rescan only happens while someone has the page open, and only when it would show something new: the
scheduler compares each volume's used space against the last scan and skips the walk if nothing moved by more than
0.5% (or 256 MB, whichever is larger). Six hours is the most a watched map is allowed to age, because moving files
around inside a volume changes the map without changing used space. A drive mounted or removed triggers a scan the
next time someone is watching.

This matters more than it sounds. On the homeserver this was written for, the previous version scanned every
30 minutes whether or not anyone was watching: each scan read ~2.8 GB of directory metadata from disk (the 96 MB
memory limit lets the kernel drop that cache between scans) and used ~42 CPU-seconds. That added up to ~130 GB of
disk reads and half an hour of CPU a day, for nobody. Now an unwatched kanshi reads nothing.

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

It goes one level at a time, so the folders you can drill into are all finished before the deep, bulky part of the
tree starts. Only directories within `KANSHI_TREE_DEPTH` of their root get a node of their own — ~2.3k instead of
~96k on the homeserver. Deeper directories are still fully traversed and counted; their bytes roll up into the
nearest kept ancestor. Sizes are space on disk (`st_blocks` on Linux, the allocation size on Windows), so they
match `du` rather than apparent size, and hard-linked files are counted once.

A root inside another root on the same filesystem — `C:\` and `C:\Users\you` by default on Windows — is not walked
twice. The outer walk picks it up on the way past and restarts the depth count there, so it gets full detail of its
own at no extra cost.

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
consecutive updates are nearly identical and deflate's window spans several of them, so each ~16 KB update shrinks
to a few hundred bytes on the wire. The containers table updates rows in place rather than rebuilding itself every
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
