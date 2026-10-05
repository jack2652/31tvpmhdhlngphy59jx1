"""Cross-worker SSE notifications and subscription-driven snapshot refresh."""
from __future__ import annotations

import asyncio
import json
import logging
import time as monotonic_time
from dataclasses import dataclass
from datetime import date, datetime, time, timedelta
from functools import lru_cache
from typing import Any, Callable
from zoneinfo import ZoneInfo

from app.db import Database
from app.services.market_calendar import is_trading_day, localize, regular_session_bounds

logger = logging.getLogger(__name__)
MARKET_TIMEZONE = ZoneInfo("America/New_York")

# Intraday refresh is deliberately bounded: upstream option-chain providers are not tick feeds.
REGULAR_REFRESH_SECONDS = 20
EXTENDED_REFRESH_SECONDS = 60
CLOSED_REFRESH_SECONDS = 300
SNAPSHOT_FRESH_SECONDS = 15
FAILURE_BACKOFF_SECONDS = 300


@lru_cache(maxsize=128)
def _is_trading_session(day: date) -> bool:
    return is_trading_day(day)


def refresh_interval_for_now(now: datetime | None = None) -> int:
    """Return an adaptive cadence: fast during regular hours, slower off-session/closed."""
    moment = localize(now or datetime.now(MARKET_TIMEZONE))
    bounds = _regular_session_bounds(moment.date().isoformat())
    if bounds and bounds[0] <= moment < bounds[1]:
        return REGULAR_REFRESH_SECONDS
    clock = moment.time()
    if time(4, 0) <= clock < time(20, 0):
        return EXTENDED_REFRESH_SECONDS if _is_trading_session(moment.date()) else CLOSED_REFRESH_SECONDS
    # Alpaca overnight runs Sunday through Thursday evenings and into the following session date.
    session_day = moment.date() + timedelta(days=1) if clock >= time(20, 0) else moment.date()
    return EXTENDED_REFRESH_SECONDS if _is_trading_session(session_day) else CLOSED_REFRESH_SECONDS


@lru_cache(maxsize=64)
def _regular_session_bounds(day: str):
    return regular_session_bounds(datetime.fromisoformat(day).replace(tzinfo=MARKET_TIMEZONE))


def seconds_until_refresh_regime_change(now: datetime | None = None, cadence: int | None = None) -> float:
    """Limit a sleep to the next real market-cadence transition (including holidays/early closes)."""
    moment = localize(now or datetime.now(MARKET_TIMEZONE))
    current_cadence = cadence or refresh_interval_for_now(moment)
    candidates: list[datetime] = []
    for offset in range(8):
        day = moment.date() + timedelta(days=offset)
        for hour in (0, 4, 20):
            boundary = datetime.combine(day, time(hour), tzinfo=MARKET_TIMEZONE)
            if boundary > moment:
                candidates.append(boundary)
        bounds = _regular_session_bounds(day.isoformat())
        if bounds:
            candidates.extend(boundary for boundary in bounds if boundary > moment)
    for boundary in sorted(candidates):
        after_boundary = boundary + timedelta(seconds=1)
        if refresh_interval_for_now(after_boundary) != current_cadence:
            return max(0.1, (boundary - moment).total_seconds())
    # Daily market-clock boundaries make this defensive fallback practically unreachable.
    return float(current_cadence)


@dataclass(frozen=True)
class PushEvent:
    event_id: int
    symbol: str
    kind: str
    created_at: str
    expiration: str | None = None


class PushEventHub:
    """One SQLite event-log poller per worker, fanning updates to local SSE clients."""

    def __init__(
        self,
        database: Database,
        refresh: Callable[..., Any] | None = None,
        interval_seconds: int | None = None,
        poll_seconds: float = 0.5,
    ):
        self.database = database
        self.refresh = refresh
        # Kept only as an optional test/embedding override. Production defaults follow the market clock.
        self.interval_override = max(1, int(interval_seconds)) if interval_seconds is not None else None
        self.poll_seconds = max(0.02, float(poll_seconds))
        self._subscribers: dict[tuple[str, str], set[asyncio.Queue[PushEvent]]] = {}
        self._task: asyncio.Task[None] | None = None
        self._last_id = 0
        self._refresh_tasks: dict[tuple[str, str], asyncio.Task[None]] = {}

    async def subscribe(self, symbol: str, expiration: str | None = None, last_event_id: int = 0):
        normalized = symbol.strip().upper()
        expiry = str(expiration or "")
        subscriber_key = (normalized, expiry)
        queue: asyncio.Queue[PushEvent] = asyncio.Queue(maxsize=32)
        self._subscribers.setdefault(subscriber_key, set()).add(queue)
        self._ensure_poller()
        self._ensure_refresh_task(subscriber_key)
        # Replay only events for this symbol newer than the browser's Last-Event-ID.
        if last_event_id > 0:
            rows = await asyncio.to_thread(self.database.push_events_since, int(last_event_id), normalized, None, expiry or None)
            for row in rows:
                event = PushEvent(int(row["id"]), row["symbol"], row["kind"], row["created_at"], row.get("expiration"))
                if queue.full():
                    try:
                        queue.get_nowait()
                    except asyncio.QueueEmpty:
                        pass
                queue.put_nowait(event)
        try:
            yield queue
        finally:
            subscribers = self._subscribers.get(subscriber_key)
            if subscribers:
                subscribers.discard(queue)
                if not subscribers:
                    self._subscribers.pop(subscriber_key, None)
            if not self._subscribers and self._task:
                self._task.cancel()
                self._task = None
            if not self._subscribers.get(subscriber_key):
                task = self._refresh_tasks.pop(subscriber_key, None)
                if task and not task.done():
                    task.cancel()

    def _ensure_poller(self) -> None:
        if self._task is None or self._task.done():
            self._last_id = self.database.latest_push_event_id()
            self._task = asyncio.create_task(self._poll(), name="sse-push-event-poller")

    def _ensure_refresh_task(self, subscription: tuple[str, str]) -> None:
        if self.refresh is None:
            return
        task = self._refresh_tasks.get(subscription)
        if task is None or task.done() or task.cancelling():
            self._refresh_tasks[subscription] = asyncio.create_task(
                self._refresh_loop(subscription),
                name=f"push-refresh-{subscription[0]}-{subscription[1]}"[:64],
            )

    async def _settle_cancelled_claim(
        self,
        claim_task: asyncio.Task[Any],
        symbol: str,
        expiration: str | None,
    ) -> None:
        try:
            claimed = await claim_task
        except Exception:
            return
        if claimed:
            await asyncio.to_thread(self.database.finish_push_refresh, symbol, expiration)

    async def _settle_cancelled_refresh(
        self,
        refresh_task: asyncio.Task[Any],
        symbol: str,
        expiration: str | None,
        cadence: int,
    ) -> None:
        try:
            result = await refresh_task
            if result and (result.get("stale") or result.get("deferred")):
                raise RuntimeError(result.get("warning") or "refresh deferred")
        except Exception:
            delay = min(FAILURE_BACKOFF_SECONDS, max(cadence, 2 * cadence))
            await asyncio.to_thread(self.database.defer_push_refresh, symbol, expiration, delay)
            logger.debug("订阅标的 %s %s 的取消中刷新最终失败", symbol, expiration, exc_info=True)
        else:
            await asyncio.to_thread(self.database.finish_push_refresh, symbol, expiration)

    async def _refresh_loop(self, subscription: tuple[str, str]) -> None:
        symbol, expiry = subscription
        failure_delay = 0
        try:
            while subscription in self._subscribers:
                iteration_started = monotonic_time.monotonic()
                cadence = self.interval_override or refresh_interval_for_now()
                # The lease enforces cross-worker cadence; per-expiry subscriptions avoid refreshing
                # anything that no connected client is currently displaying.
                claim_task = asyncio.create_task(asyncio.to_thread(
                    self.database.claim_push_refresh,
                    symbol,
                    expiry or None,
                    cadence,
                ))
                try:
                    claimed = await asyncio.shield(claim_task)
                except asyncio.CancelledError:
                    asyncio.create_task(self._settle_cancelled_claim(claim_task, symbol, expiry or None))
                    raise
                delay = cadence
                wake_at_market_change = False
                if claimed and self.refresh is not None:
                    failed = False
                    refresh_task = asyncio.create_task(asyncio.to_thread(
                        self.refresh,
                        symbol,
                        expiry or None,
                        # Server market cadence is also the snapshot-cache freshness threshold.
                        max_age_seconds=SNAPSHOT_FRESH_SECONDS,
                    ))
                    try:
                        result = await asyncio.shield(refresh_task)
                        if result and (result.get("stale") or result.get("deferred")):
                            raise RuntimeError(result.get("warning") or "refresh deferred")
                        failure_delay = 0
                        wake_at_market_change = True
                    except asyncio.CancelledError:
                        # Keep the lease until the worker thread really exits; settle it asynchronously afterward.
                        failed = True
                        asyncio.create_task(self._settle_cancelled_refresh(
                            refresh_task,
                            symbol,
                            expiry or None,
                            cadence,
                        ))
                        raise
                    except Exception:
                        failed = True
                        local_delay = min(FAILURE_BACKOFF_SECONDS, max(cadence, failure_delay * 2 or cadence))
                        failure_delay = await asyncio.to_thread(
                            self.database.defer_push_refresh,
                            symbol,
                            expiry or None,
                            local_delay,
                        )
                        delay = failure_delay
                        logger.debug("订阅标的 %s %s 刷新失败，%s 秒后重试", symbol, expiry, failure_delay, exc_info=True)
                    finally:
                        if not failed:
                            await asyncio.to_thread(self.database.finish_push_refresh, symbol, expiry or None)
                else:
                    # Honor the shared wait window without retrying the upstream lease on every event-loop tick.
                    shared_wait = await asyncio.to_thread(
                        self.database.push_refresh_wait_seconds,
                        symbol,
                        expiry or None,
                        cadence,
                    )
                    # Sleep until the shared lease/cooldown expires; cancelled in-flight work settles
                    # its own lease when the thread finishes, while dead workers have a bounded lease.
                    delay = max(0.1, shared_wait)
                    wake_at_market_change = shared_wait <= cadence
                elapsed = monotonic_time.monotonic() - iteration_started
                active_cadence = refresh_interval_for_now()
                if self.interval_override is None and claimed and wake_at_market_change and active_cadence != cadence:
                    delay = active_cadence
                sleep_seconds = max(0.1, delay - elapsed)
                if self.interval_override is None and wake_at_market_change:
                    sleep_seconds = min(sleep_seconds, seconds_until_refresh_regime_change(cadence=active_cadence))
                await asyncio.sleep(sleep_seconds)
        except asyncio.CancelledError:
            pass
        finally:
            if self._refresh_tasks.get(subscription) is asyncio.current_task():
                self._refresh_tasks.pop(subscription, None)

    async def _poll(self) -> None:
        try:
            while self._subscribers:
                try:
                    # Updates are fetched from a shared SQLite sequence, so events generated by any worker
                    # reach every worker that owns subscribers for that symbol.
                    rows = await asyncio.to_thread(self.database.push_events_since, self._last_id, None)
                    for row in rows:
                        event = PushEvent(int(row["id"]), row["symbol"], row["kind"], row["created_at"], row.get("expiration"))
                        self._last_id = max(self._last_id, event.event_id)
                        for key, queues in tuple(self._subscribers.items()):
                            if key[0] != event.symbol or (event.kind == "snapshot" and event.expiration and key[1] and event.expiration != key[1]):
                                continue
                            for queue in tuple(queues):
                                if queue.full():
                                    try:
                                        queue.get_nowait()
                                    except asyncio.QueueEmpty:
                                        pass
                                queue.put_nowait(event)
                    await asyncio.sleep(self.poll_seconds)
                except asyncio.CancelledError:
                    raise
                except Exception:
                    logger.debug("SSE 事件轮询失败", exc_info=True)
                    await asyncio.sleep(1)
        except asyncio.CancelledError:
            pass

    @staticmethod
    def format_event(event: PushEvent) -> str:
        payload = json.dumps({"symbol": event.symbol, "kind": event.kind, "created_at": event.created_at, "expiration": event.expiration}, ensure_ascii=False)
        return f"id: {event.event_id}\nevent: update\ndata: {payload}\n\n"
