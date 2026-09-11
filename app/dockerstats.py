"""Docker Engine API client over the unix socket.

Uses `GET /containers/{id}/stats?stream=false&one-shot=true`. The one-shot form
returns immediately; without it the daemon blocks each request for a full
collection cycle to produce `precpu_stats`, which measured 8.3s per tick across
31 containers versus 0.07s here.

The tradeoff is that one-shot zeroes `precpu_stats`, so CPU% is computed against
the previous tick's counters instead — the same thing the daemon would have
done, just over the poll interval rather than a 1s window. That is also a
steadier number to read at a glance.
"""
from __future__ import annotations

import asyncio
import time

import httpx

from .config import config

API_VERSION = "v1.43"

_prev_net: dict[str, tuple[float, int, int]] = {}
_prev_cpu: dict[str, tuple[int, int]] = {}   # cid -> (total_usage, system_usage)
_client: httpx.AsyncClient | None = None


def client() -> httpx.AsyncClient:
    global _client
    if _client is None:
        _client = httpx.AsyncClient(
            transport=httpx.AsyncHTTPTransport(uds=config.docker_socket, retries=1),
            base_url=f"http://docker/{API_VERSION}",
            timeout=httpx.Timeout(20.0, connect=5.0),
        )
    return _client


async def close() -> None:
    global _client
    if _client is not None:
        await _client.aclose()
        _client = None


def _cpu_percent(cid: str, stats: dict) -> float:
    """CPU% against the previous tick. 100% = one full core, as `docker stats`."""
    cpu = stats.get("cpu_stats") or {}
    usage = (cpu.get("cpu_usage") or {}).get("total_usage")
    system = cpu.get("system_cpu_usage")
    if usage is None or system is None:
        return 0.0

    prev = _prev_cpu.get(cid)
    _prev_cpu[cid] = (usage, system)
    if prev is None:
        return 0.0  # first sighting; the next tick has a real delta

    cpu_delta = usage - prev[0]
    sys_delta = system - prev[1]
    # A restarted container resets its counters — report 0 rather than a
    # nonsensical negative or a huge spike.
    if sys_delta <= 0 or cpu_delta < 0:
        return 0.0
    # online_cpus is absent on older daemons; fall back to the per-cpu array.
    ncpu = cpu.get("online_cpus") or len((cpu.get("cpu_usage") or {}).get("percpu_usage") or []) or 1
    return round(min(cpu_delta / sys_delta * ncpu * 100.0, ncpu * 100.0), 2)


def _memory(stats: dict) -> tuple[int, int]:
    mem = stats.get("memory_stats") or {}
    usage = mem.get("usage")
    if usage is None:
        return (0, 0)
    detail = mem.get("stats") or {}
    # Match `docker stats`: subtract page cache so the number reflects the
    # working set. cgroup v2 exposes inactive_file, v1 exposes cache.
    if "inactive_file" in detail:
        usage -= min(detail["inactive_file"], usage)
    elif "cache" in detail:
        usage -= min(detail["cache"], usage)
    return (usage, mem.get("limit") or 0)


def _network(cid: str, stats: dict, now: float) -> dict | None:
    networks = stats.get("networks")
    if not networks:
        # Containers on `network_mode: service:...` (e.g. behind gluetun) report
        # no interfaces of their own — their traffic shows up on the provider.
        _prev_net.pop(cid, None)
        return None
    rx = sum(n.get("rx_bytes", 0) for n in networks.values())
    tx = sum(n.get("tx_bytes", 0) for n in networks.values())
    prev = _prev_net.get(cid)
    _prev_net[cid] = (now, rx, tx)
    rate_rx = rate_tx = 0.0
    if prev and now > prev[0]:
        dt = now - prev[0]
        # A restarted container resets its counters; clamp instead of going negative.
        rate_rx = max(0, rx - prev[1]) / dt
        rate_tx = max(0, tx - prev[2]) / dt
    return {"rx": rx, "tx": tx, "rx_rate": rate_rx, "tx_rate": rate_tx}


def _block_io(stats: dict) -> dict | None:
    entries = (stats.get("blkio_stats") or {}).get("io_service_bytes_recursive")
    if not entries:
        return None  # commonly empty under cgroup v2
    read = sum(e.get("value", 0) for e in entries if e.get("op", "").lower() == "read")
    write = sum(e.get("value", 0) for e in entries if e.get("op", "").lower() == "write")
    return {"read": read, "write": write}


def _identify(meta: dict) -> tuple[str, str, str]:
    """(full name, display name, project).

    Runtipi names containers `<project>-<service>-1`, so at phone width three
    Immich containers all truncate to the same "immich_migra…". The compose
    labels carry the service name on its own, which is what actually
    distinguishes them.
    """
    full = (meta.get("Names") or ["/?"])[0].lstrip("/")
    labels = meta.get("Labels") or {}
    service = labels.get("com.docker.compose.service") or ""
    project = labels.get("com.docker.compose.project") or ""
    return full, (service or full), project


async def _one(cid: str, meta: dict, sem: asyncio.Semaphore) -> dict | None:
    async with sem:
        try:
            r = await client().get(f"/containers/{cid}/stats", params={"stream": "false", "one-shot": "true"})
            r.raise_for_status()
            stats = r.json()
        except Exception:
            return None
    now = time.monotonic()
    used, limit = _memory(stats)
    full, name, project = _identify(meta)
    state = meta.get("State", "")
    health = ((meta.get("Status") or "").split("(")[-1].rstrip(")") if "(" in (meta.get("Status") or "") else None)
    return {
        "id": cid[:12],
        "name": name,
        "full_name": full,
        "project": project,
        "image": meta.get("Image", ""),
        "state": state,
        "status": meta.get("Status", ""),
        "health": health if health in ("healthy", "unhealthy", "health: starting", "starting") else None,
        "created": meta.get("Created", 0),
        "cpu": _cpu_percent(cid, stats),
        "mem_used": used,
        "mem_limit": limit,
        "mem_percent": round(used / limit * 100, 2) if limit else 0.0,
        "pids": (stats.get("pids_stats") or {}).get("current", 0),
        "net": _network(cid, stats, now),
        "blkio": _block_io(stats),
    }


async def sample() -> dict:
    """One full pass: list containers, then fetch stats for the running ones."""
    try:
        r = await client().get("/containers/json", params={"all": "true"})
        r.raise_for_status()
        listing = r.json()
    except Exception as exc:
        return {"error": f"{type(exc).__name__}: {exc}", "containers": []}

    running = [c for c in listing if c.get("State") == "running"]
    sem = asyncio.Semaphore(max(1, config.docker_concurrency))
    results = await asyncio.gather(*(_one(c["Id"], c, sem) for c in running))
    containers = [c for c in results if c]

    # Keep stopped containers visible but without stats, so a crashed service
    # is obvious at a glance rather than silently missing from the list.
    for c in listing:
        if c.get("State") != "running":
            full, name, project = _identify(c)
            containers.append({
                "id": c["Id"][:12],
                "name": name,
                "full_name": full,
                "project": project,
                "image": c.get("Image", ""),
                "state": c.get("State", ""),
                "status": c.get("Status", ""),
                "health": None,
                "created": c.get("Created", 0),
                "cpu": 0.0, "mem_used": 0, "mem_limit": 0, "mem_percent": 0.0,
                "pids": 0, "net": None, "blkio": None,
            })

    live = {c["Id"] for c in running}
    for stale in [k for k in _prev_net if k not in live]:
        _prev_net.pop(stale, None)
    for stale in [k for k in _prev_cpu if k not in live]:
        _prev_cpu.pop(stale, None)

    containers.sort(key=lambda c: (c["state"] != "running", -c["cpu"], c["name"]))
    return {
        "containers": containers,
        "running": len(running),
        "total": len(listing),
    }
