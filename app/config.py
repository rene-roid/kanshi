"""Runtime configuration, all via environment variables.

Defaults are deliberately conservative: this box has 4 cores and ~28 other
containers, so Kanshi should be invisible in `docker stats`.
"""
from __future__ import annotations

import os
from dataclasses import dataclass, field


def _int(name: str, default: int) -> int:
    try:
        return int(os.environ.get(name, "") or default)
    except ValueError:
        return default


def _float(name: str, default: float) -> float:
    try:
        return float(os.environ.get(name, "") or default)
    except ValueError:
        return default


def _list(name: str, default: str) -> list[str]:
    raw = os.environ.get(name, "") or default
    return [p.strip() for p in raw.split(",") if p.strip()]


@dataclass(frozen=True)
class Config:
    # How often the live poller samples vitals + container stats, in seconds.
    poll_interval: float = field(default_factory=lambda: _float("KANSHI_POLL_INTERVAL", 5.0))

    # Stop polling entirely once no browser has been connected for this long.
    # Nobody is looking, so there is no reason to keep waking the Docker daemon.
    idle_timeout: float = field(default_factory=lambda: _float("KANSHI_IDLE_TIMEOUT", 30.0))

    # Max concurrent /stats requests against the Docker socket per tick.
    docker_concurrency: int = field(default_factory=lambda: _int("KANSHI_DOCKER_CONCURRENCY", 8))
    docker_socket: str = field(default_factory=lambda: os.environ.get("KANSHI_DOCKER_SOCKET", "/var/run/docker.sock"))

    # Storage walk. Roots are "label=path" or just "path".
    storage_roots: list[str] = field(default_factory=lambda: _list("KANSHI_STORAGE_ROOTS", "/=/hostfs,/mnt/data=/mnt/data"))
    storage_interval: float = field(default_factory=lambda: _float("KANSHI_STORAGE_INTERVAL", 1800.0))
    # Absolute container-side paths to skip entirely. Their bytes vanish from
    # the totals, so only exclude things you truly don't want counted.
    storage_exclude: list[str] = field(default_factory=lambda: _list("KANSHI_STORAGE_EXCLUDE", ""))
    storage_min_rescan: float = field(default_factory=lambda: _float("KANSHI_STORAGE_MIN_RESCAN", 30.0))

    # Tree pruning, to keep the JSON the phone downloads small.
    tree_depth: int = field(default_factory=lambda: _int("KANSHI_TREE_DEPTH", 4))
    # Children smaller than this fraction of their parent are folded into an
    # aggregate node rather than shipped individually.
    tree_min_fraction: float = field(default_factory=lambda: _float("KANSHI_TREE_MIN_FRACTION", 0.005))
    tree_max_children: int = field(default_factory=lambda: _int("KANSHI_TREE_MAX_CHILDREN", 24))

    host: str = field(default_factory=lambda: os.environ.get("KANSHI_HOST", "0.0.0.0"))
    port: int = field(default_factory=lambda: _int("KANSHI_PORT", 8100))

    def roots(self) -> list[tuple[str, str]]:
        out: list[tuple[str, str]] = []
        for entry in self.storage_roots:
            label, _, path = entry.partition("=")
            if not path:
                label, path = os.path.basename(label.rstrip("/")) or label, label
            out.append((label, path))
        return out


config = Config()
