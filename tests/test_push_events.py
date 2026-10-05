from __future__ import annotations

import asyncio
import json
import sqlite3
from pathlib import Path

from fastapi import FastAPI
from fastapi.testclient import TestClient

from app.api import create_router
from app.config import Settings
from app.db import Database
from app.services.push_events import (
    CLOSED_REFRESH_SECONDS,
    EXTENDED_REFRESH_SECONDS,
    REGULAR_REFRESH_SECONDS,
    PushEvent,
    PushEventHub,
    refresh_interval_for_now,
    seconds_until_refresh_regime_change,
)


class EmptyProvider:
    def normalize_symbol(self, value: str) -> str:
        return value.upper()


class EmptySnapshots:
    def refresh(self, *args, **kwargs):
        return {"skipped": True}


def test_push_event_log_filters_symbols_and_expirations(tmp_path: Path):
    db = Database(tmp_path / "push.db")
    first = db.publish_push_event("aapl", "snapshot", "2026-12-18")
    db.publish_push_event("MSFT", "snapshot", "2026-12-18")
    db.publish_push_event("AAPL", "fair_value")
    assert db.push_events_since(first - 1, "AAPL", expiration="2026-12-18")[0]["kind"] == "snapshot"
    assert [event["kind"] for event in db.push_events_since(first - 1, "AAPL")] == ["snapshot", "fair_value"]
    assert db.latest_push_event_id() >= first + 2


def test_market_aware_refresh_cadence():
    from datetime import datetime
    from zoneinfo import ZoneInfo

    eastern = ZoneInfo("America/New_York")
    assert refresh_interval_for_now(datetime(2026, 10, 5, 10, 0, tzinfo=eastern)) == REGULAR_REFRESH_SECONDS
    assert refresh_interval_for_now(datetime(2026, 10, 5, 8, 0, tzinfo=eastern)) == EXTENDED_REFRESH_SECONDS
    assert refresh_interval_for_now(datetime(2026, 10, 9, 21, 0, tzinfo=eastern)) == CLOSED_REFRESH_SECONDS
    assert refresh_interval_for_now(datetime(2026, 10, 3, 12, 0, tzinfo=eastern)) == CLOSED_REFRESH_SECONDS
    assert refresh_interval_for_now(datetime(2026, 10, 3, 21, 0, tzinfo=eastern)) == CLOSED_REFRESH_SECONDS


def test_refresh_regime_sleep_stops_at_market_transitions():
    from datetime import datetime
    from zoneinfo import ZoneInfo

    eastern = ZoneInfo("America/New_York")
    before_open = datetime(2026, 10, 5, 9, 29, 50, tzinfo=eastern)
    assert refresh_interval_for_now(before_open) == EXTENDED_REFRESH_SECONDS
    assert 9 <= seconds_until_refresh_regime_change(before_open) <= 10

    before_close = datetime(2026, 10, 5, 15, 59, 50, tzinfo=eastern)
    assert refresh_interval_for_now(before_close) == REGULAR_REFRESH_SECONDS
    assert 9 <= seconds_until_refresh_regime_change(before_close) <= 10


def test_push_refresh_state_migrates_existing_rows(tmp_path: Path):
    path = tmp_path / "legacy-push-state.db"
    with sqlite3.connect(path) as connection:
        connection.execute(
            "CREATE TABLE push_refresh_state (symbol TEXT NOT NULL, expiration TEXT NOT NULL DEFAULT '', started_at TEXT NOT NULL, PRIMARY KEY(symbol, expiration))"
        )
        connection.execute(
            "INSERT INTO push_refresh_state(symbol, expiration, started_at) VALUES ('AAPL', '2026-12-18', '2026-01-01T00:00:00+00:00')"
        )
    db = Database(path)
    with db.connect() as connection:
        columns = {row[1] for row in connection.execute("PRAGMA table_info(push_refresh_state)")}
        row = connection.execute("SELECT retry_delay_seconds, last_interval_seconds FROM push_refresh_state").fetchone()
    assert {"retry_after", "retry_delay_seconds", "last_interval_seconds", "lease_until"} <= columns
    assert row["retry_delay_seconds"] == 0
    assert row["last_interval_seconds"] == 0
    assert db.claim_push_refresh("AAPL", "2026-12-18", 20)


def test_push_refresh_claim_is_unique_per_symbol_and_expiry(tmp_path: Path):
    db = Database(tmp_path / "claims.db")
    assert db.claim_push_refresh("aapl", "2026-12-18", 60)
    assert not db.claim_push_refresh("AAPL", "2026-12-18", 60)
    assert db.claim_push_refresh("AAPL", "2027-01-15", 60)


def test_push_refresh_backoff_is_shared_across_worker_instances(tmp_path: Path):
    db = Database(tmp_path / "backoff.db")
    worker_a = Database(tmp_path / "backoff.db")
    worker_b = Database(tmp_path / "backoff.db")
    assert worker_a.claim_push_refresh("AAPL", "2026-12-18", 1)
    assert worker_a.defer_push_refresh("AAPL", "2026-12-18", 30) == 30
    assert not worker_b.claim_push_refresh("AAPL", "2026-12-18", 1)
    assert worker_b.push_refresh_wait_seconds("AAPL", "2026-12-18", 1) > 20
    with worker_a.connect() as connection:
        connection.execute("UPDATE push_refresh_state SET retry_after=NULL, started_at='2000-01-01T00:00:00+00:00' WHERE symbol='AAPL'")
    assert worker_b.claim_push_refresh("AAPL", "2026-12-18", 1)
    assert worker_b.defer_push_refresh("AAPL", "2026-12-18", 30) == 60
    assert worker_a.defer_push_refresh("AAPL", "2026-12-18", 30) == 120
    assert worker_a.defer_push_refresh("AAPL", "2026-12-18", 30) == 240
    assert worker_a.defer_push_refresh("AAPL", "2026-12-18", 30) == 300
    assert worker_a.defer_push_refresh("AAPL", "2026-12-18", 30) == 300
    assert not worker_a.claim_push_refresh("AAPL", "2026-12-18", 1)
    with worker_a.connect() as connection:
        connection.execute("UPDATE push_refresh_state SET retry_after=NULL, started_at='2000-01-01T00:00:00+00:00' WHERE symbol='AAPL'")
    assert worker_a.claim_push_refresh("AAPL", "2026-12-18", 1)
    worker_a.finish_push_refresh("AAPL", "2026-12-18")
    with worker_b.connect() as connection:
        row = connection.execute("SELECT retry_delay_seconds FROM push_refresh_state WHERE symbol='AAPL'").fetchone()
    assert row["retry_delay_seconds"] == 0


def test_sse_subscription_starts_refresh_for_only_the_active_expiry(tmp_path: Path):
    db = Database(tmp_path / "active.db")
    refreshed: list[tuple[str, str | None]] = []
    completed = asyncio.Event()

    def refresh(symbol: str, expiration: str | None = None, **kwargs):
        refreshed.append((symbol, expiration))
        completed.set()

    hub = PushEventHub(db, refresh=refresh, interval_seconds=60)

    async def scenario():
        stream = hub.subscribe("AAPL", "2026-12-18")
        await anext(stream)
        await asyncio.wait_for(completed.wait(), timeout=1)
        await stream.aclose()

    asyncio.run(scenario())
    assert refreshed == [("AAPL", "2026-12-18")]
    assert hub._refresh_tasks == {}


def test_cancelled_subscription_retains_lease_until_threaded_refresh_finishes(tmp_path: Path):
    from threading import Event

    db = Database(tmp_path / "cancel.db")
    started = Event()
    release = Event()
    finished = Event()

    def refresh(*_args, **_kwargs):
        started.set()
        release.wait(timeout=3)
        finished.set()
        return {"ok": True}

    hub = PushEventHub(db, refresh=refresh, interval_seconds=60)

    async def scenario():
        stream = hub.subscribe("AAPL", "2026-12-18")
        await anext(stream)
        await asyncio.to_thread(started.wait, 1)
        await stream.aclose()
        assert not db.claim_push_refresh("AAPL", "2026-12-18", 60)
        release.set()
        assert await asyncio.to_thread(finished.wait, 1)
        for _ in range(100):
            with db.connect() as connection:
                row = connection.execute(
                    "SELECT lease_until FROM push_refresh_state WHERE symbol='AAPL' AND expiration='2026-12-18'"
                ).fetchone()
            if row["lease_until"] is None:
                break
            await asyncio.sleep(0.01)
        assert row["lease_until"] is None
        assert not db.claim_push_refresh("AAPL", "2026-12-18", 60)

    asyncio.run(scenario())


def test_push_hubs_share_cross_worker_event_log(tmp_path: Path):
    path = tmp_path / "workers.db"
    publisher = Database(path)
    worker_db = Database(path)
    worker_hub = PushEventHub(worker_db, poll_seconds=0.02)

    async def scenario():
        stream = worker_hub.subscribe("AAPL", "2026-12-18")
        queue = await anext(stream)
        publisher.publish_push_event("AAPL", "snapshot", "2026-12-18")
        event = await asyncio.wait_for(queue.get(), timeout=1)
        await stream.aclose()
        return event

    event = asyncio.run(scenario())
    assert event.symbol == "AAPL"
    assert event.kind == "snapshot"


def test_push_hub_fans_out_persisted_event_and_formats_sse(tmp_path: Path):
    db = Database(tmp_path / "hub.db")
    hub = PushEventHub(db, poll_seconds=0.02)

    async def scenario():
        stream = hub.subscribe("AAPL", "2026-12-18")
        queue = await anext(stream)
        db.publish_push_event("AAPL", "snapshot", "2026-12-18")
        event = await asyncio.wait_for(queue.get(), timeout=1)
        await stream.aclose()
        return event

    event = asyncio.run(scenario())
    assert event.kind == "snapshot"
    assert event.expiration == "2026-12-18"
    frame = hub.format_event(event)
    assert f"id: {event.event_id}\nevent: update\n" in frame
    assert json.loads(frame.split("data: ", 1)[1].splitlines()[0])["symbol"] == "AAPL"


def test_events_endpoint_uses_sse_and_auth_guard(tmp_path: Path):
    db = Database(tmp_path / "route.db")
    settings = Settings(
        database_path=tmp_path / "route.db", proxy_url=None, default_symbols=("AAPL",),
        raw_retention_days=30,
        scheduler_enabled=False, access_key="secret",
    )
    app = FastAPI()
    from app.api import install_access_guard
    install_access_guard(app, settings)
    router = create_router(db, EmptySnapshots(), EmptyProvider(), settings)
    app.include_router(router)
    with TestClient(app) as client:
        denied = client.get("/api/events/AAPL")
        assert denied.status_code == 403
        event_route = next(route for route in router.routes if getattr(route, "path", None) == "/api/events/{stock_symbol}")
        assert "GET" in event_route.methods
        assert db.latest_push_event_id() == 0
