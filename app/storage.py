"""Filelight-style directory size tree.

The walk is a single os.scandir pass per root, run in a worker thread on a slow
timer and cached — never recomputed per refresh. Sizes come from st_blocks so
they match `du` (actual blocks on disk) rather than apparent size, and hardlinked
files are counted once.
"""
from __future__ import annotations

import asyncio
import heapq
import os
import time

from .config import config

# Never ship a tile smaller than this, regardless of the fraction threshold.
MIN_ABSOLUTE = 4 * 1024 * 1024
# Largest individual files kept per directory; the rest are aggregated.
TOP_FILES = 12

_state: dict = {"roots": [], "scanned_at": None, "duration": None, "scanning": False, "error": None}
_lock = asyncio.Lock()
_last_scan = 0.0


def _walk(root_path: str, max_depth: int) -> tuple[dict, str, int]:
    """Iterative DFS. Returns (nodes-by-path, root-path, unreadable-dir-count).

    Only directories within `max_depth` of the root get a node of their own.
    Anything deeper is still fully traversed and counted, but its bytes roll up
    into the nearest retained ancestor — the tree is pruned to this depth before
    it is served anyway, so materialising a node per directory just burns memory.
    (On this host that is ~2.3k retained nodes instead of ~96k.)
    """
    root_dev = os.stat(root_path).st_dev
    excluded = set(config.storage_exclude)
    unreadable = 0
    # Packed into one int rather than a (dev, ino) tuple: overlay2 hardlinks
    # everything, so this set reaches six figures and tuples cost ~3x the ints.
    seen_inodes: set[int] = set()
    nodes: dict[str, dict] = {}
    order: list[str] = []
    # (path, depth, nearest retained ancestor path)
    stack: list[tuple[str, int, str | None]] = [(root_path, 0, None)]

    while stack:
        cur, depth, anchor = stack.pop()
        if cur in excluded:
            continue

        if depth <= max_depth:
            node = {
                "name": os.path.basename(cur.rstrip("/")) or cur,
                "self": 0,        # bytes of files directly inside this dir
                "deep": 0,        # bytes below the retained depth
                "children": [],   # child directory paths that have their own node
                "files": [],      # min-heap of (size, name), capped at TOP_FILES
            }
            nodes[cur] = node
            order.append(cur)
            anchor = cur
        else:
            node = nodes[anchor]

        try:
            it = os.scandir(cur)
        except OSError:
            # A directory we cannot read would otherwise silently vanish from
            # the totals — count it so the UI can say the tree is incomplete
            # rather than quietly under-reporting.
            unreadable += 1
            continue
        with it:
            while True:
                try:
                    entry = next(it)
                except StopIteration:
                    break
                except OSError:
                    break
                try:
                    if entry.is_symlink():
                        continue  # never follow; the link itself is negligible
                    st = entry.stat(follow_symlinks=False)
                    if entry.is_dir(follow_symlinks=False):
                        # Don't cross into other filesystems — each root is
                        # walked separately, so we'd otherwise double-count.
                        if st.st_dev != root_dev:
                            continue
                        stack.append((entry.path, depth + 1, anchor))
                        if depth + 1 <= max_depth:
                            node["children"].append(entry.path)
                    elif entry.is_file(follow_symlinks=False):
                        if st.st_nlink > 1:
                            key = (st.st_dev << 48) | st.st_ino
                            if key in seen_inodes:
                                continue
                            seen_inodes.add(key)
                        size = st.st_blocks * 512
                        if depth <= max_depth:
                            node["self"] += size
                            if len(node["files"]) < TOP_FILES:
                                heapq.heappush(node["files"], (size, entry.name))
                            elif size > node["files"][0][0]:
                                heapq.heapreplace(node["files"], (size, entry.name))
                        else:
                            node["deep"] += size
                except OSError:
                    continue

    # Children always follow their parent in a DFS pre-order, so walking the
    # order backwards guarantees every child is totalled before its parent.
    for path in reversed(order):
        node = nodes[path]
        node["size"] = node["self"] + node["deep"] + sum(nodes[c]["size"] for c in node["children"])
    return nodes, root_path, unreadable


def _to_tree(nodes: dict, path: str, depth: int) -> dict:
    node = nodes[path]
    total = node["size"]
    out = {"name": node["name"], "path": path, "size": total, "kind": "dir"}

    if depth <= 0 or total <= 0:
        return out

    entries: list[dict] = []
    for child in node["children"]:
        entries.append({"_path": child, "size": nodes[child]["size"], "kind": "dir"})
    kept_files = sorted(node["files"], reverse=True)
    file_bytes_kept = 0
    for size, name in kept_files:
        entries.append({"name": name, "size": size, "kind": "file"})
        file_bytes_kept += size
    remainder = node["self"] + node["deep"] - file_bytes_kept
    if remainder > 0:
        entries.append({"name": "other files", "size": remainder, "kind": "rest"})

    entries.sort(key=lambda e: -e["size"])
    threshold = max(total * config.tree_min_fraction, MIN_ABSOLUTE)

    children, folded, folded_count = [], 0, 0
    for entry in entries:
        if entry["size"] < threshold or len(children) >= config.tree_max_children:
            folded += entry["size"]
            folded_count += 1
            continue
        if entry["kind"] == "dir":
            children.append(_to_tree(nodes, entry["_path"], depth - 1))
        else:
            children.append({"name": entry["name"], "size": entry["size"], "kind": entry["kind"]})
    if folded > 0:
        # Keep the aggregate so child sizes still sum to the parent — otherwise
        # the treemap silently under-reports and the areas lie.
        children.append({"name": f"{folded_count} smaller items", "size": folded, "kind": "rest"})

    if children:
        out["children"] = children
    return out


def _scan_sync() -> list[dict]:
    # The walk is a long burst of syscalls competing with the live poller for
    # this container's CPU quota. Deprioritise it so a rescan never makes the
    # at-a-glance numbers stutter — the walk finishing a few seconds later is
    # invisible, a frozen dashboard is not.
    try:
        os.nice(10)
    except OSError:
        pass

    trees = []
    for label, path in config.roots():
        if not os.path.isdir(path):
            continue
        started = time.monotonic()
        nodes, root, unreadable = _walk(path, config.tree_depth)
        tree = _to_tree(nodes, root, config.tree_depth)
        tree["name"] = label
        tree["root"] = path
        tree["unreadable"] = unreadable
        tree["walk_seconds"] = round(time.monotonic() - started, 2)
        trees.append(tree)
    return trees


async def scan(force: bool = False) -> dict:
    """Rescan the roots. Rate-limited unless `force` is set by the timer."""
    global _last_scan
    if _lock.locked():
        return _state
    since = time.monotonic() - _last_scan
    if not force and _last_scan and since < config.storage_min_rescan:
        return _state

    async with _lock:
        _state["scanning"] = True
        started = time.time()
        try:
            trees = await asyncio.to_thread(_scan_sync)
            _state.update(roots=trees, scanned_at=started, error=None,
                          duration=round(time.time() - started, 2))
        except Exception as exc:
            _state["error"] = f"{type(exc).__name__}: {exc}"
        finally:
            _state["scanning"] = False
            _last_scan = time.monotonic()
    return _state


def snapshot() -> dict:
    return _state


async def loop() -> None:
    """Background refresher — slow by default (15 min)."""
    while True:
        await scan(force=True)
        await asyncio.sleep(max(60.0, config.storage_interval))
