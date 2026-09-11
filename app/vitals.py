"""Host-wide CPU / memory / disk / network vitals via psutil.

Kanshi runs with `network_mode: host` and without lxcfs, so /proc/stat,
/proc/meminfo, /proc/diskstats and /proc/net/dev all report real host values
and psutil needs no special configuration.
"""
from __future__ import annotations

import os
import time

import psutil

from .config import config

_last_net: tuple[float, int, int] | None = None
_last_disk: tuple[float, int, int] | None = None
_last_rates: dict[str, tuple[float, float]] = {"net": (0.0, 0.0), "disk": (0.0, 0.0)}
_last_cpu_call = 0.0

# Below this gap the counter deltas are too small to divide by: a 20ms window
# turns a routine 2MB read into a fake 100MB/s spike. Hold the previous rate.
MIN_DT = 0.5


def prime() -> None:
    """Seed psutil's internal counters so the first sample isn't all zeros."""
    global _last_cpu_call
    psutil.cpu_percent(percpu=True, interval=None)
    _last_cpu_call = time.monotonic()
    _rate_net()
    _rate_disk()


def _cpu_cores() -> list[float]:
    """Per-core utilisation since the previous call.

    psutil's interval=None form is a delta against the last call, so if that
    call was moments ago every core reads 0%. When the gap is too short to be
    meaningful, measure a real (short, blocking) window instead — sample() runs
    in a worker thread, so this never stalls the event loop.
    """
    global _last_cpu_call
    gap = time.monotonic() - _last_cpu_call
    cores = psutil.cpu_percent(percpu=True, interval=None if gap >= MIN_DT else 0.25)
    _last_cpu_call = time.monotonic()
    return cores


def _rate_net() -> tuple[float, float]:
    global _last_net
    now = time.monotonic()
    try:
        c = psutil.net_io_counters()
    except Exception:
        return (0.0, 0.0)
    prev = _last_net
    if prev is not None and now - prev[0] < MIN_DT:
        return _last_rates["net"]
    _last_net = (now, c.bytes_recv, c.bytes_sent)
    if prev is None:
        return (0.0, 0.0)
    dt = now - prev[0]
    # Counters are 64-bit but can reset if an interface disappears; clamp.
    rates = (max(0, c.bytes_recv - prev[1]) / dt, max(0, c.bytes_sent - prev[2]) / dt)
    _last_rates["net"] = rates
    return rates


def _rate_disk() -> tuple[float, float]:
    global _last_disk
    now = time.monotonic()
    try:
        c = psutil.disk_io_counters()
    except Exception:
        return (0.0, 0.0)
    if c is None:
        return (0.0, 0.0)
    prev = _last_disk
    if prev is not None and now - prev[0] < MIN_DT:
        return _last_rates["disk"]
    _last_disk = (now, c.read_bytes, c.write_bytes)
    if prev is None:
        return (0.0, 0.0)
    dt = now - prev[0]
    rates = (max(0, c.read_bytes - prev[1]) / dt, max(0, c.write_bytes - prev[2]) / dt)
    _last_rates["disk"] = rates
    return rates


def _temperature() -> float | None:
    """Best-effort package temperature; needs /sys mounted (Docker does by default)."""
    try:
        temps = psutil.sensors_temperatures()
    except Exception:
        return None
    for key in ("coretemp", "k10temp", "cpu_thermal", "acpitz", "zenpower"):
        for entry in temps.get(key, []):
            if entry.current:
                return round(entry.current, 1)
    for entries in temps.values():
        for entry in entries:
            if entry.current:
                return round(entry.current, 1)
    return None


def filesystems() -> list[dict]:
    """Usage for each configured storage root, read straight off statvfs."""
    out = []
    for label, path in config.roots():
        try:
            st = os.statvfs(path)
        except OSError:
            continue
        total = st.f_blocks * st.f_frsize
        # f_bavail excludes root-reserved blocks, so used+free won't equal total.
        # Report "used" the way df does, against the non-reserved capacity.
        free = st.f_bavail * st.f_frsize
        used = total - st.f_bfree * st.f_frsize
        if total <= 0:
            continue
        out.append({
            "label": label,
            "path": path,
            "total": total,
            "used": used,
            "free": free,
            "percent": round(used / (used + free) * 100, 1) if used + free else 0.0,
        })
    return out


def sample() -> dict:
    per_core = _cpu_cores()
    mem = psutil.virtual_memory()
    swap = psutil.swap_memory()
    rx, tx = _rate_net()
    read, write = _rate_disk()
    try:
        load1, load5, load15 = os.getloadavg()
    except OSError:
        load1 = load5 = load15 = 0.0

    return {
        "ts": time.time(),
        "cpu": {
            "percent": round(sum(per_core) / len(per_core), 1) if per_core else 0.0,
            "cores": [round(c, 1) for c in per_core],
            "load": [round(load1, 2), round(load5, 2), round(load15, 2)],
            "count": len(per_core),
            "temp": _temperature(),
        },
        "memory": {
            "total": mem.total,
            "used": mem.total - mem.available,
            "available": mem.available,
            "cached": getattr(mem, "cached", 0) + getattr(mem, "buffers", 0),
            "percent": round((mem.total - mem.available) / mem.total * 100, 1) if mem.total else 0.0,
        },
        "swap": {
            "total": swap.total,
            "used": swap.used,
            "percent": round(swap.percent, 1),
        },
        "network": {"rx": rx, "tx": tx},
        "diskio": {"read": read, "write": write},
        "filesystems": filesystems(),
        "uptime": time.time() - psutil.boot_time(),
    }
