"""Kanshi — at-a-glance vitals for a single homeserver."""
from __future__ import annotations

import asyncio
import contextlib
import json
import time
from pathlib import Path

from fastapi import FastAPI
from fastapi.responses import FileResponse, JSONResponse, StreamingResponse
from fastapi.staticfiles import StaticFiles

from . import dockerstats, storage, vitals
from .config import config

WEB = Path(__file__).resolve().parent.parent / "web"

_subscribers: set[asyncio.Queue] = set()
_latest: dict = {"vitals": None, "docker": None}
_wake = asyncio.Event()
_last_seen = 0.0


async def _sample_once() -> dict:
    host, containers = await asyncio.gather(
        asyncio.to_thread(vitals.sample),
        dockerstats.sample(),
    )
    _latest["vitals"], _latest["docker"] = host, containers
    return {"vitals": host, "docker": containers}


async def _seed() -> None:
    """Fill the delta baselines so the first frame shows real numbers.

    Both CPU readings are differences against a previous sample, so a cold
    poller would otherwise publish a screen of zeros. One throwaway pass plus a
    short gap costs ~0.1s and makes the first frame the user sees correct.
    """
    vitals.prime()
    try:
        await dockerstats.sample()
    except Exception:
        pass
    await asyncio.sleep(1.0)
    vitals.prime()


async def _poller() -> None:
    await _seed()
    while True:
        idle = not _subscribers and (time.monotonic() - _last_seen) > config.idle_timeout
        if idle:
            # Nobody is watching: stop touching the Docker socket entirely and
            # wait to be woken by a new subscriber or a REST request.
            _wake.clear()
            with contextlib.suppress(asyncio.TimeoutError):
                await asyncio.wait_for(_wake.wait(), timeout=60.0)
            await _seed()
            continue

        try:
            payload = await _sample_once()
        except Exception as exc:
            payload = {"error": f"{type(exc).__name__}: {exc}"}
        for queue in list(_subscribers):
            if queue.full():
                with contextlib.suppress(asyncio.QueueEmpty):
                    queue.get_nowait()  # drop the stale frame, never block the poller
            with contextlib.suppress(asyncio.QueueFull):
                queue.put_nowait(payload)
        await asyncio.sleep(config.poll_interval)


def _touch() -> None:
    global _last_seen
    _last_seen = time.monotonic()
    _wake.set()


@contextlib.asynccontextmanager
async def lifespan(app: FastAPI):
    tasks = [asyncio.create_task(_poller()), asyncio.create_task(storage.loop())]
    try:
        yield
    finally:
        for task in tasks:
            task.cancel()
        await asyncio.gather(*tasks, return_exceptions=True)
        await dockerstats.close()


app = FastAPI(title="Kanshi", lifespan=lifespan, docs_url=None, redoc_url=None)


@app.get("/api/vitals")
async def api_vitals():
    _touch()
    if _latest["vitals"] is None:
        await _sample_once()
    return _latest["vitals"]


@app.get("/api/containers")
async def api_containers():
    _touch()
    if _latest["docker"] is None:
        await _sample_once()
    return _latest["docker"]


@app.get("/api/storage")
async def api_storage():
    snap = storage.snapshot()
    if snap["scanned_at"] is None and not snap["scanning"]:
        await storage.scan(force=True)
        snap = storage.snapshot()
    return snap


@app.post("/api/storage/rescan")
async def api_rescan():
    return await storage.scan()


@app.get("/api/config")
async def api_config():
    return {
        "poll_interval": config.poll_interval,
        "storage_interval": config.storage_interval,
        "tree_depth": config.tree_depth,
    }


@app.get("/healthz")
async def healthz():
    return {"ok": True}


@app.get("/api/stream")
async def api_stream():
    queue: asyncio.Queue = asyncio.Queue(maxsize=1)
    _subscribers.add(queue)
    _touch()

    async def events():
        try:
            if _latest["vitals"] is not None:
                yield f"data: {json.dumps(_latest)}\n\n"
            while True:
                try:
                    payload = await asyncio.wait_for(queue.get(), timeout=25.0)
                except asyncio.TimeoutError:
                    yield ": keepalive\n\n"  # keeps mobile proxies from closing the stream
                    continue
                _touch()
                yield f"data: {json.dumps(payload)}\n\n"
        finally:
            _subscribers.discard(queue)

    return StreamingResponse(
        events(),
        media_type="text/event-stream",
        headers={"Cache-Control": "no-cache, no-transform", "X-Accel-Buffering": "no", "Connection": "keep-alive"},
    )


@app.get("/")
async def index():
    return FileResponse(WEB / "index.html", headers={"Cache-Control": "no-cache"})


app.mount("/static", StaticFiles(directory=WEB), name="static")
