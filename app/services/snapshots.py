"""快照采集服务。"""

from __future__ import annotations

import logging
import threading
import time
from datetime import date, datetime, timedelta, timezone
from typing import Any
from uuid import uuid4
from zoneinfo import ZoneInfo

from app.db import Database, iso, parse_sessions
from app.providers.market import ProviderError, MarketDataProvider
from app.runtime import low_memory_enabled
from app.services.concurrency import HeavyWorkGate, SingleFlight, UpstreamBusyError, get_heavy_gate

logger = logging.getLogger(__name__)

# 美股期权到期以美国东部时间为准，过滤到期日时使用美东当天日期。
MARKET_TIMEZONE = ZoneInfo("America/New_York")

# 跨期限 Gamma 窗口的单到期日新鲜期（秒）：窗口刷新很慢，短期内的重复请求直接跳过。
# 与页面自动刷新同一口径。120 秒会让 Gamma 窗口比现价多停一轮。
WINDOW_FRESH_SECONDS = 60
# 页面请求有明确超时；锁竞争应更早回退，避免请求线程堆积到一分钟以上。
REFRESH_LOCK_TIMEOUT_SECONDS = 10
# 低内存模式仍保持单槽位，但首屏刷新可以短暂等待 Gamma 归还槽位；只等待不叠加内存。
LOW_MEMORY_REFRESH_WAIT_SECONDS = 3
REFRESH_LEASE_SECONDS = 180
# 只合并刚刚写完的并发刷新。再留 60 秒会和页面新鲜期叠成一次空刷新。
REFRESH_COALESCE_SECONDS = 15


def market_today() -> date:
    """返回美东当天日期。"""
    return datetime.now(MARKET_TIMEZONE).date()


def snapshot_age_seconds(fetched_at: str | None) -> float | None:
    """快照年龄（秒）：时间戳缺失或无法解析返回 None，时钟偏差导致的负值按 0 处理。"""
    if not fetched_at:
        return None
    try:
        moment = datetime.fromisoformat(fetched_at)
    except ValueError:
        return None
    if moment.tzinfo is None:
        moment = moment.replace(tzinfo=timezone.utc)
    return max((datetime.now(timezone.utc) - moment).total_seconds(), 0.0)


def has_valid_two_sided_quotes(rows: list[dict[str, Any]] | None) -> bool:
    """判断期权链是否至少包含一条可用于模型估算的有效买卖价。"""
    for row in rows or []:
        try:
            bid = float(row.get("bid"))
            ask = float(row.get("ask"))
        except (TypeError, ValueError):
            continue
        if bid > 0 and ask >= bid:
            return True
    return False


def active_expirations(values: list[str]) -> list[str]:
    """过滤已经过期的到期日，避免页面停留在无法刷新的历史合约上。"""
    today = market_today()
    return [value for value in values if date.fromisoformat(value) >= today]


class SnapshotService:
    def __init__(
        self,
        database: Database,
        provider: MarketDataProvider | None = None,
        heavy_gate: HeavyWorkGate | None = None,
    ):
        self.database = database
        self.provider = provider or MarketDataProvider()
        self.heavy_gate = heavy_gate or get_heavy_gate()
        self._locks: dict[str, threading.Lock] = {}
        self._locks_guard = threading.Lock()
        self._refresh_flight: SingleFlight[tuple[str, str | None, int, bool], dict[str, Any]] = SingleFlight()
        self._owner = uuid4().hex

    @staticmethod
    def _scope(symbol: str, expiration: str | None) -> str:
        """不同到期日分开串行，Gamma 窗口刷新其他期限时不再挡住当前页面。"""
        return f"{symbol}:{expiration or 'nearest'}"

    def _lock_for(self, scope: str) -> threading.Lock:
        with self._locks_guard:
            return self._locks.setdefault(scope, threading.Lock())

    def refresh(
        self,
        symbol: str,
        expiration: str | None = None,
        max_age_seconds: int = 0,
        *,
        expirations_override: list[str] | None = None,
        quote_override: dict[str, Any] | None = None,
        heavy_gate_held: bool = False,
    ) -> dict[str, Any]:
        """抓取一次快照。

        max_age_seconds 大于 0 时先读本地快照：仍在新鲜期内直接返回 skipped 结果，不再请求上游接口，
        避免定时刷新与手动刷新对同一份数据反复打接口。
        """
        normalized = self.provider.normalize_symbol(symbol)
        # 窗口刷新会顺序处理多个到期日；覆盖值只在本次调用内复用，不放进 key，避免缓存键携带大对象。
        # 外层已持有 Gamma 闸门的调用不能和普通刷新共享 SingleFlight；否则普通刷新
        # 可能等待闸门，而 Gamma 又等待它的结果，形成跨任务互相等待。
        flight_key = (normalized, expiration, max_age_seconds, heavy_gate_held)
        return self._refresh_flight.do(
            flight_key,
            lambda: self._refresh_locked(
                normalized,
                expiration,
                max_age_seconds,
                expirations_override=expirations_override,
                quote_override=quote_override,
                heavy_gate_held=heavy_gate_held,
            ),
        )

    def _refresh_locked(
        self,
        normalized: str,
        expiration: str | None,
        max_age_seconds: int,
        *,
        expirations_override: list[str] | None = None,
        quote_override: dict[str, Any] | None = None,
        heavy_gate_held: bool = False,
    ) -> dict[str, Any]:
        """同一到期日刷新串行化；同一请求键由 SingleFlight 共享结果，不再并发返回 502。"""
        scope = self._scope(normalized, expiration)
        lock = self._lock_for(scope)
        lock_acquired = False
        if heavy_gate_held:
            # Gamma 已占住重任务闸门时不能再持闸等待页面刷新持有的期限锁；否则页面会
            # 等闸门、Gamma 等期限锁，形成互相等待。该期限本轮让路即可。
            if not lock.acquire(blocking=False):
                raise UpstreamBusyError(f"{normalized} {expiration or '最近期限'} 刷新正在进行")
            lock_acquired = True
            waited_for_process_lock = False
        else:
            waited_for_process_lock = not lock.acquire(blocking=False)
            if waited_for_process_lock:
                # Gamma 窗口会在一个标的上连续持有多个期限锁。页面刷新遇到这种情况时，
                # 若本地已有链应立即沿用旧快照，不能等待 60 秒把 HTTP 请求线程拖住。
                # 只有明确启用低内存保护时才让首屏给 Gamma 让路。普通模式的闸门
                # 只是并发上限，不能把正常的排队误报成“内存保护”。
                waited_for_busy_gamma = False
                if low_memory_enabled() and self.heavy_gate.busy() and self._has_saved_chain(normalized, expiration):
                    # Gamma 可能正持有当前期限锁。低内存只限制并发数量，不应让首屏
                    # 在资源空闲时立即放弃；短暂等待仍只有一份期权链在内存中。
                    if lock.acquire(timeout=LOW_MEMORY_REFRESH_WAIT_SECONDS):
                        lock_acquired = True
                        waited_for_busy_gamma = True
                    else:
                        stale = self._stale_snapshot(normalized, expiration, "内存保护：后台 Gamma 正在刷新，本次沿用本地快照")
                        if stale is not None:
                            stale["deferred"] = True
                            stale["skipped"] = True
                            return stale
                if low_memory_enabled() and self.heavy_gate.busy() and not waited_for_busy_gamma:
                    # 没有本地链时也不能在期限锁上长时间等待：Gamma 或另一条
                    # 刷新链可能正持有同一把锁，而浏览器会在更短的请求超时后重入，
                    # 形成「旧请求继续等锁 + 新请求继续排队」的刷新风暴。让本轮快速
                    # 失败，前端保留占位/下一轮重试，后台任务完成后即可正常回源。
                    raise UpstreamBusyError(f"{normalized} {expiration or '最近期限'} 刷新正在进行")
                if not waited_for_busy_gamma and not lock.acquire(timeout=REFRESH_LOCK_TIMEOUT_SECONDS):
                    stale = self._stale_snapshot(normalized, expiration, "同一到期日正在刷新，本次沿用本地快照")
                    if stale is not None:
                        stale["deferred"] = True
                        stale["skipped"] = True
                        return stale
                    raise UpstreamBusyError(f"{normalized} 刷新等待超过 {REFRESH_LOCK_TIMEOUT_SECONDS} 秒")
            lock_acquired = True
        lease_name = f"snapshot-refresh:{scope}"
        deadline = time.monotonic() + REFRESH_LOCK_TIMEOUT_SECONDS
        waited_for_database_lease = False
        lease_acquired = False
        try:
            while not self.database.try_acquire_lease(lease_name, self._owner, REFRESH_LEASE_SECONDS):
                waited_for_database_lease = True
                if heavy_gate_held:
                    # Gamma 已持有全局重任务槽位，不能占着它等待另一个 worker 的数据库租约；
                    # 否则页面刷新拿不到闸门，双方会互相拖到超时。
                    raise UpstreamBusyError(f"{normalized} {expiration or '最近期限'} 跨进程刷新正在进行")
                if time.monotonic() >= deadline:
                    stale = self._stale_snapshot(normalized, expiration, "跨进程刷新正在进行，本次沿用本地快照")
                    if stale is not None:
                        stale["deferred"] = True
                        stale["skipped"] = True
                        return stale
                    raise UpstreamBusyError(f"{normalized} 跨进程刷新等待超过 {REFRESH_LOCK_TIMEOUT_SECONDS} 秒")
                time.sleep(0.1)
            lease_acquired = True
            try:
                # 另一个 worker 刚刚完成了同一到期日刷新时，复用其新快照；只有真正的独立强制刷新才继续回源。
                if waited_for_process_lock or waited_for_database_lease:
                    coalesced = self.recent_snapshot(normalized, expiration, REFRESH_COALESCE_SECONDS)
                    if coalesced is not None:
                        coalesced["coalesced"] = True
                        return coalesced
                if max_age_seconds > 0:
                    cached = self.recent_snapshot(normalized, expiration, max_age_seconds)
                    if cached is not None:
                        logger.debug(
                            "跳过刷新 %s %s：本地快照仍有 %.1f 秒新鲜度",
                            normalized,
                            cached.get("expiration") or "(仅现货)",
                            cached["age_seconds"],
                        )
                        return cached
                try:
                    return self._fetch_and_store(
                        normalized,
                        expiration,
                        expirations_override=expirations_override,
                        quote_override=quote_override,
                        heavy_gate_held=heavy_gate_held,
                    )
                except (ProviderError, UpstreamBusyError, RuntimeError) as exc:
                    stale = self._stale_snapshot(normalized, expiration, str(exc))
                    if stale is not None:
                        logger.warning("刷新 %s 失败，沿用本地旧快照：%s", normalized, exc)
                        return stale
                    raise
            finally:
                try:
                    if lease_acquired:
                        self.database.release_lease(lease_name, self._owner)
                finally:
                    lease_acquired = False
        finally:
            if lock_acquired:
                lock.release()

    def _stale_snapshot(self, symbol: str, expiration: str | None, warning: str) -> dict[str, Any] | None:
        """上游拥塞或失败时返回本地旧快照元数据，让调用方继续使用本地链。"""
        cached_quote = self.database.latest_quote(symbol) or {}
        has_cached_price = cached_quote.get("price") is not None and snapshot_age_seconds(cached_quote.get("fetched_at")) is not None
        target = expiration
        if not target:
            values = active_expirations(self.database.latest_expirations(symbol))
            target = values[0] if values else None
        if target and has_cached_price:
            cached = self.database.latest_chain(symbol, target)
            if cached.get("data"):
                return {
                    "symbol": symbol,
                    "expiration": target,
                    "fetched_at": cached.get("fetched_at"),
                    "rows": 0,
                    "skipped": True,
                    "stale": True,
                    "age_seconds": round(snapshot_age_seconds(cached.get("fetched_at")) or 0.0, 1),
                    "warning": warning,
                }
        # 指定到期日没有本地链不代表该标的没有期权；quote-only 会让前端误入
        # 「重新获取到期日」分支，在锁竞争期间再叠一轮慢请求。
        if expiration:
            return None
        quote = cached_quote
        if quote.get("price") is None:
            return None
        return {
            "symbol": symbol,
            "expiration": None,
            "fetched_at": quote.get("fetched_at"),
            "rows": 0,
            "skipped": True,
            "stale": True,
            "age_seconds": round(snapshot_age_seconds(quote.get("fetched_at")) or 0.0, 1),
            "quote_only": True,
            "warning": warning,
        }

    def recent_snapshot(self, symbol: str, expiration: str | None, max_age_seconds: int) -> dict[str, Any] | None:
        """本地快照仍在新鲜期内时返回可复用的结果，否则返回 None。"""
        cached_expirations = active_expirations(self.database.latest_expirations(symbol))
        target = expiration or (cached_expirations[0] if cached_expirations else None)
        if not target:
            # 完全没有缓存的到期日（例如压根不支持期权的标的）：退化为按现货快照判断新鲜度，
            # 否则每次定时刷新都会重新打一次上游接口。
            quote = self.database.latest_quote(symbol) or {}
            age = snapshot_age_seconds(quote.get("fetched_at"))
            if age is None or age >= max_age_seconds:
                return None
            return {
                "symbol": symbol,
                "expiration": None,
                "fetched_at": quote["fetched_at"],
                "rows": 0,
                "skipped": True,
                "quote_only": True,
                "age_seconds": round(age, 1),
            }
        cached = self.database.latest_chain(symbol, target)
        chain_age = snapshot_age_seconds(cached.get("fetched_at"))
        quote = self.database.latest_quote(symbol) or {}
        quote_age = snapshot_age_seconds(quote.get("fetched_at"))
        # 期权链和行情是同一份页面快照的两个必要部分；只缓存到链而没有有效现价时，
        # 不能返回 skipped，否则首屏会一直显示 --，直到用户手动再次刷新。
        if (
            chain_age is None
            or chain_age >= max_age_seconds
            or quote.get("price") is None
            or quote_age is None
            or quote_age >= max_age_seconds
            # 旧版本写入的期权链可能只有成交量/持仓量，没有买卖价；这类缓存不能阻止买方结构自动补抓。
            or not has_valid_two_sided_quotes(cached.get("data"))
        ):
            return None
        return {
            "symbol": symbol,
            "expiration": target,
            "fetched_at": cached["fetched_at"],
            "rows": 0,
            "skipped": True,
            "age_seconds": round(max(chain_age, quote_age), 1),
        }

    def _has_saved_chain(self, symbol: str, expiration: str | None) -> bool:
        """目标到期日已经有链时，忙闸门可以直接沿用，不必再排队抓一份。"""
        target = expiration
        if not target:
            values = active_expirations(self.database.latest_expirations(symbol))
            target = values[0] if values else None
        if not target:
            quote = self.database.latest_quote(symbol) or {}
            return quote.get("price") is not None
        cached = self.database.latest_chain(symbol, target)
        return bool(cached.get("data"))

    def _fetch_and_store(
        self,
        normalized: str,
        expiration: str | None,
        *,
        expirations_override: list[str] | None = None,
        quote_override: dict[str, Any] | None = None,
        heavy_gate_held: bool = False,
    ) -> dict[str, Any]:
        """请求上游接口并写入 SQLite。已有重任务时，有本地链就让路，避免内存叠满。"""
        if heavy_gate_held:
            # Gamma 窗口已经在外层持有闸门；BoundedSemaphore 不可重入，不能再次 acquire。
            return self._fetch_upstream(
                normalized,
                expiration,
                expirations_override=expirations_override,
                quote_override=quote_override,
            )
        if self.heavy_gate.acquire(
            timeout=LOW_MEMORY_REFRESH_WAIT_SECONDS if low_memory_enabled() else REFRESH_LOCK_TIMEOUT_SECONDS
        ):
            try:
                return self._fetch_upstream(
                    normalized,
                    expiration,
                    expirations_override=expirations_override,
                    quote_override=quote_override,
                )
            finally:
                self.heavy_gate.release()
        if low_memory_enabled() and self._has_saved_chain(normalized, expiration):
            stale = self._stale_snapshot(normalized, expiration, "内存保护：已有刷新在进行，本次沿用本地快照")
            if stale is not None and not (expiration and stale.get("quote_only")):
                stale["deferred"] = True
                stale["skipped"] = True
                logger.info("低内存保护：%s %s 刷新让路，沿用本地快照", normalized, expiration or "最近到期日")
                return stale
        # 普通模式已经等待过完整锁竞争窗口；低内存模式对新标的再短暂排队，
        # 防止没有本地链时把多个大对象同时拉入内存。
        wait_seconds = 5 if low_memory_enabled() else REFRESH_LOCK_TIMEOUT_SECONDS
        if not self.heavy_gate.acquire(timeout=wait_seconds):
            message = (
                f"{normalized} 刷新排队超过 {wait_seconds:g} 秒，请稍后重试"
                if low_memory_enabled()
                else f"{normalized} 已有刷新进行中，等待 {wait_seconds:g} 秒后仍未完成"
            )
            raise UpstreamBusyError(message)
        try:
            return self._fetch_upstream(
                normalized,
                expiration,
                expirations_override=expirations_override,
                quote_override=quote_override,
            )
        finally:
            self.heavy_gate.release()

    def _remember_expirations(self, symbol: str, values: list[str]) -> list[str]:
        """记下本次已经拿到的全部未过期到期日，避免为了下拉框再请求一次上游。"""
        available = active_expirations(values)
        if available:
            self.database.write_expiration_catalog(symbol, available, iso())
        return available

    def _fetch_upstream(
        self,
        normalized: str,
        expiration: str | None,
        *,
        expirations_override: list[str] | None = None,
        quote_override: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """请求上游接口并写入 SQLite，失败时记录刷新日志后抛出。"""
        run_id = self.database.start_run(normalized)
        try:
            expirations = expirations_override if expirations_override is not None else self.provider.expirations(normalized)
            # refresh_window 已在进入循环前写过一次到期日目录；逐期刷新时只过滤，
            # 避免每个期限都重复写同一条 SQLite 目录记录。
            available = (
                active_expirations(expirations)
                if expirations_override is not None
                else self._remember_expirations(normalized, expirations)
            )
            # 有些标的（例如 SPCX 这类没有挂牌期权合约的标的）根本没有到期日：此时退化成只抓现货，
            # 现货卡片照常可用，页面其余面板提示没有期权数据，而不是整页无数据。
            if not available:
                quote = quote_override or self.provider.quote(normalized)
                quote = self._with_cached_sessions(normalized, dict(quote))
                fetched_at = iso()
                self.database.write_snapshot(quote, [], fetched_at)
                self.database.finish_run(run_id, "success", 0)
                logger.info("%s 没有可用的期权到期日，本次只记录现货快照", normalized)
                return {"symbol": normalized, "expiration": None, "fetched_at": fetched_at, "rows": 0, "quote_only": True}
            selected = expiration or available[0]
            if selected not in expirations:
                raise ValueError(f"{normalized} 没有到期日 {selected}（最新可用: {available[0]}）")
            if selected not in available:
                raise ValueError(f"{normalized} 的到期日 {selected} 已过期，最新可用到期日: {available[0]}")
            if quote_override is None:
                quote, rows, fetched_at = self.provider.fetch(normalized, selected)
            else:
                # 具体行情适配器支持复用现货快照；测试/第三方适配器仍兼容原有两参数接口。
                try:
                    quote, rows, fetched_at = self.provider.fetch(
                        normalized,
                        selected,
                        quote_override=quote_override,
                    )
                except TypeError as exc:
                    if "quote_override" not in str(exc):
                        raise
                    quote, rows, fetched_at = self.provider.fetch(normalized, selected)
            written = self.database.write_snapshot(self._with_cached_sessions(normalized, quote), rows, fetched_at)
            self.database.finish_run(run_id, "success", written)
            return {
                "symbol": normalized,
                "expiration": selected,
                "fetched_at": fetched_at,
                "rows": written,
                "expirations": available,
            }
        except Exception as exc:
            self.database.finish_run(run_id, "failed", 0, str(exc))
            if isinstance(exc, (ValueError, ProviderError, RuntimeError)):
                raise
            raise ProviderError(str(exc)) from exc

    def _with_cached_sessions(self, normalized: str, quote: dict[str, Any]) -> dict[str, Any]:
        """盘前盘后抓取失败（例如被数据源限流）时沿用上一次快照的时段数据，避免卡片闪空。"""
        if not quote.get("sessions"):
            cached_quote = self.database.latest_quote(normalized) or {}
            quote["sessions"] = parse_sessions(cached_quote.get("sessions_json"))
        return quote

    def refresh_default(self, symbols: tuple[str, ...]) -> list[dict[str, Any]]:
        results = []
        for symbol in symbols:
            try:
                results.append(self.refresh(symbol))
            except Exception as exc:
                logger.warning("定时刷新 %s 失败: %s", symbol, exc)
                continue
        return results

    def refresh_window(
        self,
        symbol: str,
        horizon_days: int = 45,
        *,
        batch_size: int = 1,
        start_after: str | None = None,
    ) -> dict[str, Any]:
        """分批刷新近期期限，供跨到期日 Gamma 曲线使用。

        每次最多抓取一个小批次的到期日。批次之间释放重任务闸门，页面可以继续
        读取当前期限；调用方用 ``next_cursor`` 领取下一批，避免 45 天窗口一次性
        占满内存和上游连接。
        """
        normalized = self.provider.normalize_symbol(symbol)
        batch_size = max(1, min(int(batch_size), 2))
        # Gamma 是低优先级的跨期限任务。它只在每个期限的抓取期间占用闸门，
        # 不能把整个窗口几十秒锁住，否则 /api/levels 的历史与 Beta 永远只能拿到空回退。
        # 先用一次短闸门保护到期日目录和现货读取；忙时直接让首屏任务优先。
        if not self.heavy_gate.acquire(timeout=0):
            message = (
                "内存保护：已有重任务在进行，Gamma 窗口已让路"
                if low_memory_enabled()
                else "已有重任务在进行，Gamma 窗口稍后计算"
            )
            logger.info("%s %s", normalized, message)
            return {
                "symbol": normalized,
                "horizon_days": horizon_days,
                "expirations": [],
                "results": [],
                "errors": [message],
                "deferred": True,
                "warning": message,
            }
        try:
            expirations = self.database.latest_expiration_catalog(normalized)
            if not expirations:
                expirations = self.provider.expirations(normalized)
                # 窗口刷新本来就要问一次到期日，顺手更新下拉框缓存；这里存的是全部日期，不是 45 天切片。
                self._remember_expirations(normalized, expirations)
            today = market_today()
            cutoff = today + timedelta(days=horizon_days)
            selected = sorted([
                value for value in expirations
                if today <= date.fromisoformat(value) <= cutoff
            ])
            # 窗口内每个期限共用一份现货快照；只有确实需要抓新链时才读取行情。
            # 所有期限都新鲜时不再额外触发 quote 上游请求。
            window_quote: dict[str, Any] | None = None
            quote_loaded = False
            results: list[dict[str, Any]] = []
            errors: list[str] = []
        finally:
            # 到期日目录已经读完，马上把闸门交还给首屏历史任务。
            self.heavy_gate.release()

        candidates = [value for value in selected if not start_after or value > start_after]
        processed: list[str] = []
        batch: list[str] = []
        retry_cursor: str | None = None
        retry_pending = False
        for expiration in candidates:
            # 先检查缓存。新鲜期限不需要占用重任务槽位，避免 Gamma 在一串缓存期限上
            # 快速循环，把真正需要历史回源的首屏任务饿住。
            cached = self.database.latest_chain(normalized, expiration)
            age = snapshot_age_seconds(cached.get("fetched_at"))
            has_open_interest = any(
                float(row.get("open_interest") or 0) > 0
                for row in cached.get("data") or []
                if isinstance(row, dict)
            )
            if age is not None and age < WINDOW_FRESH_SECONDS and has_open_interest:
                processed.append(expiration)
                continue
            if len(batch) >= batch_size:
                break
            batch.append(expiration)

        for expiration in batch:
            # 每个期限独立占槽并在请求后释放。这样历史/Beta 可以在期限之间插队，
            # 也避免某个慢期限把整个 Gamma 窗口拖到超时。
            if not self.heavy_gate.acquire(timeout=0):
                reason = "内存保护：让路给首屏历史数据" if low_memory_enabled() else "让路给首屏历史数据"
                errors.append(f"{expiration}: {reason}")
                retry_cursor = start_after
                retry_pending = True
                continue
            try:
                try:
                    if not quote_loaded:
                        quote_loaded = True
                        # Gamma 只需要现价估算模型 Greeks；优先复用刚写入 SQLite 的报价，
                        # 避免窗口刷新再次抓取 5 天 1 分钟扩展时段历史。
                        cached_quote = self.database.latest_quote(normalized)
                        if cached_quote and cached_quote.get("price") is not None:
                            window_quote = cached_quote
                        else:
                            try:
                                window_quote = self.provider.quote(normalized)
                            except ProviderError as exc:
                                logger.warning("获取 %s Gamma 窗口现货失败，各期限按适配器自行回退：%s", normalized, exc)
                    # 一个 Gamma 窗口内复用一次到期日列表和行情快照，避免每期重复请求。
                    # 外层已经持有重任务闸门，内部绕过 acquire，避免自等待。
                    results.append(
                        self.refresh(
                            normalized,
                            expiration,
                            expirations_override=expirations,
                            quote_override=window_quote,
                            heavy_gate_held=True,
                        )
                    )
                    processed.append(expiration)
                except Exception as exc:
                    errors.append(f"{expiration}: {exc}")
                    # 失败期限也推进游标，避免供应商持续拒绝时反复重试同一批并卡住窗口。
                    processed.append(expiration)
                    logger.warning("刷新 %s %s 失败: %s", normalized, expiration, exc)
            finally:
                self.heavy_gate.release()
        return {
            "symbol": normalized,
            "horizon_days": horizon_days,
            "expirations": selected,
            "results": results,
            "errors": errors,
            "batch_size": batch_size,
            "batch_expirations": batch,
            "next_cursor": retry_cursor if retry_pending else (processed[-1] if processed and processed[-1] != selected[-1] else None),
            "has_more": retry_pending or bool(processed and processed[-1] != selected[-1]),
            "loaded": len([value for value in selected if value <= (processed[-1] if processed else start_after or "")]),
            "total": len(selected),
        }
